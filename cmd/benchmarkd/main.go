// Command benchmarkd serves the agent benchmark tool's read-only view over the
// autonomy reason_turns log.
//
// The log is read over autonomy's data API (docs/autonomy-api.md); the tool
// never opens a database file, so the log's location and schema stay autonomy's
// business and a moved/renamed store cannot leave this service showing stale
// data.
//
// Local usage:
//
//	go run ./cmd/benchmarkd
//	go run ./cmd/benchmarkd -autonomy-url http://127.0.0.1:4300 -addr 127.0.0.1:4231
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kaulie/agent-benchmark-tool/internal/autonomyapi"
	"github.com/kaulie/agent-benchmark-tool/internal/httpapi"
)

// defaultBaseURL is the autonomy data API this tool reads by default.
func defaultBaseURL() string { return resolveBaseURL(os.Getenv) }

// resolveBaseURL picks the autonomy address from getenv: AUTONOMY_API_URL when
// set, otherwise the service contract's port on loopback.
func resolveBaseURL(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("AUTONOMY_API_URL")); v != "" {
		return v
	}
	return autonomyapi.DefaultBaseURL
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
	baseURL := flag.String("autonomy-url", defaultBaseURL(), "autonomy data API base URL (the reason_turns source)")
	addr := flag.String("addr", defaultAddr(), "listen address, e.g. 127.0.0.1:4231")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("benchmarkd ")

	if err := run(*baseURL, *addr); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(baseURL, addr string) error {
	source, err := autonomyapi.New(baseURL)
	if err != nil {
		return err
	}

	api, err := httpapi.New(source)
	if err != nil {
		return err
	}

	// Probe the upstream once so the log states whether the data source answers.
	// A failing probe is not fatal: the service still starts and serves /health
	// (503, with the reason) and pages that explain the outage, so a restarting
	// autonomy does not turn into a failed deployment of this tool.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	upstream, probeErr := source.Probe(ctx)
	cancel()

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
	log.Printf("upstream: %s", upstream.URL)
	if probeErr != nil {
		log.Printf("upstream unreachable: %v (serving anyway; /health reports 503)", probeErr)
	} else {
		log.Printf("upstream ok: version=%s reason_turns=%d", upstream.Version, upstream.Turns)
	}
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
