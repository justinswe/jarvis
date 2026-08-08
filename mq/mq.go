// Package mq carries normalized Discord events between the ingestor and the worker.
//
// Two brokers are supported and the choice is an operator's, not the application's.
// NATS JetStream runs the combined image, local development, and any single-site
// deployment that must keep working without the internet. GCP Pub/Sub is the shared bus
// for a multi-site deployment: its topics are global, so there is no regional bus to fail
// over between and a queue survives the loss of a whole site.
//
// The two brokers do not offer the same primitives. Rather than expose the union and
// leave callers guessing which half works, this package exposes only what both can honor,
// and each driver absorbs its own broker's quirks:
//
//	mq                 NATS JetStream                 GCP Pub/Sub
//	-----------------  -----------------------------  ------------------------------
//	Publish(dedupeID)  WithMsgID; the broker drops a  an attribute only; Pub/Sub has
//	                   duplicate inside the stream's  no publisher deduplication, so
//	                   duplicate window               the reply claim is the only one
//	Ack                Ack                            Ack
//	Nak                NakWithDelay(AckWait)          Nack, then the subscription's
//	                                                  retryPolicy backoff
//	Term               Term                           Ack, plus a warning; Pub/Sub has
//	                                                  no terminate
//	keepalive          InProgress every AckWait/2     the client extends the deadline
//	MaxInFlight        MaxAckPending                  MaxOutstandingMessages
//	MaxDeliver         consumer MaxDeliver            deadLetterPolicy
//	provisioning       created and kept authoritative verified only; see openPubSub
//
// The one difference callers must know about is deduplication: on Pub/Sub, Publish's
// dedupeID is advisory. Anything that must happen once needs its own guard.
package mq

import (
	"context"
	"time"

	"github.com/justinswe/std/errors"
)

// Driver names a supported broker.
type Driver string

const (
	// DriverNATS is an embedded or external NATS JetStream cluster.
	DriverNATS Driver = "nats"
	// DriverPubSub is GCP Pub/Sub.
	DriverPubSub Driver = "pubsub"
)

// Valid reports whether the driver names a supported broker.
func (d Driver) Valid() bool { return d == DriverNATS || d == DriverPubSub }

// Message is one delivered message and its acknowledgement controls.
//
// Exactly one of Ack, Nak, or Term must be called, and calling one releases whatever the
// driver was doing to hold the message. Doing nothing holds the message until the broker
// takes it back, which is what happens when a worker dies mid-message.
type Message interface {
	// Data returns the raw message body.
	Data() []byte
	// Ack reports the message as processed. It is never redelivered.
	Ack() error
	// Nak reports the message as failed. The broker redelivers it after its retry delay,
	// up to MaxDeliver attempts.
	Nak() error
	// Term drops a message no redelivery could fix, such as an unparseable payload.
	Term() error
}

// Handler processes one delivered message.
//
// Handlers run concurrently, up to MaxInFlight at once, and must not block past
// MaxProcessingTime. The context is the subscription's lifetime, which Stop cancels —
// never the caller's, so shutdown drains rather than aborting in-flight work.
type Handler func(context.Context, Message)

// Publisher stores messages for delivery.
type Publisher interface {
	// Publish stores one message and returns once the broker has it. dedupeID may be
	// empty; where the broker supports it, republishing the same ID stores one message.
	Publish(ctx context.Context, body []byte, dedupeID string) error
	// Close releases the connection, flushing anything still pending.
	Close()
}

// Subscription is a running delivery loop.
type Subscription interface {
	// Stop ends delivery and gives messages already in hand DrainTimeout to finish.
	Stop()
}

