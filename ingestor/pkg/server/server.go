// Package server exposes the ingestor's health and readiness endpoints.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/justinswe/std/errors"
)

// shutdownTimeout bounds the graceful stop of the health server.
const shutdownTimeout = 5 * time.Second

// Gateway is the Discord Gateway connection the health server reports on.
type Gateway interface {
	Start(context.Context) error
	Ready() bool
}

// Serve runs the health server alongside the Gateway until either one stops.
func Serve(parent context.Context, port string, gateway Gateway) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           NewHandler(gateway),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errs := make(chan error, 2)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()
	go func() { errs <- gateway.Start(ctx) }()

	var result error
	completed := 0
	select {
	case <-parent.Done():
	case result = <-errs:
		completed++
		if result != nil {
			result = errors.Wrap(result, "service failed")
		}
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil && result == nil {
		result = err
	}
	for completed < 2 {
		select {
		case err := <-errs:
			completed++
			if err != nil && result == nil {
				result = err
			}
		case <-shutdownCtx.Done():
			if result == nil {
				result = shutdownCtx.Err()
			}
			return result
		}
	}
	return result
}

// NewHandler returns the health and readiness mux.
func NewHandler(ready interface{ Ready() bool }) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
