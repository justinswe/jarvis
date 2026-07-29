package discord

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tieredProvider serves one guild configuration carrying a subscription tier.
type tieredProvider struct {
	tier string
}

func (p *tieredProvider) Get(context.Context, string) (config.GuildConfig, error) {
	return config.GuildConfig{Settings: testSettings(), Tier: p.tier}, nil
}

type fakeLimiter struct {
	admission Admission
	err       error
	calls     int
	guildIDs  []string
	tiers     []string
}

func (l *fakeLimiter) Allow(_ context.Context, guildID, tier string) (Admission, error) {
	l.calls++
	l.guildIDs = append(l.guildIDs, guildID)
	l.tiers = append(l.tiers, tier)
	return l.admission, l.err
}

// recordingClient captures reactions and sent message content.
type recordingClient struct {
	mu        sync.Mutex
	reactions []string
	sent      []string
}

func (c *recordingClient) client() *fakeClient {
	return &fakeClient{
		addReaction: func(_ context.Context, _, _, emoji string) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.reactions = append(c.reactions, emoji)
			return nil
		},
		sendMessage: func(_ context.Context, _, content string) (*discordgo.Message, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.sent = append(c.sent, content)
			return &discordgo.Message{}, nil
		},
	}
}

// limitedProcessor builds a processor wired to one limiter and generator.
func limitedProcessor(limiter Limiter, generator *fakeGenerator, client Client) *Processor {
	return &Processor{
		botID: "bot", generator: generator, client: client,
		configs: &countingProvider{settings: testSettings()}, history: &fakeHistory{}, limiter: limiter,
	}
}

func TestProcessDropsRequestsDeniedByTheLimiter(t *testing.T) {
	recorder := &recordingClient{}
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	limiter := &fakeLimiter{admission: Admission{Allowed: false, RetryAfter: 2 * time.Second}}
	processor := limitedProcessor(limiter, generator, recorder.client())

	require.NoError(t, processor.Process(context.Background(), targetedMessage("m", "question")))

	assert.Nil(t, generator.request, "a denied request must never reach the model")
	assert.Equal(t, []string{rateLimitedReaction}, recorder.reactions,
		"the processing reaction must never be added for a denied request")
	assert.Empty(t, recorder.sent, "denial is reaction-only and must not post a reply")
}

func TestProcessAllowsRequestsWhenTheLimiterFails(t *testing.T) {
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	limiter := &fakeLimiter{err: errors.New("valkey unavailable")}
	processor := limitedProcessor(limiter, generator, &fakeClient{})

	require.NoError(t, processor.Process(context.Background(), targetedMessage("m", "question")))
	assert.NotNil(t, generator.request, "an unavailable limiter must fail open")
}

func TestProcessSkipsTheLimiterWhenUnconfigured(t *testing.T) {
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	processor := limitedProcessor(nil, generator, &fakeClient{})

	require.NoError(t, processor.Process(context.Background(), targetedMessage("m", "question")))
	assert.NotNil(t, generator.request)
}

func TestProcessSkipsTheLimiterForDirectMessages(t *testing.T) {
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	limiter := &fakeLimiter{admission: Admission{Allowed: false}}
	processor := limitedProcessor(limiter, generator, &fakeClient{})

	m := targetedMessage("m", "question")
	m.GuildID = ""
	require.NoError(t, processor.Process(context.Background(), m))

	assert.Zero(t, limiter.calls, "a direct message has no guild to limit")
	assert.NotNil(t, generator.request)
}

func TestProcessPassesTheGuildTierToTheLimiterAndModel(t *testing.T) {
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	limiter := &fakeLimiter{admission: Admission{Allowed: true}}
	processor := limitedProcessor(limiter, generator, &fakeClient{})
	processor.configs = &tieredProvider{tier: "pro"}

	require.NoError(t, processor.Process(context.Background(), targetedMessage("m", "question")))

	assert.Equal(t, []string{"guild"}, limiter.guildIDs)
	assert.Equal(t, []string{"pro"}, limiter.tiers)
	require.NotNil(t, generator.request)
	assert.Equal(t, "guild", generator.request.GuildID)
	assert.Equal(t, "pro", generator.request.Tier)
}

func TestProcessAppendsTheNearLimitNoticeToTheReply(t *testing.T) {
	recorder := &recordingClient{}
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "Here is the answer."}}
	limiter := &fakeLimiter{admission: Admission{Allowed: true, NearLimit: true}}
	processor := limitedProcessor(limiter, generator, recorder.client())

	require.NoError(t, processor.Process(context.Background(), targetedMessage("m", "question")))

	require.Len(t, recorder.sent, 1)
	reply := recorder.sent[0]
	assert.True(t, strings.HasPrefix(reply, "Here is the answer."))
	assert.True(t, strings.HasSuffix(reply, nearRateLimitSentence), "the notice must be the last thing in the reply")
	assert.Contains(t, reply, "\n-# ", "the notice must render as Discord subtext")
	assert.Equal(t, 1, strings.Count(reply, nearRateLimitSentence))
}

func TestProcessOmitsTheNearLimitNoticeBelowTheThreshold(t *testing.T) {
	recorder := &recordingClient{}
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "Here is the answer."}}
	limiter := &fakeLimiter{admission: Admission{Allowed: true, NearLimit: false}}
	processor := limitedProcessor(limiter, generator, recorder.client())

	require.NoError(t, processor.Process(context.Background(), targetedMessage("m", "question")))

	require.Len(t, recorder.sent, 1)
	assert.NotContains(t, recorder.sent[0], nearRateLimitSentence)
}

func TestAppendRateLimitWarning(t *testing.T) {
	assert.Equal(t, "answer", appendRateLimitWarning("answer", false))
	assert.Equal(t, "", appendRateLimitWarning("", true), "an empty reply gains no notice")
	assert.Equal(t, "   ", appendRateLimitWarning("   ", true))

	warned := appendRateLimitWarning("answer", true)
	assert.Equal(t, "answer\n\n-# "+rateLimitedReaction+" "+nearRateLimitSentence, warned)
}

// The near-limit notice must survive chunking as the tail of the final chunk.
func TestNearLimitNoticeSurvivesMessageChunking(t *testing.T) {
	long := strings.Repeat("sentence. ", 400)
	warned := appendRateLimitWarning(long, true)
	chunks := splitMessageForDiscord(warned, discordMessageMaxLength)

	require.Greater(t, len(chunks), 1, "this reply must actually be split")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(chunks[len(chunks)-1]), nearRateLimitSentence))
}

func TestReservedReactionsCannotBeUsedByTheModel(t *testing.T) {
	tool := messageReactionTool{processor: &Processor{botID: "bot", client: &fakeClient{}}, channelID: "c", currentMessageID: "m"}
	for _, emoji := range []string{processingReaction, rateLimitedReaction} {
		_, err := tool.Execute(context.Background(), map[string]any{"emoji": emoji})
		assert.Error(t, err, "emoji %q must be reserved for request status", emoji)
	}
}
