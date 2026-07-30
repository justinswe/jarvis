// Package server exposes the worker's health and readiness endpoints.
//
// The worker takes its work from JetStream rather than from HTTP; this server
// exists so the container platform and the combined-image supervisor can tell
// when the worker has started consuming.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/justinswe/std/errors"
)

const (
	// shutdownTimeout bounds the graceful stop of the health server.
	shutdownTimeout = 5 * time.Second
	// requestHeaderTimeout bounds how long a client may take to send headers.
	requestHeaderTimeout = 5 * time.Second
	// writeTimeout bounds how long a response may take to write. These endpoints answer
	// with an empty body, so anything approaching this is a stuck connection.
	writeTimeout = 10 * time.Second
	// idleTimeout bounds how long a keep-alive connection may sit unused, so probe
	// clients that never close cannot accumulate connections for the process lifetime.
	idleTimeout = 60 * time.Second
)

// Serve runs the health server until ctx ends or the server fails.
func Serve(ctx context.Context, address string) error {
	if address == "" {
		return errors.New("worker address is required")
	}
	server := &http.Server{
		Addr:              address,
		Handler:           NewHandler(),
		ReadHeaderTimeout: requestHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	done := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return errors.Wrap(err, "shut down worker health server")
		}
		return <-done
	}
}

// NewHandler returns the health and readiness mux. The worker only serves these
// once its JetStream subscription is running, so readiness needs no extra state.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	for _, path := range []string{"/healthz", "/readyz"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	}
	return mux
}
