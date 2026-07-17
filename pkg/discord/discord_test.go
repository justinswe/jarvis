package discord

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegenai "google.golang.org/genai"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type fakeGenerator struct {
	response genai.GenerateResponse
	err      error
	request  *genai.GenerateRequest
}

func (f *fakeGenerator) Generate(_ context.Context, request genai.GenerateRequest) (genai.GenerateResponse, error) {
	f.request = &request
	return f.response, f.err
}

type generatorFunc func(context.Context, genai.GenerateRequest) (genai.GenerateResponse, error)

func (f generatorFunc) Generate(ctx context.Context, request genai.GenerateRequest) (genai.GenerateResponse, error) {
	return f(ctx, request)
}

type fakeClient struct {
	channel        *discordgo.Channel
	referenced     *discordgo.Message
	messages       func(context.Context, string, int, string) ([]*discordgo.Message, error)
	sendMessage    func(context.Context, string, string) (*discordgo.Message, error)
	startThread    func(context.Context, string, string, string, int) (*discordgo.Channel, error)
	addReaction    func(context.Context, string, string, string) error
	removeReaction func(context.Context, string, string, string, string) error
	permissions    func(context.Context, string, string) (int64, error)
}

type fakeHistory struct {
	messages     []*discordgo.Message
	err          error
	messagesFunc func(context.Context, string, string, int, string) ([]*discordgo.Message, error)
	calls        int
}

func (h *fakeHistory) Messages(ctx context.Context, guildID, channelID string, limit int, before string) ([]*discordgo.Message, error) {
	h.calls++
	if h.messagesFunc != nil {
		return h.messagesFunc(ctx, guildID, channelID, limit, before)
	}
	return h.messages, h.err
}

func (f *fakeClient) Channel(context.Context, string) (*discordgo.Channel, error) {
	if f.channel == nil {
		return &discordgo.Channel{Type: discordgo.ChannelTypeGuildText}, nil
	}
	return f.channel, nil
}

func (f *fakeClient) Message(context.Context, string, string) (*discordgo.Message, error) {
	if f.referenced == nil {
		return nil, errors.New("not found")
	}
	return f.referenced, nil
}

func (f *fakeClient) Messages(ctx context.Context, channelID string, limit int, before string) ([]*discordgo.Message, error) {
	if f.messages == nil {
		return nil, nil
	}
	return f.messages(ctx, channelID, limit, before)
}

func (f *fakeClient) SendMessage(ctx context.Context, channelID, content string) (*discordgo.Message, error) {
	if f.sendMessage == nil {
		return &discordgo.Message{}, nil
	}
	return f.sendMessage(ctx, channelID, content)
}

func (f *fakeClient) StartThread(ctx context.Context, channelID, messageID, name string, duration int) (*discordgo.Channel, error) {
	if f.startThread == nil {
		return nil, errors.New("threads unavailable")
	}
	return f.startThread(ctx, channelID, messageID, name, duration)
}

func (f *fakeClient) AddReaction(ctx context.Context, channelID, messageID, emoji string) error {
	if f.addReaction == nil {
		return nil
	}
	return f.addReaction(ctx, channelID, messageID, emoji)
}

func (f *fakeClient) RemoveReaction(ctx context.Context, channelID, messageID, emoji, userID string) error {
	if f.removeReaction == nil {
		return nil
	}
	return f.removeReaction(ctx, channelID, messageID, emoji, userID)
}

func (f *fakeClient) UserChannelPermissions(ctx context.Context, userID, channelID string) (int64, error) {
	if f.permissions == nil {
		return 0, nil
	}
	return f.permissions(ctx, userID, channelID)
}

func testSettings() config.ServerSettings {
	return config.ServerSettings{
		Prompt:               "Jarvis",
		ThreadMessages:       12,
		ParentMessages:       4,
		ChannelMessages:      8,
		HistoryRunes:         6000,
		MaxOutputTokens:      256,
		MessageTimeout:       time.Minute,
		MessageRetentionDays: config.DefaultMessageRetentionDays,
		WebSearchEnabled:     true,
		ChannelSearchEnabled: true,
	}
}

