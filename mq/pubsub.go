package mq

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

// dedupeAttribute carries the Discord message ID on a published message.
//
// It is advisory. Pub/Sub has no publisher-side deduplication, so this exists so an
// operator can correlate a message in the console or a dead-letter topic with the Discord
// message it came from — not to suppress a duplicate. Suppression is the reply claim's
// job, and on this driver it is the only thing doing it.
const dedupeAttribute = "jarvis-message-id"

// topicPath and subscriptionPath build the fully qualified resource names the API takes.
func topicPath(projectID, topic string) string {
	return fmt.Sprintf("projects/%s/topics/%s", projectID, topic)
}

func subscriptionPath(projectID, subscription string) string {
	return fmt.Sprintf("projects/%s/subscriptions/%s", projectID, subscription)
}

// pubSubPublisher stores messages on a Pub/Sub topic.
type pubSubPublisher struct {
	client    *pubsub.Client
	publisher *pubsub.Publisher
}

// openPubSubPublisher connects, verifies the topic exists, and returns a publisher.
func openPubSubPublisher(ctx context.Context, cfg Config) (Publisher, error) {
	client, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		return nil, errors.Wrap(err, "initialize Pub/Sub client")
	}
	if err := verifyTopic(ctx, client, cfg); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &pubSubPublisher{client: client, publisher: client.Publisher(topicPath(cfg.ProjectID, cfg.Topic))}, nil
}

// verifyTopic fails fast when the topic is missing.
//
// Unlike the NATS driver this never creates anything. Creating topics needs broad
// administrative IAM the running service should not hold, and a mistyped name would
// silently create a second topic nobody publishes to — an outage that looks like an empty
// queue. The error names the command that fixes it instead.
func verifyTopic(ctx context.Context, client *pubsub.Client, cfg Config) error {
	name := topicPath(cfg.ProjectID, cfg.Topic)
	if _, err := client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name}); err != nil {
		return errors.Wrapf(err, "Pub/Sub topic %s is unavailable; create it with "+
			"`gcloud pubsub topics create %s --project=%s`", name, cfg.Topic, cfg.ProjectID)
	}
	return nil
}

// Publish stores one message and waits for the server-assigned ID.
func (p *pubSubPublisher) Publish(ctx context.Context, body []byte, dedupeID string) error {
	message := &pubsub.Message{Data: body}
	if dedupeID != "" {
		message.Attributes = map[string]string{dedupeAttribute: dedupeID}
	}
	if _, err := p.publisher.Publish(ctx, message).Get(ctx); err != nil {
		return errors.Wrap(err, "publish message to Pub/Sub")
	}
	return nil
}

// Close flushes buffered messages and releases the client.
func (p *pubSubPublisher) Close() {
	p.publisher.Stop()
	if err := p.client.Close(); err != nil {
		app.L().Warn("Closing the Pub/Sub client failed", zap.Error(err))
	}
}

// pubSubMessage adapts one Pub/Sub message to the Message contract.
//
// There is no keepalive to stop: the client extends the acknowledgement deadline on its
// own up to MaxExtension, which is why Config.MaxProcessingTime is what bounds a held
// message here rather than a heartbeat.
type pubSubMessage struct {
	msg *pubsub.Message
}

func (m *pubSubMessage) Data() []byte { return m.msg.Data }
func (m *pubSubMessage) Ack() error   { m.msg.Ack(); return nil }
func (m *pubSubMessage) Nak() error   { m.msg.Nack(); return nil }

// Term acknowledges a message no redelivery could fix.
//
// Pub/Sub has no terminate, and nacking would retry something that can never succeed
// until the subscription's dead-letter policy gives up on it. Acknowledging drops it now,
// at the cost of leaving no trace in the dead-letter topic; the warning logged by the
// caller is the only record.
func (m *pubSubMessage) Term() error { m.msg.Ack(); return nil }

// pubSubSubscription is a running Pub/Sub receive loop.
type pubSubSubscription struct {
	client *pubsub.Client
	// stopReceiving ends delivery. abandon cancels the handlers still working, and is
	// deliberately a second cancel: see subscribePubSub.
	stopReceiving, abandon context.CancelFunc
	// stopped closes once Receive has returned and every handler has finished.
	stopped      chan struct{}
	drainTimeout time.Duration
	// cancelGrace is a field rather than the constant so tests can shorten it.
	cancelGrace time.Duration
	once        sync.Once
}

