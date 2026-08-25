package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeOutcomeRecorder struct {
	started  []string
	finished []store.RequestOutcome
}

func TestProcessRecordsFailedOutcomeAndErrorReply(t *testing.T) {
	outcomes := &fakeOutcomeRecorder{}
	client := &fakeClient{sendMessage: func(_ context.Context, channelID, content string) (*discordgo.Message, error) {
		return &discordgo.Message{ID: "123456789012345680", ChannelID: channelID, Content: content}, nil
	}}
	processor := &Processor{
		botID: "bot", generator: &fakeGenerator{err: errors.New("provider unavailable")},
		client: client, configs: testProvider(t), outcomes: outcomes,
	}
	message := targetedMessage("123456789012345678", "question")

	assert.Error(t, processor.Process(context.Background(), message))
	require.Len(t, outcomes.finished, 1)
	assert.Equal(t, store.RequestStatusFailed, outcomes.finished[0].Status)
	assert.Equal(t, "generation", outcomes.finished[0].ErrorKind)
	assert.Equal(t, []string{"123456789012345680"}, outcomes.finished[0].ReplyMessageIDs)
}

func (r *fakeOutcomeRecorder) StartRequest(_ context.Context, _, _, messageID string, _ int) error {
	r.started = append(r.started, messageID)
	return nil
}

func (r *fakeOutcomeRecorder) FinishRequest(_ context.Context, outcome store.RequestOutcome) error {
	r.finished = append(r.finished, outcome)
	return nil
}

func TestProcessRecordsAnsweredOutcome(t *testing.T) {
	outcomes := &fakeOutcomeRecorder{}
	client := &fakeClient{sendMessage: func(_ context.Context, channelID, content string) (*discordgo.Message, error) {
		return &discordgo.Message{ID: "123456789012345679", ChannelID: channelID, Content: content}, nil
	}}
	processor := &Processor{
		botID: "bot", generator: &fakeGenerator{response: genai.GenerateResponse{Text: "answer"}},
		client: client, configs: testProvider(t), outcomes: outcomes,
	}
	message := targetedMessage("123456789012345678", "question")

	require.NoError(t, processor.Process(context.Background(), message))
	assert.Equal(t, []string{message.ID}, outcomes.started)
	require.Len(t, outcomes.finished, 1)
	assert.Equal(t, store.RequestStatusAnswered, outcomes.finished[0].Status)
	assert.Equal(t, []string{"123456789012345679"}, outcomes.finished[0].ReplyMessageIDs)
}
