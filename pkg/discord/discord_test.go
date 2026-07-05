package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeGenerator struct {
	response genai.GenerateResponse
	err      error
}

func (f fakeGenerator) Generate(context.Context, genai.GenerateRequest) (genai.GenerateResponse, error) {
	return f.response, f.err
}

func message(id, content string) *discordgo.Message {
	return &discordgo.Message{ID: id, Content: content, Author: &discordgo.User{ID: "u", Username: "alice"}}
}

func TestBuildContextPrunesParentThenOldestAndKeepsCurrent(t *testing.T) {
	sections := []contextSection{
		{"THREAD HISTORY", []*discordgo.Message{message("1", strings.Repeat("old", 20)), message("2", "new thread")}},
		{"PARENT CHANNEL", []*discordgo.Message{message("3", strings.Repeat("parent", 20))}},
	}
	got := buildContext(sections, "must stay", 100)
	assert.NotContains(t, got, "parentparent")
	assert.NotContains(t, got, "oldold")
	assert.Contains(t, got, "new thread")
	assert.Contains(t, got, "CURRENT REQUEST:\nmust stay")
}

func TestBuildPromptUsesConfiguredHistoryLimits(t *testing.T) {
	var calls []int
	b := &Bot{botID: "bot", threadLimit: 12, parentLimit: 4, historyRunes: 6000, fetchMessages: func(_ string, limit int, _ string) ([]*discordgo.Message, error) {
		calls = append(calls, limit)
		return nil, nil
	}}
	m := message("m", "<@bot> question")
	m.ChannelID = "thread"
	_, err := b.buildPrompt(&discordgo.Channel{Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "parent"}, &discordgo.MessageCreate{Message: m})
	require.NoError(t, err)
	assert.Equal(t, []int{12, 4}, calls)
}

func TestAppendSources(t *testing.T) {
	got := appendSources("answer", []genai.Source{{Title: "One", URL: "https://one"}, {Title: "Two", URL: "https://two"}, {Title: "Three", URL: "https://three"}, {Title: "Four", URL: "https://four"}})
	assert.Equal(t, "answer\n\nSources: [One](https://one) · [Two](https://two) · [Three](https://three)", got)
}

func TestReactionLifecycle(t *testing.T) {
	tests := []struct {
		name     string
		response genai.GenerateResponse
		genErr   error
		sendErr  error
	}{
		{"success", genai.GenerateResponse{Text: "ok"}, nil, nil},
		{"model failure", genai.GenerateResponse{}, errors.New("failed"), nil},
		{"empty output", genai.GenerateResponse{}, nil, nil},
		{"reply failure", genai.GenerateResponse{Text: "ok"}, nil, errors.New("send failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reactions []string
			b := &Bot{botID: "bot", generator: fakeGenerator{tt.response, tt.genErr}, channelLimit: 8, historyRunes: 6000,
				messageTimeout: defaultMessageTimeout, fetchMessages: func(string, int, string) ([]*discordgo.Message, error) { return nil, nil },
				addReaction:    func(_, _, emoji string) error { reactions = append(reactions, "+"+emoji); return nil },
				removeReaction: func(_, _, emoji, _ string) error { reactions = append(reactions, "-"+emoji); return nil },
				startThread:    func(_, _, _ string, _ int) (*discordgo.Channel, error) { return nil, errors.New("no thread") },
				sendMessage:    func(_, _ string) (*discordgo.Message, error) { return nil, tt.sendErr },
			}
			b.handleMessage(&discordgo.Channel{Type: discordgo.ChannelTypeGuildText}, &discordgo.MessageCreate{Message: message("m", "question")})
			assert.Equal(t, []string{"+🤔", "-🤔"}, reactions)
		})
	}
}

func TestReactionFailuresAreNonFatal(t *testing.T) {
	sent := false
	b := &Bot{botID: "bot", generator: fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}, channelLimit: 8, historyRunes: 6000, messageTimeout: defaultMessageTimeout,
		fetchMessages: func(string, int, string) ([]*discordgo.Message, error) { return nil, nil }, addReaction: func(string, string, string) error { return errors.New("no") }, removeReaction: func(string, string, string, string) error { return errors.New("no") },
		startThread: func(string, string, string, int) (*discordgo.Channel, error) { return nil, errors.New("no") }, sendMessage: func(string, string) (*discordgo.Message, error) { sent = true; return nil, nil }}
	b.handleMessage(&discordgo.Channel{Type: discordgo.ChannelTypeGuildText}, &discordgo.MessageCreate{Message: message("m", "question")})
	assert.True(t, sent)
}

func TestSplitMessageForDiscord(t *testing.T) {
	chunks := splitMessageForDiscord(strings.Repeat("a", 4500), 2000)
	require.Len(t, chunks, 3)
	for _, chunk := range chunks {
		assert.LessOrEqual(t, len([]rune(chunk)), 2000)
	}
}
func TestSanitizeAndPrefix(t *testing.T) {
	assert.Equal(t, "ask this channel", sanitizeContent("<@bot> ask <#123>", "bot"))
	assert.Equal(t, "hello", stripBotPrefix("Jarvis: hello"))
}
func TestTargetHelpers(t *testing.T) {
	assert.True(t, mentionsBot([]*discordgo.User{{ID: "bot"}}, "bot"))
	assert.True(t, isThreadChannel(&discordgo.Channel{Type: discordgo.ChannelTypeGuildPrivateThread}))
}

func TestSuppressedMessageDisablesLinkEmbeds(t *testing.T) {
	message := suppressedMessage("See https://example.com")
	assert.Equal(t, "See https://example.com", message.Content)
	assert.Equal(t, discordgo.MessageFlagsSuppressEmbeds, message.Flags)
}