func testProvider(t *testing.T) config.Provider {
	provider, err := config.NewStaticProvider(testSettings())
	require.NoError(t, err)
	return provider
}

func message(id, content string) *discordgo.Message {
	return &discordgo.Message{ID: id, Content: content, Type: discordgo.MessageTypeDefault, Author: &discordgo.User{ID: "u", Username: "alice"}}
}

func targetedMessage(id, content string) *discordgo.MessageCreate {
	m := message(id, "<@bot> "+content)
	m.ChannelID = "channel"
	m.GuildID = "guild"
	m.Mentions = []*discordgo.User{{ID: "bot"}}
	return &discordgo.MessageCreate{Message: m}
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

func TestFormatTranscriptIncludesUTCTimestampAndBotMarker(t *testing.T) {
	user := timedMessage("1", "hello", "2026-07-16T18:30:00-07:00")
	bot := timedMessage("2", "answer", "2026-07-17T01:31:00Z")
	bot.Author = &discordgo.User{ID: "bot", Username: "Jarvis", Bot: true}
	assert.Equal(t,
		"[2026-07-17T01:30:00Z] alice: hello\n[2026-07-17T01:31:00Z] Jarvis [bot]: answer",
		formatTranscript([]*discordgo.Message{user, bot}),
	)
}

func TestBuildPromptUsesConfiguredHistoryLimits(t *testing.T) {
	var calls []int
	client := &fakeClient{messages: func(_ context.Context, _ string, limit int, _ string) ([]*discordgo.Message, error) {
		calls = append(calls, limit)
		return nil, nil
	}}
	p := &Processor{botID: "bot", client: client}
	m := targetedMessage("m", "question")
	m.ChannelID = "thread"
	settings := testSettings()
	settings.ThreadMessages = 12
	settings.ParentMessages = 4
	_, err := p.buildPrompt(context.Background(), &discordgo.Channel{Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "parent"}, m, settings)
	require.NoError(t, err)
	assert.Equal(t, []int{12, 4}, calls)
}

func TestBuildPromptLoadsCurrentImageOnly(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}},
			Body: io.NopCloser(bytes.NewReader([]byte("png"))), Request: request}, nil
	})}
	processor := &Processor{botID: "bot", client: &fakeClient{}, imageClient: client}
	m := targetedMessage("m", "describe this")
	m.Attachments = []*discordgo.MessageAttachment{{Filename: "photo.png", ContentType: "image/png", Size: 3,
		URL: "https://cdn.discordapp.com/attachments/a/b/photo.png"}}
	messages, err := processor.buildPrompt(context.Background(), &discordgo.Channel{Type: discordgo.ChannelTypeGuildText}, m, testSettings())
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.NotNil(t, messages[0].Image)
	assert.Equal(t, []byte("png"), messages[0].Image.Data)
}

func TestBuildPromptContinuesWithSafeImageFailureNotice(t *testing.T) {
	processor := &Processor{botID: "bot", client: &fakeClient{}}
	m := targetedMessage("m", "")
	m.Attachments = []*discordgo.MessageAttachment{{Filename: "bad\nname.gif", ContentType: "image/gif",
		URL: "https://example.com/secret"}}
	messages, err := processor.buildPrompt(context.Background(), &discordgo.Channel{Type: discordgo.ChannelTypeGuildText}, m, testSettings())
	require.NoError(t, err)
	assert.Nil(t, messages[0].Image)
	assert.Contains(t, messages[0].Content, "IMAGE ATTACHMENT NOTICE: badname.gif")
	assert.Contains(t, messages[0].Content, "unsupported_format")
	assert.NotContains(t, messages[0].Content, "example.com")
}

