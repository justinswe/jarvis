package consumer

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// liveStream dials the configured NATS server, skipping when none is configured.
//
// Only these tests exercise real JetStream provisioning, acknowledgement, and
// redelivery. Unit tests fake the message, so behavior against a real broker is
// verified only here.
func liveStream(t *testing.T) (jetstream.JetStream, Config) {
	t.Helper()
	if manualTestOptions.natsURL == "" {
		t.Skip("set --nats-url (or NATS_URL) to run live NATS tests")
	}
	connection, err := nats.Connect(manualTestOptions.natsURL)
	require.NoError(t, err)
	t.Cleanup(connection.Close)

	stream, err := jetstream.New(connection)
	require.NoError(t, err)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	cfg := Config{
		Stream:        "JARVIS_LIVE_" + suffix,
		Subject:       "jarvis.live." + suffix + ".messages",
		Durable:       "jarvis-live-" + suffix,
		AckWait:       2 * time.Second,
		MaxDeliver:    3,
		MaxAckPending: 4,
		DrainTimeout:  5 * time.Second,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stream.DeleteStream(ctx, cfg.Stream)
	})
	return stream, cfg
}

func publish(t *testing.T, stream jetstream.JetStream, cfg Config, request *discordv1.IngestMessageRequest) {
	t.Helper()
	body, err := proto.Marshal(request)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = stream.Publish(ctx, cfg.Subject, body)
	require.NoError(t, err)
}

// start runs a consumer that is stopped when the test ends.
func start(t *testing.T, stream jetstream.JetStream, cfg Config, processor Processor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	subscription, err := Start(ctx, stream, cfg, processor, nil)
	require.NoError(t, err)
	t.Cleanup(subscription.Stop)
}

func TestLiveConsumerProcessesPublishedMessage(t *testing.T) {
	stream, cfg := liveStream(t)
	processed := make(chan *discordgo.MessageCreate, 1)
	start(t, stream, cfg, &fakeProcessor{process: func(_ context.Context, message *discordgo.MessageCreate) error {
		processed <- message
		return nil
	}})

	publish(t, stream, cfg, validRequest())

	select {
	case message := <-processed:
		assert.Equal(t, "message", message.ID)
		assert.Equal(t, "guild", message.GuildID)
		assert.Equal(t, discordgo.MessageTypeReply, message.Type)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the message to be processed")
	}
}

func TestLiveConsumerRedeliversAfterProcessingFailure(t *testing.T) {
	stream, cfg := liveStream(t)
	var mu sync.Mutex
	deliveries := 0
	succeeded := make(chan struct{})
	start(t, stream, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		mu.Lock()
		deliveries++
		attempt := deliveries
		mu.Unlock()
		if attempt == 1 {
			return errors.New("model unavailable")
		}
		close(succeeded)
		return nil
	}})

	publish(t, stream, cfg, validRequest())

	select {
	case <-succeeded:
	case <-time.After(20 * time.Second):
		t.Fatal("a failed message must be redelivered")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, deliveries)
}

func TestLiveConsumerStopsRedeliveringAfterMaxDeliver(t *testing.T) {
	stream, cfg := liveStream(t)
	cfg.MaxDeliver = 2
	var mu sync.Mutex
	deliveries := 0
	start(t, stream, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		mu.Lock()
		deliveries++
		mu.Unlock()
		return errors.New("permanently failing")
	}})

	publish(t, stream, cfg, validRequest())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return deliveries == 2
	}, 20*time.Second, 100*time.Millisecond, "the message must be delivered exactly MaxDeliver times")

	// Give the broker room to redeliver again, and confirm it does not.
	time.Sleep(3 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, deliveries, "delivery must stop at MaxDeliver")
}

func TestLiveConsumerTerminatesUnprocessableMessage(t *testing.T) {
	stream, cfg := liveStream(t)
	var mu sync.Mutex
	processed := 0
	start(t, stream, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		mu.Lock()
		processed++
		mu.Unlock()
		return nil
	}})

	// Missing author: valid protobuf, but permanently unprocessable.
	publish(t, stream, cfg, &discordv1.IngestMessageRequest{
		Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message", ChannelId: "channel"},
	})
	publish(t, stream, cfg, validRequest())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return processed == 1
	}, 20*time.Second, 100*time.Millisecond, "the valid message must be processed")

	time.Sleep(3 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, processed, "the unprocessable message must never be redelivered")
}

func TestLiveConsumerProcessesMessagesConcurrently(t *testing.T) {
	stream, cfg := liveStream(t)
	cfg.AckWait = 30 * time.Second
	const messages = 3
	var mu sync.Mutex
	active, peak := 0, 0
	release := make(chan struct{})
	done := make(chan struct{}, messages)
	start(t, stream, cfg, &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
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
		publish(t, stream, cfg, validRequest())
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return peak == messages
	}, 20*time.Second, 50*time.Millisecond, "a slow message must not stall the others")
	close(release)
	for range messages {
		<-done
	}
}
