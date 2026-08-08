package mq

import (
	"context"
	"sync"
	"time"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// maxMessageAge bounds how long an undelivered message stays in the stream. It is the
// stream's own retention, distinct from how long any one delivery may be held.
const maxMessageAge = time.Hour

// cancelGrace bounds how long Stop waits for cancelled messages to unwind, after
// DrainTimeout has already passed.
const cancelGrace = 5 * time.Second

// connectNATS opens a connection that reconnects for the process lifetime.
func connectNATS(url, name string) (*nats.Conn, error) {
	connection, err := nats.Connect(url,
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			app.L().Warn("Disconnected from NATS", zap.Error(err))
		}),
		nats.ReconnectHandler(func(conn *nats.Conn) {
			app.L().Info("Reconnected to NATS", zap.String("url", conn.ConnectedUrl()))
		}),
	)
	if err != nil {
		return nil, errors.Wrap(err, "connect to NATS")
	}
	return connection, nil
}

// drainNATS flushes pending traffic and closes the connection.
func drainNATS(connection *nats.Conn) {
	if connection == nil {
		return
	}
	if err := connection.Drain(); err != nil {
		app.L().Warn("Draining the NATS connection failed", zap.Error(err))
	}
}

// ensureStream provisions the stream carrying messages on cfg.Topic.
//
// Both the publisher and the subscriber call it, because either may reach the bus first:
// publishing to a subject no stream is bound to fails. The call is idempotent, so
// whichever arrives first wins harmlessly.
//
// It is also authoritative rather than merely creative: the configuration below is
// applied every time either side starts, so an operator's out-of-band change to retention
// or age is reverted on the next restart. Change it here.
func ensureStream(ctx context.Context, js jetstream.JetStream, stream, subject string) error {
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      stream,
		Subjects:  []string{subject},
		Retention: jetstream.WorkQueuePolicy,
		MaxAge:    maxMessageAge,
	}); err != nil {
		return errors.Wrap(err, "provision JetStream stream")
	}
	return nil
}

// natsPublisher stores messages on a JetStream subject.
type natsPublisher struct {
	connection *nats.Conn
	stream     jetstream.JetStream
	subject    string
}

// openNATSPublisher connects, provisions the stream, and returns a publisher.
func openNATSPublisher(ctx context.Context, cfg Config) (Publisher, error) {
	connection, err := connectNATS(cfg.URL, cfg.ClientName)
	if err != nil {
		return nil, err
	}
	stream, err := jetstream.New(connection)
	if err != nil {
		connection.Close()
		return nil, errors.Wrap(err, "initialize JetStream")
	}
	if err := ensureStream(ctx, stream, cfg.Stream, cfg.Topic); err != nil {
		connection.Close()
		return nil, err
	}
	return &natsPublisher{connection: connection, stream: stream, subject: cfg.Topic}, nil
}

// Publish stores one message, deduplicating on dedupeID where one is supplied.
//
// A message with no ID is published without deduplication: sharing one key across every
// such message would silently discard all but the first.
func (p *natsPublisher) Publish(ctx context.Context, body []byte, dedupeID string) error {
	var options []jetstream.PublishOpt
	if dedupeID != "" {
		options = append(options, jetstream.WithMsgID(dedupeID))
	}
	if _, err := p.stream.Publish(ctx, p.subject, body, options...); err != nil {
		return errors.Wrap(err, "publish message to JetStream")
	}
	return nil
}

// Close drains and closes the connection.
func (p *natsPublisher) Close() { drainNATS(p.connection) }

// natsMsg is the subset of [jetstream.Msg] this driver needs. Narrowing it keeps the
// dispatch and keepalive paths testable without a live broker.
type natsMsg interface {
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
	InProgress() error
}

// natsMessage adapts one JetStream message to the Message contract.
//
// Every acknowledgement stops the keepalive first. That ordering is load-bearing: an
// InProgress landing after a Nak resets the redelivery delay the Nak just asked for, and
// one landing after an Ack is an error against a message that is gone. Holding it here
// rather than in the handler makes it impossible to get wrong.
type natsMessage struct {
	msg        natsMsg
	retryDelay time.Duration
	stop       func()
}

func (m *natsMessage) Data() []byte { return m.msg.Data() }

func (m *natsMessage) Ack() error {
	m.stop()
	return m.msg.Ack()
}

func (m *natsMessage) Nak() error {
	m.stop()
	return m.msg.NakWithDelay(m.retryDelay)
}

func (m *natsMessage) Term() error {
	m.stop()
	return m.msg.Term()
}

// natsSubscription is a running JetStream subscription.
type natsSubscription struct {
	connection *nats.Conn
	consume    jetstream.ConsumeContext
	slots      chan struct{}
	inFlight   sync.WaitGroup
	// lifetime is cancelled by Stop, never by the context passed to subscribeNATS.
	lifetime     context.Context
	cancel       context.CancelFunc
	drainTimeout time.Duration
	// cancelGrace is a field rather than the constant so tests can shorten it.
	cancelGrace time.Duration
}