func TestAllowedImageURL(t *testing.T) {
	for _, raw := range []string{"https://cdn.discordapp.com/a", "https://media.discordapp.net/a"} {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		require.NoError(t, err)
		assert.True(t, allowedImageURL(request.URL))
	}
	request, err := http.NewRequest(http.MethodGet, "https://cdn.discordapp.com.evil.test/a", nil)
	require.NoError(t, err)
	assert.False(t, allowedImageURL(request.URL))
}

func TestBuildPromptIncludesConfiguredParentChannelMessages(t *testing.T) {
	client := &fakeClient{messages: func(_ context.Context, channelID string, _ int, _ string) ([]*discordgo.Message, error) {
		switch channelID {
		case "thread":
			return []*discordgo.Message{message("t1", "thread context")}, nil
		case "parent":
			return []*discordgo.Message{message("p2", "new parent"), message("p1", "old parent")}, nil
		default:
			return nil, nil
		}
	}}
	p := &Processor{botID: "bot", client: client}
	m := targetedMessage("m", "question")
	m.ChannelID = "thread"
	settings := testSettings()
	settings.ThreadMessages = 2
	settings.ParentMessages = 2
	got, err := p.buildPrompt(context.Background(), &discordgo.Channel{Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "parent"}, m, settings)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Content, "PARENT CHANNEL:\n[timestamp unavailable] alice: old parent\n[timestamp unavailable] alice: new parent")
}

func TestBuildPromptUsesPartialDatabaseHistoryWithoutDiscordFallback(t *testing.T) {
	discordCalls := 0
	client := &fakeClient{messages: func(context.Context, string, int, string) ([]*discordgo.Message, error) {
		discordCalls++
		return nil, nil
	}}
	history := &fakeHistory{messages: []*discordgo.Message{message("1", "stored context")}, err: errors.New("partial decode")}
	processor := &Processor{botID: "bot", client: client, history: history}
	got, err := processor.buildPrompt(context.Background(), &discordgo.Channel{Type: discordgo.ChannelTypeGuildText}, targetedMessage("2", "question"), testSettings())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Content, incompleteContextNotice)
	assert.Contains(t, got[0].Content, "stored context")
	assert.Equal(t, 1, history.calls)
	assert.Zero(t, discordCalls)
}

func TestSearchCurrentChannelPagesStoredHistoryWithoutDiscordFallback(t *testing.T) {
	var calls []string
	history := &fakeHistory{messagesFunc: func(_ context.Context, guildID, channelID string, limit int, before string) ([]*discordgo.Message, error) {
		assert.Equal(t, "guild", guildID)
		assert.Equal(t, "channel", channelID)
		calls = append(calls, before)
		assert.Equal(t, channelSearchPageSize, limit)
		switch before {
		case "current":
			return []*discordgo.Message{timedMessage("4", "deploy now", "2026-07-08T12:04:00Z"), timedMessage("3", "unrelated", "2026-07-08T12:03:00Z")}, nil
		case "3":
			return []*discordgo.Message{timedMessage("2", "DEPLOY later", "2026-07-08T12:02:00Z"), timedMessage("1", "deploy old", "2026-07-08T12:01:00Z")}, nil
		default:
			return nil, nil
		}
	}}
	discordCalls := 0
	client := &fakeClient{messages: func(context.Context, string, int, string) ([]*discordgo.Message, error) {
		discordCalls++
		return nil, nil
	}}
	p := &Processor{client: client, history: history}
	got, err := p.searchChannel(context.Background(), "guild", "channel", "current", channelSearchCriteria{query: "deploy"})
	require.NoError(t, err)
	assert.Equal(t, []string{"current", "3", "1"}, calls)
	assert.Zero(t, discordCalls)
	assert.Equal(t, 4, got.SearchedMessages)
	assert.False(t, got.Truncated)
	assert.False(t, got.Incomplete)
	require.Len(t, got.Results, 3)
	assert.Equal(t, "1", got.Results[0].ID)
	assert.Equal(t, "4", got.Results[2].ID)
	assert.Equal(t, "u", got.Results[2].AuthorID)
	assert.Equal(t, "https://discord.com/channels/guild/channel/4", got.Results[2].URL)
}

