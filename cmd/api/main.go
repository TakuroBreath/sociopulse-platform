// Package main is the entrypoint for cmd/api — the СоциоПульс monolith HTTP/WS server.
//
// Subsequent plans add: config loading, observability (zap, OTel, Prometheus),
// gateway middleware, module registration, WS hub, gRPC for sidecars.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultAddr            = ":8080"
	defaultReadTimeout     = 10 * time.Second
	defaultWriteTimeout    = 30 * time.Second
	defaultShutdownTimeout = 15 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("cmd/api: %v", err)
	}
}

func run() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("cmd/api: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("cmd/api: shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Printf("cmd/api: clean shutdown")
	return nil
}

// healthzHandler reports liveness. Always 200 OK with body "ok\n" once the process
// is past startup. Liveness only — readiness check (DB, NATS) lives at /readyz
// (added in Plan 02 alongside the real config and observability stack).
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}
