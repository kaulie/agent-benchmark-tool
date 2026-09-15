// Command benchmarkd serves the agent benchmark tool's read-only view over an
// autonomy reason_turns log.
//
// Local usage:
//
//	go run ./cmd/benchmarkd
//	go run ./cmd/benchmarkd -db /path/to/autonomy.db -addr 127.0.0.1:4231
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

// defaultAddr is the listen address. It deliberately ignores the ambient
// HOST/PORT variables (a shared shell may set PORT for a different service);
// use BENCHMARK_ADDR or -addr to override.
func defaultAddr() string {
	if v := os.Getenv("BENCHMARK_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:4231"
}

func main() {
	dbPath := flag.String("db", defaultDBPath(), "path to the autonomy SQLite database (read-only)")
	addr := flag.String("addr", defaultAddr(), "listen address, e.g. 127.0.0.1:4231")
	flag.Parse()

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
	log.Printf("db (read-only): %s", db.Path())
	log.Printf("reason_turns: %d rows", total)
	log.Printf("listening on http://%s/  (json: /api/reason-turns, health: /healthz)", ln.Addr())

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