func TestSearchCurrentChannelSanitizesAndAcceptsOptionalCriteria(t *testing.T) {
	history := &fakeHistory{messagesFunc: func(_ context.Context, _, _ string, _ int, before string) ([]*discordgo.Message, error) {
		if before != "current" {
			return nil, nil
		}
		return []*discordgo.Message{message("1", "<@bot> see <#123> &amp; deploy")}, nil
	}}
	p := &Processor{botID: "bot", history: history}
	got, err := p.searchChannel(context.Background(), "", "channel", "current", channelSearchCriteria{author: "<@u>"})
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	assert.Equal(t, "see this channel & deploy", got.Results[0].Content)
	assert.Equal(t, "https://discord.com/channels/@me/channel/1", got.Results[0].URL)
}

func TestSearchCurrentChannelAppliesAuthorAndTimeFilters(t *testing.T) {
	alice := timedMessage("3", "DEPLOY later", "2026-07-08T12:03:00Z")
	alice.Author = &discordgo.User{ID: "alice-id", Username: "alice", GlobalName: "Alice Smith"}
	tooOld := timedMessage("2", "deploy old", "2026-07-08T12:02:59Z")
	tooOld.Author = alice.Author
	bob := timedMessage("4", "deploy by Bob", "2026-07-08T12:04:00Z")
	bob.Author = &discordgo.User{ID: "bob-id", Username: "bob"}
	atEnd := timedMessage("5", "deploy at exclusive end", "2026-07-08T12:05:00Z")
	atEnd.Author = alice.Author
	history := &fakeHistory{messagesFunc: func(_ context.Context, _, _ string, _ int, before string) ([]*discordgo.Message, error) {
		if before == "current" {
			return []*discordgo.Message{atEnd, bob, alice, tooOld}, nil
		}
		return nil, nil
	}}
	start, err := time.Parse(time.RFC3339, "2026-07-08T12:03:00Z")
	require.NoError(t, err)
	end, err := time.Parse(time.RFC3339, "2026-07-08T12:05:00Z")
	require.NoError(t, err)
	p := &Processor{history: history}
	got, err := p.searchChannel(context.Background(), "guild", "channel", "current", channelSearchCriteria{
		query: "deploy", author: "Alice Smith", start: &start, end: &end,
	})
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	assert.Equal(t, "3", got.Results[0].ID)
	assert.Equal(t, "2026-07-08T12:03:00Z", got.StartTime)
	assert.Equal(t, "2026-07-08T12:05:00Z", got.EndTime)
}

func TestSearchCurrentChannelStopsAtNewestEightMatches(t *testing.T) {
	messages := []*discordgo.Message{
		message("9", "match"), message("8", "match"), message("7", "match"),
		message("6", "match"), message("5", "match"), message("4", "match"),
		message("3", "match"), message("2", "match"), message("1", "match"),
	}
	history := &fakeHistory{messagesFunc: func(_ context.Context, _, _ string, _ int, _ string) ([]*discordgo.Message, error) {
		return messages, nil
	}}
	p := &Processor{history: history}
	got, err := p.searchChannel(context.Background(), "guild", "channel", "current", channelSearchCriteria{query: "match"})
	require.NoError(t, err)
	assert.True(t, got.Truncated)
	assert.Equal(t, 8, got.SearchedMessages)
	require.Len(t, got.Results, 8)
	assert.Equal(t, "2", got.Results[0].ID)
	assert.Equal(t, "9", got.Results[7].ID)
	assert.Equal(t, 1, history.calls)
}

