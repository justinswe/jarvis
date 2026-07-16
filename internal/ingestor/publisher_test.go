package ingestor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type countingBody struct {
	reader *bytes.Reader
	read   int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (*countingBody) Close() error { return nil }

func TestHTTPPublisherSendsRawProtobuf(t *testing.T) {
	want := &discordv1.IngestMessageRequest{Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message"}}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/messages:process", r.URL.Path)
		assert.Equal(t, protobufContentType, r.Header.Get("Content-Type"))
		body := &discordv1.IngestMessageRequest{}
		require.NoError(t, proto.Unmarshal(readBody(t, r), body))
		assert.True(t, proto.Equal(want, body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	publisher, err := NewHTTPPublisher(server.URL+"/v1/messages:process", server.Client())
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(context.Background(), want))
	assert.Equal(t, 1, requests)
}

func TestHTTPPublisherRejectsFailureStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "failed", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	publisher, err := NewHTTPPublisher(server.URL, server.Client())
	require.NoError(t, err)
	err = publisher.Publish(context.Background(), &discordv1.IngestMessageRequest{})
	assert.ErrorContains(t, err, "503 Service Unavailable")
}

func TestHTTPPublisherBoundsResponseDrain(t *testing.T) {
	body := &countingBody{reader: bytes.NewReader(make([]byte, maxResponseDrainSize*2))}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{Status: "503 Service Unavailable", StatusCode: http.StatusServiceUnavailable, Body: body}, nil
	})}
	publisher, err := NewHTTPPublisher("http://worker/v1/messages:process", client)
	require.NoError(t, err)
	err = publisher.Publish(context.Background(), &discordv1.IngestMessageRequest{})
	assert.ErrorContains(t, err, "503 Service Unavailable")
	assert.Equal(t, maxResponseDrainSize, body.read)
}

func TestHTTPPublisherPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	publisher, err := NewHTTPPublisher("http://worker/v1/messages:process", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})})
	require.NoError(t, err)
	err = publisher.Publish(ctx, &discordv1.IngestMessageRequest{})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestNewHTTPPublisherValidatesConfiguration(t *testing.T) {
	_, err := NewHTTPPublisher("worker:8081", http.DefaultClient)
	assert.Error(t, err)
	_, err = NewHTTPPublisher("http://worker:8081", nil)
	assert.Error(t, err)
}

func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return body
}
