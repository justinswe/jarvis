package usage

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valkey-io/valkey-go"
)

// liveClient dials the configured Valkey server, skipping when none is configured.
//
// This is the only test that actually executes the Lua. Unit tests fake the commander,
// so script behavior and the documented key layout are verified only here.
func liveClient(t *testing.T, tiers map[string]Limits, defaultTier string) (*Client, valkey.Client, string) {
	t.Helper()
	if manualTestOptions.valkeyAddress == "" {
		t.Skip("set --valkey-address (or VALKEY_ADDRESS) to run live Valkey tests")
	}
	prefix := "jarvis-live-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	connection := Connection{
		Addresses:   []string{manualTestOptions.valkeyAddress},
		Username:    manualTestOptions.valkeyUsername,
		Password:    manualTestOptions.valkeyPassword,
		DialTimeout: 5 * time.Second,
	}
	cfg := Config{
		KeyPrefix: prefix, Timeout: 2 * time.Second, FlushInterval: time.Hour,
		RequestTTL: time.Hour, TokenTTL: time.Hour,
		WarnThreshold: 95, Tiers: tiers, DefaultTier: defaultTier,
	}
	client, err := New(context.Background(), connection, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	raw, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:  []string{manualTestOptions.valkeyAddress},
		Username:     manualTestOptions.valkeyUsername,
		Password:     manualTestOptions.valkeyPassword,
		DisableCache: true,
	})
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	return client, raw, prefix
}

func TestLiveGCRAAdmitsTheBurstThenThrottles(t *testing.T) {
	const burst = 3
	client, _, _ := liveClient(t, map[string]Limits{
		"test": {RequestsPerSecond: 1, Burst: burst, TokensPerHour: 0},
	}, "test")

	for attempt := 0; attempt < burst; attempt++ {
		admission, err := client.Allow(context.Background(), "guild-burst", "test")
		require.NoError(t, err)
		assert.True(t, admission.Allowed, "request %d of the burst must be admitted", attempt+1)
	}

	denied, err := client.Allow(context.Background(), "guild-burst", "test")
	require.NoError(t, err)
	assert.False(t, denied.Allowed, "the request after the burst must be denied")
	assert.Positive(t, denied.RetryAfter)
	assert.LessOrEqual(t, denied.RetryAfter, time.Second)

	time.Sleep(denied.RetryAfter + 100*time.Millisecond)
	recovered, err := client.Allow(context.Background(), "guild-burst", "test")
	require.NoError(t, err)
	assert.True(t, recovered.Allowed, "the limiter must recover after retry_after elapses")
}

func TestLiveUtilizationCrossesTheWarningThreshold(t *testing.T) {
	client, _, _ := liveClient(t, map[string]Limits{
		"test": {RequestsPerSecond: 1, Burst: 20, TokensPerHour: 0},
	}, "test")

	var sawNearLimit bool
	for attempt := 0; attempt < 20; attempt++ {
		admission, err := client.Allow(context.Background(), "guild-util", "test")
		require.NoError(t, err)
		if !admission.Allowed {
			break
		}
		sawNearLimit = sawNearLimit || admission.NearLimit
	}
	assert.True(t, sawNearLimit, "consuming the burst must cross the near-limit threshold")
}

func TestLiveTokenBudgetDeniesOnceExhausted(t *testing.T) {
	client, _, _ := liveClient(t, map[string]Limits{
		"test": {RequestsPerSecond: 0, Burst: 0, TokensPerHour: 100},
	}, "test")

	admitted, err := client.Allow(context.Background(), "guild-tokens", "test")
	require.NoError(t, err)
	require.True(t, admitted.Allowed)

	client.RecordUsage(genai.UsageReport{
		GuildID: "guild-tokens", Tier: "test", Provider: "vertex", ModelID: "gemini", Calls: 1,
		Usage: llm.Usage{InputTokens: 80, OutputTokens: 70, TotalTokens: 150},
	})
	client.flush(context.Background())

	denied, err := client.Allow(context.Background(), "guild-tokens", "test")
	require.NoError(t, err)
	assert.False(t, denied.Allowed, "an exhausted hourly token budget must deny")
	assert.Positive(t, denied.RetryAfter)
}