func TestSearchCurrentChannelReturnsUsablePartialResults(t *testing.T) {
	history := &fakeHistory{messages: []*discordgo.Message{message("1", "deploy")}, err: errors.New("partial decode")}
	p := &Processor{history: history}
	got, err := p.searchChannel(context.Background(), "guild", "channel", "current", channelSearchCriteria{query: "deploy"})
	require.NoError(t, err)
	assert.True(t, got.Incomplete)
	require.Len(t, got.Results, 1)

	p.history = &fakeHistory{err: errors.New("database down")}
	_, err = p.searchChannel(context.Background(), "guild", "channel", "current", channelSearchCriteria{query: "deploy"})
	var executionErr *genai.ExecutionError
	require.ErrorAs(t, err, &executionErr)
	assert.Equal(t, "channel_search_unavailable", executionErr.Code)
}

func TestParseChannelSearchCriteriaValidatesInputs(t *testing.T) {
	criteria, err := parseChannelSearchCriteria(map[string]any{
		"author": " alice ", "start_time": "2026-07-08T12:03:00-07:00", "end_time": "2026-07-08T20:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, "alice", criteria.author)
	assert.Equal(t, "2026-07-08T19:03:00Z", criteria.start.Format(time.RFC3339))

	for _, args := range []map[string]any{
		nil,
		{"query": 1},
		{"start_time": "not-a-time"},
		{"start_time": "2026-07-09T00:00:00Z", "end_time": "2026-07-08T00:00:00Z"},
		{"unknown": "value"},
	} {
		_, err := parseChannelSearchCriteria(args)
		assert.Error(t, err)
	}
}

func TestSearchCurrentChannelPassesCancellationToHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetched := false
	history := &fakeHistory{messagesFunc: func(got context.Context, _, _ string, _ int, _ string) ([]*discordgo.Message, error) {
		fetched = true
		return nil, got.Err()
	}}
	p := &Processor{history: history}
	_, err := p.searchChannel(ctx, "guild", "channel", "current", channelSearchCriteria{query: "deploy"})
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, fetched)
}

func TestProcessUsesPerServerSettings(t *testing.T) {
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	client := &fakeClient{}
	settings := testSettings()
	settings.GuildPrompt = "Use guild terminology."
	provider := &countingProvider{settings: settings}
	p := &Processor{botID: "bot", generator: generator, client: client, configs: provider, history: &fakeHistory{}, version: "v0.6.0"}
	require.NoError(t, p.Process(context.Background(), targetedMessage("m", "question")))
	assert.Equal(t, "guild", provider.guildID)
	require.NotNil(t, generator.request)
	require.NotNil(t, generator.request.Config)
	assert.Equal(t, "Jarvis\n\nGuild-specific instructions:\nUse guild terminology.", generator.request.Config.Prompt)
	assert.Equal(t, 256, generator.request.Config.MaxOutputTokens)
	assert.True(t, generator.request.Config.WebSearchEnabled)
	assert.Equal(t, googlegenai.ThinkingLevelMedium, generator.request.Config.ThinkingLevel)
	assert.Equal(t, genai.AccuracyPolicy{}, generator.request.Config.AccuracyPolicy)
	require.Len(t, generator.request.Tools, 3)
	assert.Equal(t, runtimeContextToolName, generator.request.Tools[0].Name())
	runtimeResult, err := generator.request.Tools[0].Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "v0.6.0", runtimeResult.(runtimeContextResponse).Version)
	assert.Equal(t, messageReactionToolName, generator.request.Tools[1].Name())
	assert.Equal(t, channelSearchToolName, generator.request.Tools[2].Name())
}

func TestProcessClassifiesOnlySanitizedCurrentRequest(t *testing.T) {
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	p := &Processor{botID: "bot", generator: generator, client: &fakeClient{}, configs: testProvider(t)}
	require.NoError(t, p.Process(context.Background(), targetedMessage("m", "What time is it?")))
	require.NotNil(t, generator.request)
	assert.Equal(t, genai.AccuracyPolicy{
		RequiredFunctionNames: []string{runtimeContextToolName}, RuntimeContextRelevant: true,
	}, generator.request.Config.AccuracyPolicy)
}

