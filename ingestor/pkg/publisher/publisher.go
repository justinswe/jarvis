// Package publisher delivers normalized Discord events to the worker.
package publisher

import (
	"context"
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/mq"
	"github.com/justinswe/std/errors"
	"google.golang.org/protobuf/proto"
)

// Publisher hands normalized Discord events to the message queue. It returns as soon as
// the broker has stored a message and never waits for the worker to process it; delivery
// and retries are the consumer's concern.
type Publisher struct {
	queue   mq.Publisher
	timeout time.Duration
}

// New creates a publisher that stores events on queue.
func New(queue mq.Publisher, timeout time.Duration) (*Publisher, error) {
	if queue == nil {
		return nil, errors.New("message queue publisher is required")
	}
	if timeout <= 0 {
		return nil, errors.New("publish timeout must be positive")
	}
	return &Publisher{queue: queue, timeout: timeout}, nil
}

// Publish stores one message and returns without awaiting processing.
func (p *Publisher) Publish(parent context.Context, message *discordv1.IngestMessageRequest) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return errors.Wrap(err, "marshal worker request")
	}
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	return p.queue.Publish(ctx, body, messageID(message))
}

// messageID returns the Discord message ID a request is deduplicated on, if it has one.
//
// A message with no ID deduplicates on nothing: sharing one key across every such message
// would silently discard all but the first.
func messageID(message *discordv1.IngestMessageRequest) string {
	if message == nil || message.Event == nil {
		return ""
	}
	return message.Event.MessageId
}
