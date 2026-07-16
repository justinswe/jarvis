package worker

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeProcessor struct {
	message *discordgo.MessageCreate
	err     error
	process func(context.Context, *discordgo.MessageCreate) error
}

type fakeRecorder struct {
	event  *discordv1.DiscordMessageCreateEvent
	err    error
	called bool
}

func (r *fakeRecorder) Record(_ context.Context, event *discordv1.DiscordMessageCreateEvent) error {
	r.called = true
	r.event = event
	return r.err
}

func (p *fakeProcessor) Process(ctx context.Context, message *discordgo.MessageCreate) error {
	p.message = message
	if p.process != nil {
		return p.process(ctx, message)
	}
	return p.err
}

func validRequest() *discordv1.IngestMessageRequest {
	return &discordv1.IngestMessageRequest{Event: &discordv1.DiscordMessageCreateEvent{
		MessageId:        "message",
		GuildId:          "guild",
		ChannelId:        "channel",
		Content:          "hello",
		Kind:             discordv1.MessageKind_MESSAGE_KIND_REPLY,
		Author:           &discordv1.MessageAuthor{Id: "user", Username: "alice"},
		MentionedUserIds: []string{"bot"},
		Reference:        &discordv1.MessageReference{MessageId: "parent", ChannelId: "channel"},
	}}
}

func TestHTTPWorkerProcessesRawProtobuf(t *testing.T) {
	processor := &fakeProcessor{}
	recorder := serveRequest(t, NewHandler(processor), http.MethodPost, processPath, "application/x-protobuf", validRequest())
	assert.Equal(t, http.StatusNoContent, recorder.Code)
	require.NotNil(t, processor.message)
	assert.Equal(t, "message", processor.message.ID)
	assert.Equal(t, discordgo.MessageTypeReply, processor.message.Type)
	assert.Equal(t, "parent", processor.message.MessageReference.MessageID)
	assert.Equal(t, "bot", processor.message.Mentions[0].ID)
}

func TestHTTPWorkerRecordsBeforeProcessing(t *testing.T) {
	recorder := &fakeRecorder{}
	processor := &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		assert.True(t, recorder.called)
		return nil
	}}
	response := serveRequest(t, NewHandler(processor, recorder), http.MethodPost, processPath, "application/x-protobuf", validRequest())
	assert.Equal(t, http.StatusNoContent, response.Code)
	require.NotNil(t, recorder.event)
	assert.Equal(t, "message", recorder.event.MessageId)
}

func TestHTTPWorkerFailsOpenWhenRecordingFails(t *testing.T) {
	recorder := &fakeRecorder{err: errors.New("DynamoDB unavailable")}
	processor := &fakeProcessor{}
	response := serveRequest(t, NewHandler(processor, recorder), http.MethodPost, processPath, "application/x-protobuf", validRequest())
	assert.Equal(t, http.StatusNoContent, response.Code)
	require.NotNil(t, processor.message)
}

func TestHTTPWorkerAcceptsPubSubUnwrappedContentTypes(t *testing.T) {
	for _, contentType := range []string{"", "application/octet-stream"} {
		recorder := serveRequest(t, NewHandler(&fakeProcessor{}), http.MethodPost, processPath, contentType, validRequest())
		assert.Equal(t, http.StatusNoContent, recorder.Code)
	}
}

func TestHTTPWorkerRejectsInvalidRequests(t *testing.T) {
	handler := NewHandler(&fakeProcessor{})
	tests := []struct {
		name, method, contentType string
		body                      []byte
		want                      int
	}{
		{name: "method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "content type", method: http.MethodPost, contentType: "application/json", want: http.StatusUnsupportedMediaType},
		{name: "protobuf", method: http.MethodPost, contentType: "application/x-protobuf", body: []byte("invalid"), want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, processPath, bytes.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assert.Equal(t, test.want, recorder.Code)
		})
	}
	semantic := serveRequest(t, handler, http.MethodPost, processPath, "application/x-protobuf", &discordv1.IngestMessageRequest{})
	assert.Equal(t, http.StatusBadRequest, semantic.Code)
}

func TestHTTPWorkerRejectsOversizedBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, processPath, bytes.NewReader(make([]byte, maxRequestSize+1)))
	request.Header.Set("Content-Type", "application/x-protobuf")
	recorder := httptest.NewRecorder()
	NewHandler(&fakeProcessor{}).ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
}

func TestHTTPWorkerReturnsFailureAfterProcessingError(t *testing.T) {
	processor := &fakeProcessor{err: errors.New("failed")}
	recorder := serveRequest(t, NewHandler(processor), http.MethodPost, processPath, "application/x-protobuf", validRequest())
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.NotNil(t, processor.message)
}

func TestHTTPWorkerPropagatesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var processErr error
	processor := &fakeProcessor{process: func(got context.Context, _ *discordgo.MessageCreate) error {
		processErr = got.Err()
		return processErr
	}}
	body, err := proto.Marshal(validRequest())
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, processPath, bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/x-protobuf")
	recorder := httptest.NewRecorder()
	NewHandler(processor).ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.ErrorIs(t, processErr, context.Canceled)
}

func TestHTTPWorkerServerTimeouts(t *testing.T) {
	server := newHTTPServer(":8080", &fakeProcessor{})
	assert.Equal(t, requestHeaderTimeout, server.ReadHeaderTimeout)
	assert.Equal(t, requestReadTimeout, server.ReadTimeout)
	assert.Equal(t, idleConnectionTimeout, server.IdleTimeout)
	assert.Zero(t, server.WriteTimeout)
}

func TestHTTPWorkerHealthEndpoints(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		NewHandler(&fakeProcessor{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, recorder.Code)
		recorder = httptest.NewRecorder()
		NewHandler(&fakeProcessor{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		assert.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
	}
}

func serveRequest(t *testing.T, handler http.Handler, method, path, contentType string, message proto.Message) *httptest.ResponseRecorder {
	t.Helper()
	body, err := proto.Marshal(message)
	require.NoError(t, err)
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
