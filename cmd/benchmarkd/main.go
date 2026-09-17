// Command benchmarkd serves the agent benchmark tool's read-only view over an
// autonomy reason_turns log.
//
// Local usage:
//
//	go run ./cmd/benchmarkd
//	go run ./cmd/benchmarkd -db /path/to/autonomy.db -addr 127.0.0.1:4231
//	SERVICE_PORT=8080 go run ./cmd/benchmarkd
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kaulie/agent-benchmark-tool/internal/httpapi"
	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// defaultDBPath is the autonomy runtime database this tool reads by default.
func defaultDBPath() string {
	if v := os.Getenv("AUTONOMY_DB"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "data/autonomy.db"
	}
	return filepath.Join(home, "Projects", "autonomy", "data", "autonomy.db")
}

// defaultListenAddr is the built-in listen address (loopback only: the tool
// reads a local log and has no auth).
const defaultListenAddr = "127.0.0.1:4231"

// defaultAddr is the listen address from the environment, or the default.
func defaultAddr() string { return resolveAddr(os.Getenv) }

// resolveAddr picks the listen address from getenv, most specific first:
//
//	BENCHMARK_ADDR  full listen address — this service's own override
//	SERVICE_PORT    the deployment port: a bare port ("8080") or a full
//	                "host:port"; a bare port still stays on loopback
//
// Anything else falls back to defaultListenAddr. The ambient HOST/PORT variables
// are ignored on purpose: a shared shell may set PORT for a different service.
// An unusable SERVICE_PORT is not fatal — the service falls back to the default
// instead of refusing to start. The getenv indirection keeps the precedence
// testable.
func resolveAddr(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("BENCHMARK_ADDR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(getenv("SERVICE_PORT")); v != "" {
		// A full host:port is accepted too, but a missing host stays loopback:
		// ":8080" must not silently expose the log on every interface.
		if host, port, err := net.SplitHostPort(v); err == nil && port != "" {
			if host == "" {
				host = "127.0.0.1"
			}
			return net.JoinHostPort(host, port)
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			return net.JoinHostPort("127.0.0.1", v)
		}
		log.Printf("ignoring SERVICE_PORT=%q: not a port, using %s", v, defaultListenAddr)
	}
	return defaultListenAddr
}

// version is stamped at build time via -ldflags "-X main.version=<hash>".
var version = "dev"

func main() {
	dbPath := flag.String("db", defaultDBPath(), "path to the autonomy SQLite database (read-only)")
	addr := flag.String("addr", defaultAddr(), "listen address, e.g. 127.0.0.1:4231")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("benchmarkd ")

	if err := run(*dbPath, *addr); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(dbPath, addr string) error {
	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	api, err := httpapi.New(db)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	total, err := db.CountAll(ctx)
	cancel()
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	log.Printf("benchmarkd %s", version)
	log.Printf("db (read-only): %s", db.Path())
	log.Printf("reason_turns: %d rows", total)
	log.Printf("listening on http://%s/  (json: /api/reason-turns, health: /health)", ln.Addr())

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}
