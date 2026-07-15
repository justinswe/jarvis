package ingestor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
	"google.golang.org/protobuf/proto"
)

const (
	maxResponseDrainSize = 4 << 10
	protobufContentType  = "application/x-protobuf"
)

// MessagePublisher delivers normalized Discord events to a message processor.
type MessagePublisher interface {
	Publish(context.Context, *discordv1.IngestMessageRequest) error
}

// HTTPPublisher synchronously delivers raw protobuf messages over HTTP.
type HTTPPublisher struct {
	endpoint string
	client   *http.Client
}

// NewHTTPPublisher creates a publisher for a Pub/Sub-compatible push endpoint.
func NewHTTPPublisher(endpoint string, client *http.Client) (*HTTPPublisher, error) {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil {
		return nil, errors.Wrap(err, "parse worker URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("worker URL must be an absolute HTTP or HTTPS URL")
	}
	if client == nil {
		return nil, errors.New("HTTP client is required")
	}
	return &HTTPPublisher{endpoint: endpoint, client: client}, nil
}

// Publish sends one message and waits for the worker to finish processing it.
func (p *HTTPPublisher) Publish(ctx context.Context, message *discordv1.IngestMessageRequest) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return errors.Wrap(err, "marshal worker request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.Wrap(err, "create worker request")
	}
	request.Header.Set("Content-Type", protobufContentType)
	response, err := p.client.Do(request)
	if err != nil {
		return errors.Wrap(err, "send worker request")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseDrainSize))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.Errorf("worker returned HTTP status %s", response.Status)
	}
	return nil
}
