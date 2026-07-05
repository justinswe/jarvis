package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

type readyState bool

func (r readyState) Ready() bool { return bool(r) }

func TestHealthEndpoints(t *testing.T) {
	for _, test := range []struct {
		name  string
		path  string
		ready readyState
		want  int
	}{
		{"health", "/healthz", false, http.StatusOK},
		{"not ready", "/readyz", false, http.StatusServiceUnavailable},
		{"ready", "/readyz", true, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			newHTTPHandler(test.ready).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			assert.Equal(t, test.want, recorder.Code)
		})
	}
}