// subscribePubSub verifies the subscription exists, then receives from it.
//
// As in the NATS driver, ctx bounds setup only. Messages are handled under a lifetime
// context that only Stop cancels, so SIGTERM drains in-flight work rather than aborting
// every message at once.
func subscribePubSub(ctx context.Context, cfg Config, handler Handler) (Subscription, error) {
	client, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		return nil, errors.Wrap(err, "initialize Pub/Sub client")
	}
	name := subscriptionPath(cfg.ProjectID, cfg.Consumer)
	if _, err := client.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: name}); err != nil {
		_ = client.Close()
		return nil, errors.Wrapf(err, "Pub/Sub subscription %s is unavailable; create it with "+
			"`gcloud pubsub subscriptions create %s --topic=%s --project=%s`",
			name, cfg.Consumer, cfg.Topic, cfg.ProjectID)
	}

	subscriber := client.Subscriber(name)
	subscriber.ReceiveSettings.MaxOutstandingMessages = cfg.MaxInFlight
	// The client owns the keepalive this driver has no heartbeat for. Capping it is what
	// stops a wedged handler from holding a message for the process lifetime.
	subscriber.ReceiveSettings.MaxExtension = cfg.MaxProcessingTime
	// One extension is worth exactly one AckWait, matching what the NATS driver's
	// heartbeat buys per tick, so the two brokers redeliver on the same schedule when a
	// subscriber dies without acknowledging.
	subscriber.ReceiveSettings.MaxDurationPerAckExtension = cfg.AckWait

	lifetime, stopReceiving := context.WithCancel(context.WithoutCancel(ctx))
	// Handlers run under a second context rather than the one Receive hands them, because
	// the client derives that one from lifetime: cancelling lifetime to stop delivery would
	// also cancel every generation already in progress, and the drain budget below would
	// have nothing left to protect. This is the same guarantee natsSubscription.Stop makes.
	work, abandon := context.WithCancel(context.WithoutCancel(ctx))
	subscription := &pubSubSubscription{
		client:        client,
		stopReceiving: stopReceiving,
		abandon:       abandon,
		stopped:       make(chan struct{}),
		drainTimeout:  cfg.DrainTimeout,
		cancelGrace:   cancelGrace,
	}
	go func() {
		defer close(subscription.stopped)
		// Receive returns only once every handler it started has returned, so this is
		// also the drain signal Stop waits on.
		if err := subscriber.Receive(lifetime, func(_ context.Context, msg *pubsub.Message) {
			handler(work, &pubSubMessage{msg: msg})
		}); err != nil {
			app.L().Error("Pub/Sub receive stopped", zap.Error(err))
		}
	}()
	app.L().Info("Consuming messages from Pub/Sub",
		zap.String("project_id", cfg.ProjectID),
		zap.String("topic", cfg.Topic),
		zap.String("subscription", cfg.Consumer),
		zap.Duration("ack_wait", cfg.AckWait),
		zap.Duration("max_processing_time", cfg.MaxProcessingTime),
		zap.Int("max_in_flight", cfg.MaxInFlight),
		zap.Duration("drain_timeout", cfg.DrainTimeout),
	)
	return subscription, nil
}

// Stop ends delivery and gives the messages already in hand up to DrainTimeout to finish,
// so an ordinary SIGTERM completes the replies a user is waiting on rather than abandoning
// them. Whatever has not finished by then is cancelled and left un-acknowledged.
//
// Cancelling the lifetime is what stops Receive pulling new messages; the client then nacks
// anything it had pulled but not yet handed to a handler, so another replica picks those up
// immediately rather than waiting out the acknowledgement deadline.
func (s *pubSubSubscription) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.stopReceiving()
		if !s.awaitStopped(s.drainTimeout) {
			app.L().Warn("Cancelling Pub/Sub messages still in flight at the drain deadline",
				zap.Duration("drain_timeout", s.drainTimeout))
			s.abandon()
			// Bounded, because cancellation is a request rather than a guarantee: a
			// handler that does not watch its context would otherwise hold shutdown open
			// until the platform resorts to SIGKILL.
			grace := s.cancelGrace
			if grace <= 0 {
				grace = cancelGrace
			}
			if !s.awaitStopped(grace) {
				app.L().Warn("Abandoning Pub/Sub messages that did not stop after cancellation",
					zap.Duration("cancel_grace", grace))
			}
		}
		s.abandon()
		if err := s.client.Close(); err != nil {
			app.L().Warn("Closing the Pub/Sub client failed", zap.Error(err))
		}
	})
}

// awaitStopped reports whether Receive and every handler it started finished within limit.
func (s *pubSubSubscription) awaitStopped(limit time.Duration) bool {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-s.stopped:
		return true
	case <-timer.C:
		return false
	}
}
