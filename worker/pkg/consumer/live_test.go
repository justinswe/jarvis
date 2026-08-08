package consumer

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/mq"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Only these tests exercise real provisioning, acknowledgement, and redelivery through an
// mq driver. Unit tests fake the message, so behavior against a real broker is verified
// only here — and because almost every test below runs against both brokers, they are
// also what proves the two drivers actually agree.

// eachBroker runs one test body against every broker that is configured.
//
// tune adjusts the configuration before any resource is provisioned from it, because
// Pub/Sub bakes the delivery policy into the subscription at creation time: a test that
// changed AckWait afterwards would be describing a subscription that does not exist.
func eachBroker(t *testing.T, tune func(*mq.Config), body func(*testing.T, mq.Config)) {
	t.Helper()
	for _, driver := range []mq.Driver{mq.DriverNATS, mq.DriverPubSub} {
		t.Run(string(driver), func(t *testing.T) { body(t, liveConfig(t, driver, tune)) })
	}
}

// liveConfig returns the configuration for one broker, skipping when it is not set up.
func liveConfig(t *testing.T, driver mq.Driver, tune func(*mq.Config)) mq.Config {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	cfg := mq.Config{
		Driver:            driver,
		ClientName:        "jarvis-live-" + suffix,
		Consumer:          "jarvis-live-" + suffix,
		AckWait:           natsAckWait,
		MaxProcessingTime: time.Minute,
		DrainTimeout:      5 * time.Second,
		MaxDeliver:        minDeliveryAttempts,
		MaxInFlight:       4,
	}
	if driver == mq.DriverPubSub {
		if manualTestOptions.pubsubProjectID == "" {
			t.Skip("set --pubsub-project-id (or PUBSUB_PROJECT_ID) to run live Pub/Sub tests")
		}
		cfg.ProjectID = manualTestOptions.pubsubProjectID
		cfg.Topic = "jarvis-live-" + suffix
		// Pub/Sub refuses anything shorter, so the two brokers cannot share one value.
		cfg.AckWait = pubSubAckWait
		if tune != nil {
			tune(&cfg)
		}
		createPubSubResources(t, cfg)
		return cfg
	}
	if manualTestOptions.natsURL == "" {
		t.Skip("set --nats-url (or NATS_URL) to run live NATS tests")
	}
	cfg.URL = manualTestOptions.natsURL
	cfg.Stream = "JARVIS_LIVE_" + suffix
	cfg.Topic = "jarvis.live." + suffix + ".messages"
	if tune != nil {
		tune(&cfg)
	}
	t.Cleanup(func() { deleteStream(t, cfg) })
	return cfg
}

// createPubSubResources provisions the per-run topic, dead-letter topic, and subscription.
//
// The driver deliberately refuses to create them, so the test has to. What it creates
// mirrors what docs/pubsub.md tells an operator to provision, which is the point: the
// delivery policy the NATS driver asserts on its consumer lives here instead.
func createPubSubResources(t *testing.T, cfg mq.Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := pubsub.NewClient(ctx, cfg.ProjectID)
	require.NoError(t, err)

	prefix := "projects/" + cfg.ProjectID
	topic := prefix + "/topics/" + cfg.Topic
	deadLetter := prefix + "/topics/" + cfg.Topic + "-dead-letter"
	subscription := prefix + "/subscriptions/" + cfg.Consumer
	for _, name := range []string{topic, deadLetter} {
		_, err = client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name})
		require.NoError(t, err)
	}
	_, err = client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name:               subscription,
		Topic:              topic,
		AckDeadlineSeconds: int32(cfg.AckWait / time.Second),
		RetryPolicy: &pubsubpb.RetryPolicy{
			MinimumBackoff: durationpb.New(cfg.AckWait),
			MaximumBackoff: durationpb.New(cfg.AckWait),
		},
		DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{
			DeadLetterTopic:     deadLetter,
			MaxDeliveryAttempts: int32(cfg.MaxDeliver),
		},
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = client.SubscriptionAdminClient.DeleteSubscription(cleanupCtx,
			&pubsubpb.DeleteSubscriptionRequest{Subscription: subscription})
		for _, name := range []string{topic, deadLetter} {
			_ = client.TopicAdminClient.DeleteTopic(cleanupCtx, &pubsubpb.DeleteTopicRequest{Topic: name})
		}
		_ = client.Close()
	})
}

