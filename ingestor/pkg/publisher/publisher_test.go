package publisher

import (
	"context"
	"testing"
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeQueue struct {
	payload  []byte
	dedupeID string
	deadline bool
	err      error
	calls    int
}

func (q *fakeQueue) Publish(ctx context.Context, body []byte, dedupeID string) error {
	q.calls++
	q.payload = body
	q.dedupeID = dedupeID
	_, q.deadline = ctx.Deadline()
	return q.err
}

func (q *fakeQueue) Close() {}

func TestPublisherStoresRawProtobuf(t *testing.T) {
	want := &discordv1.IngestMessageRequest{Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message"}}
	queue := &fakeQueue{}
	publisher, err := New(queue, time.Second)
	require.NoError(t, err)

	require.NoError(t, publisher.Publish(t.Context(), want))
	assert.Equal(t, 1, queue.calls)
	got := &discordv1.IngestMessageRequest{}
	require.NoError(t, proto.Unmarshal(queue.payload, got))
	assert.True(t, proto.Equal(want, got))
}

func TestPublisherBoundsEachPublishByTimeout(t *testing.T) {
	queue := &fakeQueue{}
	publisher, err := New(queue, time.Second)
	require.NoError(t, err)

	require.NoError(t, publisher.Publish(t.Context(), &discordv1.IngestMessageRequest{}))
	assert.True(t, queue.deadline, "publish should carry a deadline")
}

func TestPublisherReportsBrokerFailure(t *testing.T) {
	queue := &fakeQueue{err: errors.New("no stream matches subject")}
	publisher, err := New(queue, time.Second)
	require.NoError(t, err)

	assert.ErrorContains(t, publisher.Publish(t.Context(), &discordv1.IngestMessageRequest{}),
		"no stream matches subject")
}

func TestPublisherPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	publisher, err := New(&fakeQueue{err: context.Canceled}, time.Second)
	require.NoError(t, err)

	assert.ErrorIs(t, publisher.Publish(ctx, &discordv1.IngestMessageRequest{}), context.Canceled)
}

// TestPublisherDeduplicatesOnMessageID covers the retry path: publishing the same Discord
// message twice must carry a dedup ID so a broker that supports deduplication stores it
// once, rather than queueing the same reply for a second time.
func TestPublisherDeduplicatesOnMessageID(t *testing.T) {
	queue := &fakeQueue{}
	publisher, err := New(queue, time.Second)
	require.NoError(t, err)

	require.NoError(t, publisher.Publish(t.Context(), &discordv1.IngestMessageRequest{
		Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message"},
	}))

	assert.Equal(t, "message", queue.dedupeID)
}

// TestPublisherSkipsDeduplicationWithoutAnID is the important half: one shared dedup key
// across every ID-less message would silently discard all but the first.
func TestPublisherSkipsDeduplicationWithoutAnID(t *testing.T) {
	for name, message := range map[string]*discordv1.IngestMessageRequest{
		"nil request": nil,
		"no event":    {},
		"empty ID":    {Event: &discordv1.DiscordMessageCreateEvent{}},
	} {
		t.Run(name, func(t *testing.T) {
			queue := &fakeQueue{}
			publisher, err := New(queue, time.Second)
			require.NoError(t, err)

			require.NoError(t, publisher.Publish(t.Context(), message))
			assert.Empty(t, queue.dedupeID)
		})
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	_, err := New(nil, time.Second)
	assert.Error(t, err)
	_, err = New(&fakeQueue{}, 0)
	assert.Error(t, err)
}