func TestProcessExposesRuntimeAndReactionToolsWhenChannelSearchIsDisabled(t *testing.T) {
	settings := testSettings()
	settings.ChannelSearchEnabled = false
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	p := &Processor{botID: "bot", generator: generator, client: &fakeClient{}, configs: &countingProvider{settings: settings}}
	require.NoError(t, p.Process(context.Background(), targetedMessage("m", "question")))
	require.Len(t, generator.request.Tools, 2)
	assert.Equal(t, runtimeContextToolName, generator.request.Tools[0].Name())
	assert.Equal(t, messageReactionToolName, generator.request.Tools[1].Name())
}

func TestProcessDoesNotExposeChannelSearchWithoutDynamoDBHistory(t *testing.T) {
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	p := &Processor{botID: "bot", generator: generator, client: &fakeClient{}, configs: testProvider(t)}
	require.NoError(t, p.Process(context.Background(), targetedMessage("m", "question")))
	require.Len(t, generator.request.Tools, 2)
	assert.Equal(t, runtimeContextToolName, generator.request.Tools[0].Name())
	assert.Equal(t, messageReactionToolName, generator.request.Tools[1].Name())
}

func TestProcessSkipsConfigurationForUntargetedMessages(t *testing.T) {
	provider := &countingProvider{settings: testSettings()}
	p := &Processor{botID: "bot", generator: &fakeGenerator{}, client: &fakeClient{}, configs: provider}
	m := message("m", "ordinary message")
	m.ChannelID = "channel"
	require.NoError(t, p.Process(context.Background(), &discordgo.MessageCreate{Message: m}))
	assert.Zero(t, provider.calls)
}

func TestProcessKeepsOnlyLatestOverlappingThreadRequest(t *testing.T) {
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	releaseFirst := make(chan struct{})
	latestRequest := make(chan genai.GenerateRequest, 1)
	generator := generatorFunc(func(ctx context.Context, request genai.GenerateRequest) (genai.GenerateResponse, error) {
		switch request.RequestID {
		case "first":
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			<-releaseFirst
			return genai.GenerateResponse{}, ctx.Err()
		case "latest":
			latestRequest <- request
			return genai.GenerateResponse{Text: "latest answer"}, nil
		default:
			t.Errorf("unexpected generation for %q", request.RequestID)
			return genai.GenerateResponse{}, nil
		}
	})

	var sent, added, removed []string
	var clientMu sync.Mutex
	client := &fakeClient{
		channel: &discordgo.Channel{Type: discordgo.ChannelTypeGuildPublicThread, ID: "thread", ParentID: "parent", OwnerID: "bot"},
		sendMessage: func(_ context.Context, channelID, content string) (*discordgo.Message, error) {
			clientMu.Lock()
			defer clientMu.Unlock()
			sent = append(sent, channelID+":"+content)
			return &discordgo.Message{}, nil
		},
		addReaction: func(_ context.Context, _, messageID, _ string) error {
			clientMu.Lock()
			defer clientMu.Unlock()
			added = append(added, messageID)
			return nil
		},
		removeReaction: func(_ context.Context, _, messageID, _, _ string) error {
			clientMu.Lock()
			defer clientMu.Unlock()
			removed = append(removed, messageID)
			return nil
		},
	}
	history := &fakeHistory{messagesFunc: func(_ context.Context, _, channelID string, _ int, before string) ([]*discordgo.Message, error) {
		if channelID == "thread" && before == "latest" {
			return []*discordgo.Message{message("middle", "second thought"), message("first", "first thought")}, nil
		}
		return nil, nil
	}}
	processor := &Processor{botID: "bot", generator: generator, client: client, configs: testProvider(t), history: history}

	first := targetedMessage("first", "first thought")
	first.ChannelID = "thread"
	middle := targetedMessage("middle", "second thought")
	middle.ChannelID = "thread"
	latest := targetedMessage("latest", "final question")
	latest.ChannelID = "thread"

	firstResult := processAsync(processor, first)
	requireReceive(t, firstStarted)
	middleResult := processAsync(processor, middle)
	requireReceive(t, firstCanceled)
	latestResult := processAsync(processor, latest)
	assert.NoError(t, requireReceive(t, middleResult))

	close(releaseFirst)
	assert.NoError(t, requireReceive(t, firstResult))
	request := requireReceive(t, latestRequest)
	assert.NoError(t, requireReceive(t, latestResult))
	require.Len(t, request.Messages, 1)
	assert.Contains(t, request.Messages[0].Content, "first thought")
	assert.Contains(t, request.Messages[0].Content, "second thought")
	assert.Contains(t, request.Messages[0].Content, "CURRENT REQUEST:\nfinal question")

	clientMu.Lock()
	defer clientMu.Unlock()
	assert.Equal(t, []string{"thread:latest answer"}, sent)
	assert.Equal(t, []string{"first", "latest"}, added)
	assert.Equal(t, []string{"first", "latest"}, removed)
}