// The recorded keys and fields must match docs/valkey.md exactly. This is the
// cross-check that the Lua and the Go key builders agree.
func TestLiveKeysMatchTheDocumentedSchema(t *testing.T) {
	client, raw, prefix := liveClient(t, map[string]Limits{
		"test": {RequestsPerSecond: 1, Burst: 5, TokensPerHour: 1000000},
	}, "test")
	ctx := context.Background()

	_, err := client.Allow(ctx, "guild-schema", "test")
	require.NoError(t, err)
	client.RecordUsage(genai.UsageReport{
		GuildID: "guild-schema", Tier: "test", Provider: "vertex", ModelID: "gemini", Calls: 1,
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2, TotalTokens: 16},
	})
	client.flush(ctx)

	now := time.Now().Unix()
	base := guildBase(prefix, "guild-schema")

	requests, err := raw.Do(ctx, raw.B().Hgetall().Key(requestKey(base, now/60)).Build()).AsStrMap()
	require.NoError(t, err)
	assert.Equal(t, "test", requests[tierField])
	assert.Equal(t, "1", requests[strconv.FormatInt(now%60, 10)])

	tokens, err := raw.Do(ctx, raw.B().Hgetall().Key(tokenKey(base, now/3600)).Build()).AsStrMap()
	require.NoError(t, err)
	assert.Equal(t, "test", tokens[tierField])
	assert.Equal(t, "10", tokens[usageField("vertex/gemini", metricInput)])
	assert.Equal(t, "4", tokens[usageField("vertex/gemini", metricOutput)])
	assert.Equal(t, "2", tokens[usageField("vertex/gemini", metricReasoning)])
	assert.Equal(t, "16", tokens[usageField("vertex/gemini", metricTotal)])
	assert.Equal(t, "1", tokens[usageField("vertex/gemini", metricCalls)])
	assert.Equal(t, "16", tokens[usageField(aggregateModel, metricTotal)],
		"the cross-model aggregate backs the token budget check")

	members, err := raw.Do(ctx, raw.B().Smembers().Key(guildIndexKey(prefix, now/86400)).Build()).AsStrSlice()
	require.NoError(t, err)
	assert.Contains(t, members, "guild-schema")

	for _, key := range []string{requestKey(base, now/60), tokenKey(base, now/3600), guildIndexKey(prefix, now/86400)} {
		ttl, err := raw.Do(ctx, raw.B().Ttl().Key(key).Build()).AsInt64()
		require.NoError(t, err)
		assert.Positive(t, ttl, "key %q must expire", key)
	}
}

func TestLiveDeniedRequestsAreRecordedSeparately(t *testing.T) {
	client, raw, prefix := liveClient(t, map[string]Limits{
		"test": {RequestsPerSecond: 1, Burst: 1, TokensPerHour: 0},
	}, "test")
	ctx := context.Background()

	_, err := client.Allow(ctx, "guild-denied", "test")
	require.NoError(t, err)
	denied, err := client.Allow(ctx, "guild-denied", "test")
	require.NoError(t, err)
	require.False(t, denied.Allowed)

	now := time.Now().Unix()
	base := guildBase(prefix, "guild-denied")
	denials, err := raw.Do(ctx, raw.B().Hgetall().Key(deniedKey(base, now/60)).Build()).AsStrMap()
	require.NoError(t, err)
	assert.Equal(t, "1", denials[strconv.FormatInt(now%60, 10)],
		"offered load must be recoverable from the denial hash")
}

// Lua.Exec recovers from a flushed script cache by falling back to EVAL.
func TestLiveScriptSurvivesScriptFlush(t *testing.T) {
	client, raw, _ := liveClient(t, nil, "")
	ctx := context.Background()

	_, err := client.Allow(ctx, "guild-flush", "")
	require.NoError(t, err)
	require.NoError(t, raw.Do(ctx, raw.B().ScriptFlush().Build()).Error())

	admission, err := client.Allow(ctx, "guild-flush", "")
	require.NoError(t, err)
	assert.True(t, admission.Allowed)
}
