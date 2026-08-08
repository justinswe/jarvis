package discord

import (
	"context"
	"strings"
	"unicode"

	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/errors"
)

const (
	messageReactionToolName = "add_message_reaction"
	processingReaction      = "🤔"
	rateLimitedReaction     = "⏳"
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
			"emoji": map[string]any{"type": "string", "description": "The emoji character itself, such as 💯 or 👍. Translate a name the user gives into its character: \"100\" is 💯, \"thumbs up\" is 👍. Never send a :shortcode: name, a word, or digits. Custom Discord emoji and the reserved processing emoji are not supported."},
			"message_id": map[string]any{"type": "string", "description": "Optional message ID from the current channel. Omit this to react to the current request."},
		}, "required": []string{"emoji"}},
		Effect: llm.ToolEffectMutation,
	}
}

// Execute adds the reaction. Every rejection is a genai.ExecutionError, because the loop
// only forwards those to the model: a plain error becomes an opaque "tool execution failed",
// which leaves the model unable to perform the corrected retry the prompt asks of it.
func (t messageReactionTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	emoji, ok := args["emoji"].(string)
	if !ok {
		return nil, genai.NewExecutionError("invalid_emoji", "The emoji must be a string holding one emoji character.", nil)
	}
	if strings.TrimSpace(emoji) == "" {
		return nil, genai.NewExecutionError("invalid_emoji", "An emoji character is required.", nil)
	}
	if strings.IndexFunc(emoji, unicode.IsSpace) >= 0 {
		return nil, genai.NewExecutionError("invalid_emoji", "The emoji must be a single emoji character with no spaces.", nil)
	}
	// A name in colons is how a person writes an emoji in Discord, so the model copies it.
	// Naming the fix is what lets it recover; calling this a custom emoji did not.
	if strings.Contains(emoji, ":") {
		return nil, genai.NewExecutionError("invalid_emoji",
			"Send the emoji character itself, not a :name: shortcode. For example, use 💯 rather than :100:, or 👍 rather than :thumbsup:.", nil)
	}
	if !containsEmoji(emoji) {
		return nil, genai.NewExecutionError("invalid_emoji",
			"That is not an emoji character. Translate the name into its character and send that, such as 💯 for \"100\" or 👍 for \"thumbs up\".", nil)
	}
	if emoji == processingReaction || emoji == rateLimitedReaction {
		return nil, genai.NewExecutionError("reserved_emoji",
			"That emoji is reserved for Jarvis request status. Choose a different one.", nil)
	}

	messageID := t.currentMessageID
	if value, exists := args["message_id"]; exists {
		var ok bool
		messageID, ok = value.(string)
		if !ok {
			return nil, genai.NewExecutionError("invalid_message_id", "The message_id must be a string.", nil)
		}
		messageID = strings.TrimSpace(messageID)
		if messageID == "" {
			return nil, genai.NewExecutionError("invalid_message_id", "The message_id must not be empty. Omit it to react to the current request.", nil)
		}
	}
	if t.channelID == "" || messageID == "" {
		return nil, genai.NewExecutionError("reaction_unavailable", "Reactions are unavailable for this message.", nil)
	}
	if err := t.processor.client.AddReaction(ctx, t.channelID, messageID, emoji); err != nil {
		return nil, genai.NewExecutionError("reaction_failed",
			"Discord rejected the reaction. Confirm the emoji is one standard emoji character and that the message is in this channel.",
			errors.Wrap(err, "add Discord message reaction"))
	}
	return messageReactionResponse{MessageID: messageID, Emoji: emoji}, nil
}

// containsEmoji reports whether the value holds a pictographic character, catching a name
// or bare digits locally instead of spending a Discord call to learn the same thing.
//
// It is deliberately permissive: symbols cover BMP emoji such as ❤ and ✅, the astral range
// covers 💯 and flags, and marks cover the variation selector and enclosing keycap that make
// up 1️⃣. Rejecting a valid emoji would be worse than accepting an odd symbol Discord will
// refuse anyway.
func containsEmoji(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.S, r) || r >= 0x1F000 || unicode.Is(unicode.M, r) {
			return true
		}
	}
	return false
}
