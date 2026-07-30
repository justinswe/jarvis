package discord

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageReactionToolDeclaration(t *testing.T) {
	declaration := (messageReactionTool{}).Declaration()
	assert.Equal(t, messageReactionToolName, declaration.Name)
	assert.Equal(t, []string{"emoji"}, declaration.InputSchema["required"])
	assert.Contains(t, schemaProperties(t, declaration), "message_id")
}

func TestMessageReactionToolUsesCurrentChannel(t *testing.T) {
	for _, test := range []struct {
		name, wantMessageID string
		args                map[string]any
	}{
		{name: "current message", wantMessageID: "current", args: map[string]any{"emoji": "👍"}},
		{name: "prior message", wantMessageID: "prior", args: map[string]any{"emoji": "✅", "message_id": "prior"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var channelID, messageID, emoji string
			processor := &Processor{client: &fakeClient{addReaction: func(_ context.Context, channel, message, reaction string) error {
				channelID, messageID, emoji = channel, message, reaction
				return nil
			}}}
			output, err := processor.reactToMessage("channel", "current").Execute(context.Background(), test.args)
			require.NoError(t, err)
			assert.Equal(t, "channel", channelID)
			assert.Equal(t, test.wantMessageID, messageID)
			assert.Equal(t, test.args["emoji"], emoji)
			assert.Equal(t, messageReactionResponse{MessageID: test.wantMessageID, Emoji: emoji}, output)
		})
	}
}

func TestMessageReactionToolValidatesArguments(t *testing.T) {
	tool := (&Processor{client: &fakeClient{}}).reactToMessage("channel", "current")
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{name: "missing emoji", args: map[string]any{}},
		{name: "empty emoji", args: map[string]any{"emoji": "  "}},
		{name: "surrounding whitespace", args: map[string]any{"emoji": " 👍 "}},
		{name: "emoji whitespace", args: map[string]any{"emoji": "👍 ✅"}},
		{name: "custom emoji", args: map[string]any{"emoji": "party:123"}},
		{name: "processing emoji", args: map[string]any{"emoji": processingReaction}},
		{name: "invalid message ID", args: map[string]any{"emoji": "👍", "message_id": 123}},
		{name: "empty message ID", args: map[string]any{"emoji": "👍", "message_id": " "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), test.args)
			assert.Error(t, err)
		})
	}
}

func TestMessageReactionToolPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	processor := &Processor{client: &fakeClient{addReaction: func(got context.Context, _, _, _ string) error {
		assert.ErrorIs(t, got.Err(), context.Canceled)
		return got.Err()
	}}}
	_, err := processor.reactToMessage("channel", "current").Execute(ctx, map[string]any{"emoji": "👍"})
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "add Discord message reaction")
}
