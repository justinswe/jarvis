package publisher

import (
	"context"
	"testing"
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeStream struct {
	subject  string
	payload  []byte
	deadline bool
	err      error
	calls    int
}

func (s *fakeStream) Publish(ctx context.Context, subject string, payload []byte, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	s.calls++
	s.subject = subject
	s.payload = payload
	_, s.deadline = ctx.Deadline()
	if s.err != nil {
		return nil, s.err
	}
	return &jetstream.PubAck{Stream: "JARVIS_DISCORD", Sequence: 1}, nil
}

func TestPublisherStoresRawProtobuf(t *testing.T) {
	want := &discordv1.IngestMessageRequest{Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message"}}
	stream := &fakeStream{}
	publisher, err := New(stream, "jarvis.discord.v1.messages", time.Second)
	require.NoError(t, err)

	require.NoError(t, publisher.Publish(context.Background(), want))
	assert.Equal(t, 1, stream.calls)
	assert.Equal(t, "jarvis.discord.v1.messages", stream.subject)
	got := &discordv1.IngestMessageRequest{}
	require.NoError(t, proto.Unmarshal(stream.payload, got))
	assert.True(t, proto.Equal(want, got))
}

func TestPublisherBoundsEachPublishByTimeout(t *testing.T) {
	stream := &fakeStream{}
	publisher, err := New(stream, "subject", time.Second)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(context.Background(), &discordv1.IngestMessageRequest{}))
	assert.True(t, stream.deadline, "publish should carry a deadline")
}

func TestPublisherReportsBrokerFailure(t *testing.T) {
	stream := &fakeStream{err: errors.New("no stream matches subject")}
	publisher, err := New(stream, "subject", time.Second)
	require.NoError(t, err)
	err = publisher.Publish(context.Background(), &discordv1.IngestMessageRequest{})
	assert.ErrorContains(t, err, "no stream matches subject")
}

func TestPublisherPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := &fakeStream{err: context.Canceled}
	publisher, err := New(stream, "subject", time.Second)
	require.NoError(t, err)
	assert.ErrorIs(t, publisher.Publish(ctx, &discordv1.IngestMessageRequest{}), context.Canceled)
}

// TestPublisherDeduplicatesOnMessageID covers the retry path: publishing the same
// Discord message twice must carry a dedup ID so JetStream stores it once, rather
// than queueing the same reply for a second time.
func TestPublisherDeduplicatesOnMessageID(t *testing.T) {
	publisher, err := New(&fakeStream{}, "subject", time.Second)
	require.NoError(t, err)

	withID := &discordv1.IngestMessageRequest{
		Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message"},
	}
	assert.Len(t, publisher.options(withID), 1, "a message with an ID must be deduplicated")
	assert.Equal(t, "message", messageID(withID))
}

// TestPublisherSkipsDeduplicationWithoutAnID is the important half: one shared dedup
// key across every ID-less message would silently discard all but the first.
func TestPublisherSkipsDeduplicationWithoutAnID(t *testing.T) {
	publisher, err := New(&fakeStream{}, "subject", time.Second)
	require.NoError(t, err)

	for name, message := range map[string]*discordv1.IngestMessageRequest{
		"nil request": nil,
		"no event":    {},
		"empty ID":    {Event: &discordv1.DiscordMessageCreateEvent{}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, messageID(message))
			assert.Empty(t, publisher.options(message))
		})
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	_, err := New(nil, "subject", time.Second)
	assert.Error(t, err)
	_, err = New(&fakeStream{}, "", time.Second)
	assert.Error(t, err)
	_, err = New(&fakeStream{}, "subject", 0)
	assert.Error(t, err)
}
