package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRecorder struct {
	err       error
	messages  []string
	retention []int
}

func (r *fakeRecorder) Record(_ context.Context, message *discordgo.Message, retentionDays int) error {
	r.messages = append(r.messages, message.ID)
	r.retention = append(r.retention, retentionDays)
	return r.err
}

func TestRecordPersistsEveryMessageWithTheGuildRetention(t *testing.T) {
	recorder := &fakeRecorder{}
	processor := &Processor{recorder: recorder}

	processor.record(t.Context(), 30,
		&discordgo.Message{ID: "1", ChannelID: "c"},
		nil, // a failed chunk send can leave gaps
		&discordgo.Message{ID: "2", ChannelID: "c"},
	)

	assert.Equal(t, []string{"1", "2"}, recorder.messages)
	assert.Equal(t, []int{30, 30}, recorder.retention)
}

// TestRecordOutlivesACancelledRequest is why record detaches from the caller's context: a
// SIGTERM landing right after the reply is posted must not hollow out the stored thread.
func TestRecordOutlivesACancelledRequest(t *testing.T) {
	recorder := &fakeRecorder{}
	processor := &Processor{recorder: recorder}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	processor.record(ctx, 14, &discordgo.Message{ID: "1", ChannelID: "c"})

	assert.Equal(t, []string{"1"}, recorder.messages)
}

// TestProcessRecordsTargetedConversation pins where recording happens: after targeting,
// so stored history is exactly the conversation addressed to the bot plus its replies.
func TestProcessRecordsTargetedConversation(t *testing.T) {
	recorder := &fakeRecorder{}
	client := &fakeClient{sendMessage: func(_ context.Context, channelID, _ string) (*discordgo.Message, error) {
		return &discordgo.Message{ID: "reply-1", ChannelID: channelID}, nil
	}}
	processor := &Processor{
		botID: "bot", generator: &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}},
		client: client, configs: testProvider(t), recorder: recorder,
	}

	require.NoError(t, processor.Process(t.Context(), targetedMessage("m", "question")))

	assert.Equal(t, []string{"m", "reply-1"}, recorder.messages,
		"the inbound mention and the posted reply are the stored conversation")
	retention := testSettings().MessageRetentionDays
	assert.Equal(t, []int{retention, retention}, recorder.retention)
}

func TestProcessRecordsNothingForUntargetedMessages(t *testing.T) {
	recorder := &fakeRecorder{}
	processor := &Processor{botID: "bot", generator: &fakeGenerator{}, client: &fakeClient{},
		configs: testProvider(t), recorder: recorder}
	m := message("m", "ordinary channel chatter")
	m.ChannelID = "channel"

	require.NoError(t, processor.Process(t.Context(), &discordgo.MessageCreate{Message: m}))

	assert.Empty(t, recorder.messages, "untargeted traffic must never be stored")
}

func TestRecordToleratesAnUnavailableStore(t *testing.T) {
	processor := &Processor{recorder: &fakeRecorder{err: errors.New("store down")}}
	assert.NotPanics(t, func() {
		processor.record(t.Context(), 14, &discordgo.Message{ID: "1", ChannelID: "c"})
	}, "a failure costs stored context, never the reply")

	nothing := &Processor{}
	assert.NotPanics(t, func() { nothing.record(t.Context(), 14, &discordgo.Message{ID: "1"}) })
}
