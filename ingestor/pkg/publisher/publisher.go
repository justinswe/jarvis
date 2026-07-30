// Package publisher delivers normalized Discord events to the worker's JetStream subject.
package publisher

import (
	"context"
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Stream stores one message and reports the broker's acknowledgement.
type Stream interface {
	Publish(context.Context, string, []byte, ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

// Publisher hands normalized Discord events to JetStream. It returns as soon as
// the broker has durably stored a message and never waits for the worker to
// process it; delivery and retries are the consumer's concern.
type Publisher struct {
	stream  Stream
	subject string
	timeout time.Duration
}

// New creates a publisher that stores events on subject.
func New(stream Stream, subject string, timeout time.Duration) (*Publisher, error) {
	if stream == nil {
		return nil, errors.New("JetStream context is required")
	}
	if subject == "" {
		return nil, errors.New("NATS subject is required")
	}
	if timeout <= 0 {
		return nil, errors.New("publish timeout must be positive")
	}
	return &Publisher{stream: stream, subject: subject, timeout: timeout}, nil
}

// Publish stores one message in the stream and returns without awaiting processing.
func (p *Publisher) Publish(parent context.Context, message *discordv1.IngestMessageRequest) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return errors.Wrap(err, "marshal worker request")
	}
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	if _, err := p.stream.Publish(ctx, p.subject, body, p.options(message)...); err != nil {
		return errors.Wrap(err, "publish message to JetStream")
	}
	return nil
}

// options returns the publish options for one message, deduplicating on the Discord
// message ID so a retried publish stores the message once rather than queueing the
// same reply twice.
//
// A message with no ID is published without deduplication: sharing one dedup key
// across every such message would silently discard all but the first.
func (p *Publisher) options(message *discordv1.IngestMessageRequest) []jetstream.PublishOpt {
	id := messageID(message)
	if id == "" {
		return nil
	}
	return []jetstream.PublishOpt{jetstream.WithMsgID(id)}
}

// messageID returns the Discord message ID carried by a request, if it has one.
func messageID(message *discordv1.IngestMessageRequest) string {
	if message == nil || message.Event == nil {
		return ""
	}
	return message.Event.MessageId
}
