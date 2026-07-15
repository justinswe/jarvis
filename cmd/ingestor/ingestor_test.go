package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type readyState bool

func (r readyState) Ready() bool { return bool(r) }

type blockingGateway struct{}

func (blockingGateway) Ready() bool { return true }
func (blockingGateway) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func TestIngestorConfigDefaults(t *testing.T) {
	command := newRootCommand()
	port, err := command.Flags().GetString("port")
	require.NoError(t, err)
	workerURL, err := command.Flags().GetString("worker-url")
	require.NoError(t, err)
	timeout, err := command.Flags().GetDuration("worker-request-timeout")
	require.NoError(t, err)
	assert.Equal(t, "8080", port)
	assert.Empty(t, workerURL)
	assert.Equal(t, 75*time.Second, timeout)
}

func TestHealthEndpoints(t *testing.T) {
	for _, test := range []struct {
		path  string
		ready readyState
		want  int
	}{
		{"/healthz", false, http.StatusOK},
		{"/readyz", false, http.StatusServiceUnavailable},
		{"/readyz", true, http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		newHTTPHandler(test.ready).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		assert.Equal(t, test.want, recorder.Code)
	}
}

func TestServeStopsWhenParentIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.NoError(t, serve(ctx, "0", blockingGateway{}))
}
