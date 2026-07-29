package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

type readyState bool

func (r readyState) Ready() bool { return bool(r) }

type blockingGateway struct{}

func (blockingGateway) Ready() bool { return true }
func (blockingGateway) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
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
		NewHandler(test.ready).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		assert.Equal(t, test.want, recorder.Code)
	}
}

func TestServeStopsWhenParentIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.NoError(t, Serve(ctx, "0", blockingGateway{}))
}