// deleteStream removes the per-run NATS stream so repeated runs do not accumulate them.
func deleteStream(t *testing.T, cfg mq.Config) {
	t.Helper()
	connection, err := nats.Connect(cfg.URL)
	if err != nil {
		return
	}
	defer connection.Close()
	stream, err := jetstream.New(connection)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = stream.DeleteStream(ctx, cfg.Stream)
}

// publish stores one request through the same publisher the ingestor uses.
//
// It deliberately passes no deduplication ID. These tests publish several distinct
// deliveries that share one Discord message ID, and a broker that deduplicates would
// otherwise collapse them into the single message deduplication exists to guarantee.
func publish(t *testing.T, cfg mq.Config, request *discordv1.IngestMessageRequest) {
	t.Helper()
	publishWithID(t, cfg, request, "")
}

// publishWithID stores one request under an explicit deduplication ID.
func publishWithID(t *testing.T, cfg mq.Config, request *discordv1.IngestMessageRequest, dedupeID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	publisher, err := mq.OpenPublisher(ctx, cfg)
	require.NoError(t, err)
	defer publisher.Close()
	body, err := proto.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(ctx, body, dedupeID))
}

// start runs a consumer that is stopped when the test ends.
func start(t *testing.T, cfg mq.Config, processor Processor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	subscription, err := Start(ctx, cfg, processor, nil)
	require.NoError(t, err)
	t.Cleanup(subscription.Stop)
}

// The delivery policy these tests run with, at each broker's fastest legal setting so the
// suite stays as short as the platforms allow. Pub/Sub rejects a dead-letter policy below
// five attempts and an acknowledgement deadline below ten seconds; JetStream has neither
// floor, so it runs tighter.
const (
	minDeliveryAttempts = 5
	natsAckWait         = 2 * time.Second
	pubSubAckWait       = 10 * time.Second
)

// settleWindow is how long a test waits for the broker to finish redelivering, derived
// from the policy so the slower broker is not held to the faster one's schedule.
func settleWindow(cfg mq.Config) time.Duration {
	return cfg.AckWait*time.Duration(cfg.MaxDeliver+2) + 10*time.Second
}

// quietWindow is how long a test watches for a delivery that must not arrive.
func quietWindow(cfg mq.Config) time.Duration { return cfg.AckWait + 3*time.Second }

// counter records deliveries from the handler goroutines.
type counter struct {
	mu    sync.Mutex
	count int
}

func (c *counter) increment() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	return c.count
}

func (c *counter) value() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func TestLiveConsumerProcessesPublishedMessage(t *testing.T) {
	eachBroker(t, nil, func(t *testing.T, cfg mq.Config) {
		processed := make(chan *discordgo.MessageCreate, 1)
		start(t, cfg, &fakeProcessor{process: func(_ context.Context, message *discordgo.MessageCreate) error {
			processed <- message
			return nil
		}})

		publish(t, cfg, validRequest())

		select {
		case message := <-processed:
			assert.Equal(t, "message", message.ID)
			assert.Equal(t, "guild", message.GuildID)
			assert.Equal(t, discordgo.MessageTypeReply, message.Type)
		case <-time.After(settleWindow(cfg)):
			t.Fatal("timed out waiting for the message to be processed")
		}
	})
}

func TestLiveConsumerRedeliversAfterProcessingFailure(t *testing.T) {
	eachBroker(t, nil, func(t *testing.T, cfg mq.Config) {
		deliveries := &counter{}
		succeeded := make(chan struct{})
		start(t, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
			if deliveries.increment() == 1 {
				return errors.New("model unavailable")
			}
			close(succeeded)
			return nil
		}})

		publish(t, cfg, validRequest())

		select {
		case <-succeeded:
		case <-time.After(settleWindow(cfg)):
			t.Fatal("a failed message must be redelivered")
		}
		assert.Equal(t, 2, deliveries.value())
	})
}

// TestLiveConsumerStopsRedeliveringAfterMaxDeliver covers the one delivery limit the two
// brokers enforce from different places: JetStream from the consumer the driver
// provisions, Pub/Sub from the subscription's dead-letter policy.
func TestLiveConsumerStopsRedeliveringAfterMaxDeliver(t *testing.T) {
	eachBroker(t, func(cfg *mq.Config) { cfg.MaxDeliver = minDeliveryAttempts }, func(t *testing.T, cfg mq.Config) {
		deliveries := &counter{}
		start(t, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
			deliveries.increment()
			return errors.New("permanently failing")
		}})

		publish(t, cfg, validRequest())

		require.Eventually(t, func() bool { return deliveries.value() == minDeliveryAttempts },
			settleWindow(cfg), 100*time.Millisecond, "the message must be delivered exactly MaxDeliver times")

		// Give the broker room to redeliver again, and confirm it does not.
		time.Sleep(quietWindow(cfg))
		assert.Equal(t, minDeliveryAttempts, deliveries.value(), "delivery must stop at MaxDeliver")
	})
}