// subscribeNATS provisions the stream and durable consumer, then begins consuming.
//
// ctx bounds provisioning only. Messages are handled under a lifetime context that only
// Stop cancels, because the caller's ctx is typically the process signal context: handing
// it to in-flight work would cancel every message the instant SIGTERM arrived, which is
// what Stop exists to avoid.
func subscribeNATS(ctx context.Context, cfg Config, handler Handler) (Subscription, error) {
	connection, err := connectNATS(cfg.URL, cfg.ClientName)
	if err != nil {
		return nil, err
	}
	stream, err := jetstream.New(connection)
	if err != nil {
		connection.Close()
		return nil, errors.Wrap(err, "initialize JetStream")
	}
	if err := ensureStream(ctx, stream, cfg.Stream, cfg.Topic); err != nil {
		connection.Close()
		return nil, err
	}
	consumer, err := stream.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:       cfg.Consumer,
		FilterSubject: cfg.Topic,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
		MaxAckPending: cfg.MaxInFlight,
	})
	if err != nil {
		connection.Close()
		return nil, errors.Wrap(err, "provision JetStream consumer")
	}

	schedule := newTiming(cfg.AckWait, cfg.MaxProcessingTime)
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	subscription := &natsSubscription{
		connection:   connection,
		slots:        make(chan struct{}, cfg.MaxInFlight),
		lifetime:     lifetime,
		cancel:       cancel,
		drainTimeout: cfg.DrainTimeout,
		cancelGrace:  cancelGrace,
	}
	consume, err := consumer.Consume(func(msg jetstream.Msg) {
		subscription.dispatch(msg, handler, schedule)
	})
	if err != nil {
		cancel()
		connection.Close()
		return nil, errors.Wrap(err, "consume JetStream messages")
	}
	subscription.consume = consume
	app.L().Info("Consuming messages from NATS",
		zap.String("stream", cfg.Stream),
		zap.String("subject", cfg.Topic),
		zap.String("durable", cfg.Consumer),
		zap.Duration("ack_wait", cfg.AckWait),
		zap.Int("max_deliver", cfg.MaxDeliver),
		zap.Int("max_in_flight", cfg.MaxInFlight),
		zap.Duration("drain_timeout", cfg.DrainTimeout),
	)
	return subscription, nil
}

// dispatch runs one message on its own goroutine so a slow message does not stall the
// others. JetStream invokes the callback sequentially, so without this a single long
// generation would halt the whole subscriber. The slot budget keeps the number of
// messages held at once within MaxInFlight.
func (s *natsSubscription) dispatch(msg natsMsg, handler Handler, schedule timing) {
	select {
	case s.slots <- struct{}{}:
	case <-s.lifetime.Done():
		return
	}
	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		defer func() { <-s.slots }()
		stop := keepAlive(msg, schedule.heartbeat, schedule.maxProcessing)
		// A safety net only: every acknowledgement path already stops it.
		defer stop()
		handler(s.lifetime, &natsMessage{msg: msg, retryDelay: schedule.retryDelay, stop: stop})
	}()
}

// Stop stops consuming and gives the messages already in hand up to DrainTimeout to
// finish, so an ordinary SIGTERM completes the replies a user is waiting on rather than
// abandoning them. Whatever has not finished by then is cancelled and left
// un-acknowledged for redelivery.
func (s *natsSubscription) Stop() {
	if s == nil {
		return
	}
	defer s.cancel()
	if s.consume != nil {
		s.consume.Drain()
		<-s.consume.Closed()
	}
	s.awaitInFlight()
	drainNATS(s.connection)
}

// awaitInFlight waits out the drain budget, then cancels whatever is left.
func (s *natsSubscription) awaitInFlight() {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		s.inFlight.Wait()
	}()
	timer := time.NewTimer(s.drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
		return
	case <-timer.C:
	}
	app.L().Warn("Cancelling messages still in flight at the drain deadline",
		zap.Duration("drain_timeout", s.drainTimeout))
	s.cancel()
	// Bounded, because cancellation is a request rather than a guarantee: a message
	// blocked somewhere that does not watch its context would otherwise hang shutdown
	// until the platform resorts to SIGKILL. Whatever is left is abandoned
	// un-acknowledged and redelivered to another replica.
	// A zero value must not mean "no grace at all": that would abandon every cancelled
	// message the instant the deadline passed, before it could even record its Nak.
	grace := s.cancelGrace
	if grace <= 0 {
		grace = cancelGrace
	}
	stragglers := time.NewTimer(grace)
	defer stragglers.Stop()
	select {
	case <-drained:
	case <-stragglers.C:
		app.L().Warn("Abandoning messages that did not stop after cancellation",
			zap.Duration("cancel_grace", grace))
	}
}

// timing is the per-message schedule, derived from the configuration so the parts never
// disagree.
type timing struct {
	// heartbeat is how often a held message is marked in progress. Half of AckWait leaves
	// room for one missed tick before the broker assumes the subscriber is gone.
	heartbeat time.Duration
	// retryDelay is how long a failed message waits before redelivery.
	retryDelay time.Duration
	// maxProcessing bounds the heartbeat, so a wedged handler releases its message rather
	// than holding it for the process lifetime.
	maxProcessing time.Duration
}

func newTiming(ackWait, maxProcessing time.Duration) timing {
	return timing{heartbeat: ackWait / 2, retryDelay: ackWait, maxProcessing: maxProcessing}
}

// keepAlive resets the redelivery timer until the returned stop is called, or until
// maxProcessing elapses.
//
// stop is idempotent and blocks until the heartbeat has actually finished, so a caller
// can stop it at a precise point and still defer it as a safety net.
func keepAlive(msg natsMsg, interval, maxProcessing time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		deadline := time.NewTimer(maxProcessing)
		defer deadline.Stop()
		for {
			select {
			case <-done:
				return
			case <-deadline.C:
				app.L().Warn("Releasing a message held past the processing limit",
					zap.Duration("max_processing_time", maxProcessing))
				return
			case <-ticker.C:
				if err := msg.InProgress(); err != nil {
					app.L().Warn("Message keepalive failed", zap.Error(err))
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-stopped
	}
}