func TestProcessDoesNotQueueNonThreadMessages(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	generator := generatorFunc(func(_ context.Context, request genai.GenerateRequest) (genai.GenerateResponse, error) {
		started <- request.RequestID
		<-release
		return genai.GenerateResponse{Text: "ok"}, nil
	})
	processor := &Processor{botID: "bot", generator: generator, client: &fakeClient{}, configs: testProvider(t)}

	firstResult := processAsync(processor, targetedMessage("first", "one"))
	secondResult := processAsync(processor, targetedMessage("second", "two"))
	got := []string{requireReceive(t, started), requireReceive(t, started)}
	assert.ElementsMatch(t, []string{"first", "second"}, got)

	close(release)
	assert.NoError(t, requireReceive(t, firstResult))
	assert.NoError(t, requireReceive(t, secondResult))
}

func processAsync(processor *Processor, message *discordgo.MessageCreate) <-chan error {
	result := make(chan error, 1)
	go func() { result <- processor.Process(context.Background(), message) }()
	return result
}

type countingProvider struct {
	settings config.ServerSettings
	calls    int
	guildID  string
}

func (p *countingProvider) Get(_ context.Context, guildID string) (config.GuildConfig, error) {
	p.calls++
	p.guildID = guildID
	return config.GuildConfig{Settings: p.settings}, nil
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
			client := &fakeClient{
				addReaction: func(context.Context, string, string, string) error {
					reactions = append(reactions, "+🤔")
					return nil
				},
				removeReaction: func(context.Context, string, string, string, string) error {
					reactions = append(reactions, "-🤔")
					return nil
				},
				sendMessage: func(context.Context, string, string) (*discordgo.Message, error) { return nil, tt.sendErr },
			}
			p := &Processor{botID: "bot", generator: &fakeGenerator{response: tt.response, err: tt.genErr}, client: client, configs: testProvider(t)}
			_ = p.Process(context.Background(), targetedMessage("m", "question"))
			assert.Equal(t, []string{"+🤔", "-🤔"}, reactions)
		})
	}
}

func TestReactionFailuresAreNonFatal(t *testing.T) {
	sent := false
	client := &fakeClient{
		addReaction:    func(context.Context, string, string, string) error { return errors.New("no") },
		removeReaction: func(context.Context, string, string, string, string) error { return errors.New("no") },
		sendMessage:    func(context.Context, string, string) (*discordgo.Message, error) { sent = true; return nil, nil },
	}
	p := &Processor{botID: "bot", generator: &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}, client: client, configs: testProvider(t)}
	require.NoError(t, p.Process(context.Background(), targetedMessage("m", "question")))
	assert.True(t, sent)
}

func TestReactionCleanupSurvivesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cleanupContextErr := context.Canceled
	cleanupHasDeadline := false
	client := &fakeClient{
		sendMessage: func(context.Context, string, string) (*discordgo.Message, error) {
			cancel()
			return &discordgo.Message{}, nil
		},
		removeReaction: func(cleanupCtx context.Context, _, _, _, _ string) error {
			cleanupContextErr = cleanupCtx.Err()
			_, cleanupHasDeadline = cleanupCtx.Deadline()
			return nil
		},
	}
	p := &Processor{botID: "bot", generator: &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}, client: client, configs: testProvider(t)}
	require.NoError(t, p.Process(ctx, targetedMessage("m", "question")))
	assert.NoError(t, cleanupContextErr)
	assert.True(t, cleanupHasDeadline)
}

