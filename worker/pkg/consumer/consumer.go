// Package consumer processes normalized Discord messages from a JetStream stream.
//
// A message stays un-acknowledged for as long as the worker is working on it,
// so a worker that crashes mid-message has that message redelivered rather than
// losing it. Messages that can never succeed are terminated instead of retried.
package consumer

import (
	"context"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// cancelGrace bounds how long Stop waits for cancelled messages to unwind before
// abandoning them, after DrainTimeout has already passed.
const cancelGrace = 5 * time.Second

// Processor handles a normalized Discord message request.
type Processor interface {
	Process(context.Context, *discordgo.MessageCreate) error
}

// Recorder persists one validated normalized Discord message before processing.
type Recorder interface {
	Record(context.Context, *discordv1.DiscordMessageCreateEvent) error
}

// message is the subset of [jetstream.Msg] the handler needs.
type message interface {
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
	InProgress() error
}

// Config configures the stream and the durable consumer backing one worker.
type Config struct {
	Stream        string
	Subject       string
	Durable       string
	AckWait       time.Duration
	MaxDeliver    int
	MaxAckPending int
	// DrainTimeout bounds how long Stop lets in-flight messages finish before it
	// cancels them. It must fit inside the platform's shutdown grace period.
	DrainTimeout time.Duration
}

// Consumer is a running JetStream subscription.
type Consumer struct {
	consume  jetstream.ConsumeContext
	slots    chan struct{}
	inFlight sync.WaitGroup
	// lifetime is cancelled by Stop, never by the context passed to Start. See Start.
	lifetime     context.Context
	cancel       context.CancelFunc
	drainTimeout time.Duration
	// cancelGrace bounds the wait after cancellation. A field rather than the constant
	// so tests can shorten it.
	cancelGrace time.Duration
}

// Start provisions the stream and durable consumer, then begins consuming.
// Messages are handled until Stop is called.
//
// ctx bounds provisioning only. Message handling runs under a lifetime context that
// only Stop cancels, because the caller's ctx is typically the process signal context:
// handing it to in-flight generations would cancel every one of them the instant
// SIGTERM arrived, which is what Stop exists to avoid.
func Start(ctx context.Context, js jetstream.JetStream, cfg Config, processor Processor, recorder Recorder) (*Consumer, error) {
	if js == nil {
		return nil, errors.New("JetStream context is required")
	}
	if processor == nil {
		return nil, errors.New("message processor is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := discordv1.EnsureStream(ctx, js, cfg.Stream, cfg.Subject); err != nil {
		return nil, err
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:       cfg.Durable,
		FilterSubject: cfg.Subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
		MaxAckPending: cfg.MaxAckPending,
	})
	if err != nil {
		return nil, errors.Wrap(err, "provision JetStream consumer")
	}

	schedule := newTiming(cfg.AckWait)
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	subscription := &Consumer{
		slots:        make(chan struct{}, cfg.MaxAckPending),
		lifetime:     lifetime,
		cancel:       cancel,
		drainTimeout: cfg.DrainTimeout,
		cancelGrace:  cancelGrace,
	}
	consume, err := consumer.Consume(func(msg jetstream.Msg) {
		subscription.dispatch(msg, processor, recorder, schedule)
	})
	if err != nil {
		cancel()
		return nil, errors.Wrap(err, "consume JetStream messages")
	}
	subscription.consume = consume
	app.L().Info("Consuming Discord messages",
		zap.String("stream", cfg.Stream),
		zap.String("subject", cfg.Subject),
		zap.String("durable", cfg.Durable),
		zap.Duration("ack_wait", cfg.AckWait),
		zap.Int("max_deliver", cfg.MaxDeliver),
		zap.Int("max_ack_pending", cfg.MaxAckPending),
		zap.Duration("drain_timeout", cfg.DrainTimeout),
	)
	return subscription, nil
}

// dispatch runs one message on its own goroutine so that a slow message does
// not stall the others. JetStream invokes the callback sequentially, so without
// this a single long generation would halt the whole worker. The slot budget
// keeps the number of messages held at once within MaxAckPending.
func (c *Consumer) dispatch(msg message, processor Processor, recorder Recorder, schedule timing) {
	select {
	case c.slots <- struct{}{}:
	case <-c.lifetime.Done():
		return
	}
	c.inFlight.Add(1)
	go func() {
		defer c.inFlight.Done()
		defer func() { <-c.slots }()
		handle(c.lifetime, msg, processor, recorder, schedule)
	}()
}

// Stop stops consuming and gives the messages already in hand up to DrainTimeout to
// finish, so an ordinary SIGTERM completes the replies a user is waiting on rather
// than abandoning them. Whatever has not finished by then is cancelled and left
// un-acknowledged for redelivery.
func (c *Consumer) Stop() {
	if c == nil {
		return
	}
	defer c.cancel()
	if c.consume != nil {
		c.consume.Drain()
		<-c.consume.Closed()
	}
	c.awaitInFlight()
}

// awaitInFlight waits out the drain budget, then cancels whatever is left.
func (c *Consumer) awaitInFlight() {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		c.inFlight.Wait()
	}()
	timer := time.NewTimer(c.drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
		return
	case <-timer.C:
	}
	app.L().Warn("Cancelling messages still in flight at the drain deadline",
		zap.Duration("drain_timeout", c.drainTimeout))
	c.cancel()
	// Bounded, because cancellation is a request rather than a guarantee: a message
	// blocked somewhere that does not watch its context would otherwise hang shutdown
	// until the platform resorts to SIGKILL. Whatever is left is abandoned
	// un-acknowledged and redelivered to another replica.
	// A zero value must not mean "no grace at all": that would abandon every cancelled
	// message the instant the deadline passed, before it could even record its Nak.
	grace := c.cancelGrace
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

// validate reports whether the consumer configuration is usable.
func (c Config) validate() error {
	if c.Stream == "" {
		return errors.New("stream name is required")
	}
	if c.Subject == "" {
		return errors.New("subject is required")
	}
	if c.Durable == "" {
		return errors.New("durable consumer name is required")
	}
	if c.AckWait <= 0 {
		return errors.New("ack wait must be positive")
	}
	if c.MaxDeliver <= 0 {
		return errors.New("max deliver must be positive")
	}
	if c.MaxAckPending <= 0 {
		return errors.New("max ack pending must be positive")
	}
	if c.DrainTimeout <= 0 {
		return errors.New("drain timeout must be positive")
	}
	return nil
}

// timing is the per-message schedule, derived from AckWait so the two never disagree.
type timing struct {
	// heartbeat is how often a held message is marked in progress. Half of AckWait
	// leaves room for one missed tick before the broker assumes the worker is gone.
	heartbeat time.Duration
	// retryDelay is how long a failed message waits before redelivery. Without it a
	// transient failure — a model 503, a Discord outage — burns every MaxDeliver
	// attempt within milliseconds, and the retries buy nothing.
	retryDelay time.Duration
}

func newTiming(ackWait time.Duration) timing {
	return timing{heartbeat: ackWait / 2, retryDelay: ackWait}
}

// handle processes one message, holding it un-acknowledged until it completes.
func handle(ctx context.Context, msg message, processor Processor, recorder Recorder, schedule timing) {
	request := &discordv1.IngestMessageRequest{}
	if err := proto.Unmarshal(msg.Data(), request); err != nil {
		terminate(msg, "invalid protobuf message", err)
		return
	}
	discordMsg, err := discordMessage(request)
	if err != nil {
		terminate(msg, "invalid Discord message", err)
		return
	}

	// Stopped explicitly before the acknowledgement below rather than by defer: an
	// InProgress landing after a Nak would reset the redelivery delay that Nak just
	// asked for, and one landing after an Ack is an error on a message that is gone.
	stop := keepAlive(msg, schedule.heartbeat)
	defer stop()

	fields := []zap.Field{
		zap.String("guild_id", discordMsg.GuildID),
		zap.String("channel_id", discordMsg.ChannelID),
		zap.String("message_id", discordMsg.ID),
	}
	if recorder != nil {
		if err := recorder.Record(ctx, request.Event); err != nil {
			app.L().Warn("Discord message recording failed", append(fields, zap.Error(err))...)
		}
	}
	processErr := processor.Process(ctx, discordMsg)
	stop()
	if processErr != nil {
		app.L().Warn("Discord message processing failed", append(fields, zap.Error(processErr))...)
		if err := msg.NakWithDelay(schedule.retryDelay); err != nil {
			app.L().Warn("Message negative acknowledgement failed", append(fields, zap.Error(err))...)
		}
		return
	}
	if err := msg.Ack(); err != nil {
		app.L().Warn("Message acknowledgement failed", append(fields, zap.Error(err))...)
	}
}

// keepAlive resets the redelivery timer until the returned stop is called.
//
// stop is idempotent and blocks until the heartbeat has actually finished, so a caller
// can stop it at a precise point and still defer it as a safety net.
func keepAlive(msg message, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
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

// terminate drops a message that redelivery could never make succeed.
func terminate(msg message, reason string, cause error) {
	app.L().Warn("Dropping unprocessable message", zap.String("reason", reason), zap.Error(cause))
	if err := msg.Term(); err != nil {
		app.L().Warn("Message termination failed", zap.Error(err))
	}
}
