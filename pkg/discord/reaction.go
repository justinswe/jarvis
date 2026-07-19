package discord

import (
	"context"
	"strings"
	"unicode"

	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/jarvis/pkg/llm"
	"github.com/justinswe/std/errors"
)

const (
	messageReactionToolName = "add_message_reaction"
	processingReaction      = "🤔"
)

type messageReactionTool struct {
	processor                   *Processor
	channelID, currentMessageID string
}

type messageReactionResponse struct {
	MessageID string `json:"messageId"`
	Emoji     string `json:"emoji"`
}

func (p *Processor) reactToMessage(channelID, currentMessageID string) genai.FunctionTool {
	return messageReactionTool{processor: p, channelID: channelID, currentMessageID: currentMessageID}
}

func (messageReactionTool) Name() string { return messageReactionToolName }

func (messageReactionTool) Declaration() *llm.ToolDefinition {
	return &llm.ToolDefinition{
		Name: messageReactionToolName,
		Description: "Add a Unicode emoji reaction to the current Discord message or another message in the current channel. " +
			"Use this when a lightweight reaction improves the interaction, but not instead of a substantive answer when one is needed. " +
			"Omit message_id to react to the current request; use only message IDs provided in the conversation or by search_current_channel.",
		InputSchema: llm.JSONSchema{"type": "object", "properties": map[string]any{
			"emoji":      map[string]any{"type": "string", "description": "One standard Unicode emoji. Custom Discord emoji and the reserved processing emoji are not supported."},
			"message_id": map[string]any{"type": "string", "description": "Optional message ID from the current channel. Omit this to react to the current request."},
		}, "required": []string{"emoji"}},
		Effect: llm.ToolEffectMutation,
	}
}

func (t messageReactionTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	emoji, ok := args["emoji"].(string)
	if !ok {
		return nil, errors.New("emoji must be a string")
	}
	if strings.TrimSpace(emoji) == "" {
		return nil, errors.New("emoji is required")
	}
	if strings.IndexFunc(emoji, unicode.IsSpace) >= 0 {
		return nil, errors.New("emoji must not contain whitespace")
	}
	if strings.Contains(emoji, ":") {
		return nil, errors.New("custom Discord emoji are not supported")
	}
	if emoji == processingReaction {
		return nil, errors.New("the processing emoji is reserved")
	}

	messageID := t.currentMessageID
	if value, exists := args["message_id"]; exists {
		var ok bool
		messageID, ok = value.(string)
		if !ok {
			return nil, errors.New("message_id must be a string")
		}
		messageID = strings.TrimSpace(messageID)
		if messageID == "" {
			return nil, errors.New("message_id must not be empty")
		}
	}
	if t.channelID == "" || messageID == "" {
		return nil, errors.New("message reaction is unavailable")
	}
	if err := t.processor.client.AddReaction(ctx, t.channelID, messageID, emoji); err != nil {
		return nil, errors.Wrap(err, "add Discord message reaction")
	}
	return messageReactionResponse{MessageID: messageID, Emoji: emoji}, nil
}