func TestAppendSources(t *testing.T) {
	got := appendSources("answer", []genai.Source{
		{Title: "en.Wikipedia.org", Domain: "google.com", URL: "https://vertexaisearch.cloud.google.com/grounding-api-redirect/one"},
		{Title: "BBC article", Domain: "www.bbc.co.uk", URL: "https://two.example/article"},
		{URL: "https://updates.three.example.org/article"},
		{Domain: "four.example", URL: "https://four.example/article"},
	})
	assert.Equal(t, "answer\n\n-# Sources: [wikipedia.org](https://vertexaisearch.cloud.google.com/grounding-api-redirect/one) · [bbc.co.uk](https://two.example/article) · [example.org](https://updates.three.example.org/article)", got)
}

func TestAppendSourcesUsesTextTitleForGroundingRedirect(t *testing.T) {
	got := appendSources("answer", []genai.Source{{
		Title:  `Wikipedia [article]\guide`,
		Domain: "vertexaisearch.cloud.google.com",
		URL:    "https://vertexaisearch.cloud.google.com/grounding-api-redirect/one",
	}})
	assert.Equal(t, "answer\n\n-# Sources: [Wikipedia \\[article\\]\\\\guide](https://vertexaisearch.cloud.google.com/grounding-api-redirect/one)", got)
}

func TestAppendSourcesSkipsInvalidURLs(t *testing.T) {
	got := appendSources("answer", []genai.Source{
		{Domain: "ignored.example", URL: "ftp://ignored.example/article"},
		{Domain: "ignored.example", URL: "://invalid"},
		{URL: "https://192.0.2.1/article"},
		{URL: "https://localhost/article"},
	})
	assert.Equal(t, "answer\n\n-# Sources: [192.0.2.1](https://192.0.2.1/article) · [localhost](https://localhost/article)", got)
}

func TestAppendSourcesPreservesRepeatedDomains(t *testing.T) {
	got := appendSources("answer", []genai.Source{
		{Domain: "news.example.com", URL: "https://example.com/one"},
		{Domain: "www.example.com", URL: "https://example.com/two"},
	})
	assert.Equal(t, "answer\n\n-# Sources: [example.com](https://example.com/one) · [example.com](https://example.com/two)", got)
}

func TestAppendEvidenceUsesCompactNonWebFooter(t *testing.T) {
	got := appendEvidence("answer", []genai.Evidence{
		{Kind: genai.EvidenceKindRuntimeContext},
		{Kind: genai.EvidenceKindWeb},
		{Kind: genai.EvidenceKindChannelHistory},
		{Kind: genai.EvidenceKindRuntimeContext},
		{Kind: genai.EvidenceKindCodeExecution},
	})
	assert.Equal(t, "answer\n\n-# Evidence used: runtime context · channel history · code execution", got)
}

func TestProcessPersistsEvidenceFooterInDiscordReply(t *testing.T) {
	var sent string
	client := &fakeClient{sendMessage: func(_ context.Context, _ string, content string) (*discordgo.Message, error) {
		sent = content
		return &discordgo.Message{}, nil
	}}
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "18:30 UTC", Evidence: []genai.Evidence{
		{Kind: genai.EvidenceKindRuntimeContext}, {Kind: genai.EvidenceKindWeb},
	}}}
	p := &Processor{botID: "bot", generator: generator, client: client, configs: testProvider(t)}
	require.NoError(t, p.Process(context.Background(), targetedMessage("m", "What time is it?")))
	assert.Equal(t, "18:30 UTC\n\n-# Evidence used: runtime context", sent)
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