func TestLiveConsumerTerminatesUnprocessableMessage(t *testing.T) {
	eachBroker(t, nil, func(t *testing.T, cfg mq.Config) {
		processed := &counter{}
		start(t, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
			processed.increment()
			return nil
		}})

		// Missing author: valid protobuf, but permanently unprocessable.
		publish(t, cfg, &discordv1.IngestMessageRequest{
			Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message", ChannelId: "channel"},
		})
		publish(t, cfg, validRequest())

		require.Eventually(t, func() bool { return processed.value() == 1 },
			settleWindow(cfg), 100*time.Millisecond, "the valid message must be processed")

		time.Sleep(quietWindow(cfg))
		assert.Equal(t, 1, processed.value(), "the unprocessable message must never be redelivered")
	})
}

func TestLiveConsumerProcessesMessagesConcurrently(t *testing.T) {
	eachBroker(t, func(cfg *mq.Config) { cfg.AckWait = 30 * time.Second }, func(t *testing.T, cfg mq.Config) {
		const messages = 3
		var mu sync.Mutex
		active, peak := 0, 0
		release := make(chan struct{})
		done := make(chan struct{}, messages)
		start(t, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()
			<-release
			mu.Lock()
			active--
			mu.Unlock()
			done <- struct{}{}
			return nil
		}})

		for range messages {
			publish(t, cfg, validRequest())
		}
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return peak == messages
		}, settleWindow(cfg), 50*time.Millisecond, "a slow message must not stall the others")
		close(release)
		for range messages {
			<-done
		}
	})
}

// TestLiveConsumerDrainsInFlightWorkOnStop is the shutdown guarantee, checked against both
// drivers because they arrive at it by different means and one of them nearly did not.
//
// Pub/Sub derives the context it hands a handler from the one passed to Receive, so a
// driver that cancelled that context to stop delivery would abort every reply already in
// progress — on the platform where SIGTERM precedes every deployment. The handler here
// must observe a live context after Stop has begun.
func TestLiveConsumerDrainsInFlightWorkOnStop(t *testing.T) {
	eachBroker(t, nil, func(t *testing.T, cfg mq.Config) {
		deliveries := &counter{}
		started, release := make(chan struct{}), make(chan struct{})
		observed := make(chan error, 1)

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		subscription, err := Start(ctx, cfg, &fakeProcessor{
			process: func(msgCtx context.Context, _ *discordgo.MessageCreate) error {
				if deliveries.increment() > 1 {
					return nil
				}
				close(started)
				<-release
				observed <- msgCtx.Err()
				return nil
			},
		}, nil)
		require.NoError(t, err)

		publish(t, cfg, validRequest())
		select {
		case <-started:
		case <-time.After(settleWindow(cfg)):
			t.Fatal("timed out waiting for the message to be delivered")
		}

		stopped := make(chan struct{})
		go func() { defer close(stopped); subscription.Stop() }()
		// Long enough for Stop to have cancelled anything it was going to, well inside
		// the drain budget.
		time.Sleep(500 * time.Millisecond)
		close(release)
		<-stopped

		assert.NoError(t, <-observed, "shutdown must let an in-flight reply finish, not cancel it")
	})
}

// TestLiveDeduplicationDiffersByBroker pins the one place the drivers genuinely disagree,
// because the whole active-active design turns on it. JetStream drops a republished
// message. Pub/Sub has no publisher-side deduplication and delivers it twice, which is
// exactly why a multi-site deployment needs the reply claim and cannot lean on the broker.
func TestLiveDeduplicationDiffersByBroker(t *testing.T) {
	eachBroker(t, nil, func(t *testing.T, cfg mq.Config) {
		processed := &counter{}
		start(t, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
			processed.increment()
			return nil
		}})

		publishWithID(t, cfg, validRequest(), "message")
		publishWithID(t, cfg, validRequest(), "message")

		want := 1
		if cfg.Driver == mq.DriverPubSub {
			want = 2
		}
		require.Eventually(t, func() bool { return processed.value() == want },
			settleWindow(cfg), 100*time.Millisecond, "expected %d deliveries on %s", want, cfg.Driver)

		time.Sleep(quietWindow(cfg))
		assert.Equal(t, want, processed.value())
	})
}
