package mq

import (
	"context"

	"github.com/justinswe/std/errors"
)

// OpenPublisher connects to the configured broker and returns a publisher.
func OpenPublisher(ctx context.Context, cfg Config) (Publisher, error) {
	if err := cfg.validatePublisher(); err != nil {
		return nil, err
	}
	switch cfg.Driver {
	case DriverPubSub:
		return openPubSubPublisher(ctx, cfg)
	default:
		return openNATSPublisher(ctx, cfg)
	}
}

// Subscribe delivers messages to handler until the returned subscription is stopped.
func Subscribe(ctx context.Context, cfg Config, handler Handler) (Subscription, error) {
	if err := cfg.validateSubscriber(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, errors.New("message handler is required")
	}
	switch cfg.Driver {
	case DriverPubSub:
		return subscribePubSub(ctx, cfg, handler)
	default:
		return subscribeNATS(ctx, cfg, handler)
	}
}
