package discord

import (
	"context"
	"strings"
	"testing"
	"time"

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
	b := &Bot{botID: "bot", threadLimit: 12, parentLimit: 4, historyRunes: 6000, fetchMessages: func(_ context.Context, _ string, limit int, _ string) ([]*discordgo.Message, error) {
		calls = append(calls, limit)
		return nil, nil
	}}
	m := message("m", "<@bot> question")
	m.ChannelID = "thread"
	_, err := b.buildPrompt(context.Background(), &discordgo.Channel{Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "parent"}, &discordgo.MessageCreate{Message: m})
	require.NoError(t, err)
	assert.Equal(t, []int{12, 4}, calls)
}

func TestBuildPromptIncludesConfiguredParentChannelMessages(t *testing.T) {
	b := &Bot{botID: "bot", threadLimit: 2, parentLimit: 2, historyRunes: 6000, fetchMessages: func(_ context.Context, channelID string, _ int, _ string) ([]*discordgo.Message, error) {
		switch channelID {
		case "thread":
			return []*discordgo.Message{message("t1", "thread context")}, nil
		case "parent":
			return []*discordgo.Message{message("p2", "new parent"), message("p1", "old parent")}, nil
		default:
			return nil, nil
		}
	}}
	m := message("m", "<@bot> question")
	m.ChannelID = "thread"
	got, err := b.buildPrompt(context.Background(), &discordgo.Channel{Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "parent"}, &discordgo.MessageCreate{Message: m})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Content, "PARENT CHANNEL:\nalice: old parent\nalice: new parent")
}

func TestSearchCurrentChannelScansBoundedRecentMessages(t *testing.T) {
	var calls []string
	b := &Bot{fetchMessages: func(_ context.Context, channelID string, limit int, before string) ([]*discordgo.Message, error) {
		assert.Equal(t, "channel", channelID)
		calls = append(calls, before)
		assert.LessOrEqual(t, limit, channelSearchMessages)
		switch before {
		case "current":
			return []*discordgo.Message{
				timedMessage("4", "deploy now", "2026-07-08T12:04:00Z"),
				timedMessage("3", "unrelated", "2026-07-08T12:03:00Z"),
			}, nil
		case "3":
			return []*discordgo.Message{
				timedMessage("2", "DEPLOY later", "2026-07-08T12:02:00Z"),
				timedMessage("1", "deploy old", "2026-07-08T12:01:00Z"),
			}, nil
		default:
			return nil, nil
		}
	}}
	got, err := b.searchChannel(context.Background(), "guild", "channel", "current", "deploy")
	require.NoError(t, err)
	assert.Equal(t, []string{"current", "3", "1"}, calls)
	assert.Equal(t, 4, got.SearchedMessages)
	assert.False(t, got.Truncated)
	require.Len(t, got.Results, 3)
	assert.Equal(t, "1", got.Results[0].ID)
	assert.Equal(t, "4", got.Results[2].ID)
	assert.Equal(t, "https://discord.com/channels/guild/channel/4", got.Results[2].URL)
	assert.Equal(t, "2026-07-08T12:04:00Z", got.Results[2].Timestamp)
}

func TestSearchCurrentChannelSanitizesAndRequiresQuery(t *testing.T) {
	b := &Bot{botID: "bot", fetchMessages: func(_ context.Context, _ string, _ int, before string) ([]*discordgo.Message, error) {
		if before != "current" {
			return nil, nil
		}
		return []*discordgo.Message{message("1", "<@bot> see <#123> &amp; deploy")}, nil
	}}
	got, err := b.searchChannel(context.Background(), "", "channel", "current", "deploy")
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	assert.Equal(t, "see this channel & deploy", got.Results[0].Content)
	assert.Equal(t, "https://discord.com/channels/@me/channel/1", got.Results[0].URL)

	_, err = b.searchChannel(context.Background(), "", "channel", "current", "")
	assert.ErrorContains(t, err, "query is required")
}

func TestSearchCurrentChannelPassesCancellationToFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetched := false
	b := &Bot{fetchMessages: func(got context.Context, _ string, _ int, _ string) ([]*discordgo.Message, error) {
		fetched = true
		return nil, got.Err()
	}}
	_, err := b.searchChannel(ctx, "guild", "channel", "current", "deploy")
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, fetched)
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
				messageTimeout: defaultMessageTimeout, fetchMessages: func(context.Context, string, int, string) ([]*discordgo.Message, error) { return nil, nil },
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
		fetchMessages: func(context.Context, string, int, string) ([]*discordgo.Message, error) { return nil, nil }, addReaction: func(string, string, string) error { return errors.New("no") }, removeReaction: func(string, string, string, string) error { return errors.New("no") },
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

func timedMessage(id, content, timestamp string) *discordgo.Message {
	m := message(id, content)
	parsed, _ := time.Parse(time.RFC3339, timestamp)
	m.Timestamp = parsed
	return m
}
