package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHealthEndpoints(t *testing.T) {
	for _, test := range []struct {
		path, method string
		want         int
	}{
		{"/healthz", http.MethodGet, http.StatusOK},
		{"/readyz", http.MethodGet, http.StatusOK},
		{"/healthz", http.MethodPost, http.StatusMethodNotAllowed},
		{"/readyz", http.MethodPost, http.StatusMethodNotAllowed},
	} {
		recorder := httptest.NewRecorder()
		NewHandler().ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		assert.Equal(t, test.want, recorder.Code, "%s %s", test.method, test.path)
	}
}

func TestServeRequiresAddress(t *testing.T) {
	assert.Error(t, Serve(context.Background(), ""))
}

func TestServeStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.NoError(t, Serve(ctx, "127.0.0.1:0"))
}