// Config selects a broker and the delivery policy applied to it.
type Config struct {
	// Driver selects the broker.
	Driver Driver
	// ClientName identifies this process to the broker in its connection logs.
	ClientName string

	// Topic is where messages are published: a NATS subject, or a Pub/Sub topic.
	Topic string
	// Consumer names the durable subscription every worker replica shares, which is what
	// load-balances one stream across them: a NATS durable consumer, or a Pub/Sub
	// subscription. Publishers leave it empty.
	Consumer string

	// URL and Stream configure DriverNATS. Stream is the JetStream stream bound to Topic.
	URL, Stream string

	// ProjectID configures DriverPubSub.
	ProjectID string

	// AckWait bounds how long a held message may go without progress before the broker
	// redelivers it. It is also how long a failed message waits before its retry: without
	// a delay a transient failure burns every attempt in milliseconds and the retries buy
	// nothing.
	AckWait time.Duration
	// MaxProcessingTime bounds how long one message may be held in total, however much
	// progress it reports. It exists because a wedged handler would otherwise hold a
	// message for the process lifetime. Set it above the longest processing deadline any
	// caller can configure.
	MaxProcessingTime time.Duration
	// DrainTimeout bounds how long Stop lets in-flight messages finish before cancelling
	// them. It must fit inside the platform's shutdown grace period.
	DrainTimeout time.Duration
	// MaxDeliver caps delivery attempts per message.
	MaxDeliver int
	// MaxInFlight caps how many messages one subscriber holds un-acknowledged at once.
	MaxInFlight int
}

// validatePublisher reports whether the configuration can publish.
func (c Config) validatePublisher() error {
	if !c.Driver.Valid() {
		return errors.Errorf("unsupported message queue driver %q", c.Driver)
	}
	if c.Topic == "" {
		return errors.New("topic is required")
	}
	return c.validateDriver()
}

// validateSubscriber reports whether the configuration can consume.
func (c Config) validateSubscriber() error {
	if err := c.validatePublisher(); err != nil {
		return err
	}
	if c.Consumer == "" {
		return errors.New("consumer name is required")
	}
	for _, required := range []struct {
		name  string
		value time.Duration
	}{
		{"ack wait", c.AckWait},
		{"max processing time", c.MaxProcessingTime},
		{"drain timeout", c.DrainTimeout},
	} {
		if required.value <= 0 {
			return errors.Errorf("%s must be positive", required.name)
		}
	}
	if c.MaxDeliver <= 0 {
		return errors.New("max deliver must be positive")
	}
	if c.MaxInFlight <= 0 {
		return errors.New("max in flight must be positive")
	}
	return c.validateDeliveryPolicy()
}

// validateDriver reports whether the fields the selected broker needs are present.
func (c Config) validateDriver() error {
	switch c.Driver {
	case DriverNATS:
		if c.URL == "" {
			return errors.New("NATS URL is required")
		}
		if c.Stream == "" {
			return errors.New("NATS stream is required")
		}
	case DriverPubSub:
		if c.ProjectID == "" {
			return errors.New("Pub/Sub project ID is required")
		}
	}
	return nil
}

// Pub/Sub accepts a dead-letter policy only within this range, and rejects a subscription
// that asks for anything outside it. JetStream has no such floor, so a configuration that
// works on one broker can be impossible on the other; validation says so at startup
// rather than at the first failed message.
const (
	minPubSubDeliveryAttempts = 5
	maxPubSubDeliveryAttempts = 100
	minPubSubAckWait          = 10 * time.Second
	maxPubSubAckWait          = 600 * time.Second
)

// validateDeliveryPolicy reports whether the broker can express the configured policy.
func (c Config) validateDeliveryPolicy() error {
	if c.Driver != DriverPubSub {
		return nil
	}
	if c.MaxDeliver < minPubSubDeliveryAttempts || c.MaxDeliver > maxPubSubDeliveryAttempts {
		return errors.Errorf("Pub/Sub requires max deliver between %d and %d, got %d",
			minPubSubDeliveryAttempts, maxPubSubDeliveryAttempts, c.MaxDeliver)
	}
	if c.AckWait < minPubSubAckWait || c.AckWait > maxPubSubAckWait {
		return errors.Errorf("Pub/Sub requires ack wait between %s and %s, got %s",
			minPubSubAckWait, maxPubSubAckWait, c.AckWait)
	}
	return nil
}
