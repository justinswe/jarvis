package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCommander struct {
	mu          sync.Mutex
	allowReply  []int64
	allowErr    error
	allowArgs   [][]string
	allowKeys   []string
	flushed     [][]guildBatch
	flushErr    error
	indexedKeys []string
	closed      bool
}

func (c *fakeCommander) allow(ctx context.Context, slotKey string, args []string) ([]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allowKeys = append(c.allowKeys, slotKey)
	c.allowArgs = append(c.allowArgs, args)
	if c.allowErr != nil {
		return nil, c.allowErr
	}
	return c.allowReply, nil
}

func (c *fakeCommander) flush(_ context.Context, pending []guildBatch, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushed = append(c.flushed, pending)
	return c.flushErr
}

func (c *fakeCommander) indexGuilds(_ context.Context, indexKey string, _ []string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.indexedKeys = append(c.indexedKeys, indexKey)
	return nil
}

func (c *fakeCommander) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

// flushes returns a copy of the recorded flush batches.
func (c *fakeCommander) flushes() [][]guildBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]guildBatch(nil), c.flushed...)
}

// testConfig returns a valid configuration with a slow flush so tests drive it explicitly.
func testConfig() Config {
	return Config{
		KeyPrefix: "jarvis",
		Timeout:   50 * time.Millisecond, FlushInterval: time.Hour,
		RequestTTL: 48 * time.Hour, TokenTTL: 720 * time.Hour,
		WarnThreshold: 95,
		DefaultTier:   "free",
		Tiers:         map[string]Limits{"free": {RequestsPerSecond: 1, Burst: 2, TokensPerHour: 1000}},
	}
}

func newTestClient(t *testing.T, cfg Config, commands commander) *Client {
	t.Helper()
	normalized, err := cfg.normalized()
	require.NoError(t, err)
	client := newClient(commands, normalized)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestAllowFailsOpenOnCommanderError(t *testing.T) {
	commands := &fakeCommander{allowErr: errors.New("valkey unavailable")}
	client := newTestClient(t, testConfig(), commands)

	admission, err := client.Allow(context.Background(), "g1", "free")
	require.Error(t, err)
	assert.True(t, admission.Allowed, "an unavailable limiter must not block requests")
	assert.False(t, admission.NearLimit)
}

func TestAllowFailsOpenOnCancelledContext(t *testing.T) {
	commands := &fakeCommander{allowErr: context.DeadlineExceeded}
	client := newTestClient(t, testConfig(), commands)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	admission, err := client.Allow(ctx, "g1", "free")
	require.Error(t, err)
	assert.True(t, admission.Allowed)
}

func TestAllowFailsOpenOnMalformedReply(t *testing.T) {
	commands := &fakeCommander{allowReply: []int64{1, 0}}
	client := newTestClient(t, testConfig(), commands)

	admission, err := client.Allow(context.Background(), "g1", "free")
	require.Error(t, err)
	assert.True(t, admission.Allowed)
}

func TestAllowDecodesDenialAndRetryAfter(t *testing.T) {
	commands := &fakeCommander{allowReply: []int64{0, 1500, denyKindRate, 100}}
	client := newTestClient(t, testConfig(), commands)

	admission, err := client.Allow(context.Background(), "g1", "free")
	require.NoError(t, err)
	assert.False(t, admission.Allowed)
	assert.Equal(t, 1500*time.Millisecond, admission.RetryAfter)
	assert.Equal(t, "jarvis:v1:g:{g1}:gcra", commands.allowKeys[0])
}

func TestAllowReportsNearLimitAtTheThreshold(t *testing.T) {
	tests := []struct {
		name          string
		utilization   int64
		wantNearLimit bool
	}{
		{name: "below threshold", utilization: 94, wantNearLimit: false},
		{name: "at threshold", utilization: 95, wantNearLimit: true},
		{name: "above threshold", utilization: 99, wantNearLimit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commands := &fakeCommander{allowReply: []int64{1, 0, denyKindNone, test.utilization}}
			client := newTestClient(t, testConfig(), commands)

			admission, err := client.Allow(context.Background(), "g1", "free")
			require.NoError(t, err)
			assert.True(t, admission.Allowed)
			assert.Equal(t, test.wantNearLimit, admission.NearLimit)
		})
	}
}

func TestAllowSkipsDirectMessages(t *testing.T) {
	commands := &fakeCommander{allowReply: []int64{0, 100, denyKindRate, 100}}
	client := newTestClient(t, testConfig(), commands)

	admission, err := client.Allow(context.Background(), "", "free")
	require.NoError(t, err)
	assert.True(t, admission.Allowed)
	assert.Empty(t, commands.allowKeys, "a direct message has no guild to limit")
}

func TestAllowSubstitutesTheDefaultTierForUnknownTiers(t *testing.T) {
	commands := &fakeCommander{allowReply: []int64{1, 0, denyKindNone, 0}}
	client := newTestClient(t, testConfig(), commands)

	_, err := client.Allow(context.Background(), "g1", "retired-tier")
	require.NoError(t, err)
	require.Len(t, commands.allowArgs, 1)
	assert.Equal(t, "free", commands.allowArgs[0][1])
	assert.Equal(t, "1", commands.allowArgs[0][2], "unknown tiers inherit the default tier limits")
}

func TestAllowLeavesLimitsUnsetWithoutATierTable(t *testing.T) {
	cfg := testConfig()
	cfg.Tiers, cfg.DefaultTier = nil, ""
	commands := &fakeCommander{allowReply: []int64{1, 0, denyKindNone, 0}}
	client := newTestClient(t, cfg, commands)

	_, err := client.Allow(context.Background(), "g1", "anything")
	require.NoError(t, err)
	require.Len(t, commands.allowArgs, 1)
	assert.Equal(t, "0", commands.allowArgs[0][2], "meter-only mode must not enforce a rate")
	assert.Equal(t, "0", commands.allowArgs[0][4], "meter-only mode must not enforce a token budget")
}

func TestRecordUsageAggregatesUntilFlush(t *testing.T) {
	commands := &fakeCommander{}
	client := newTestClient(t, testConfig(), commands)

	for round := 0; round < 3; round++ {
		client.RecordUsage(genai.UsageReport{
			GuildID: "g1", Tier: "free", Provider: "vertex", ModelID: "gemini", Calls: 1,
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}
	assert.Empty(t, commands.flushes(), "metering must not touch Valkey on the request path")

	client.flush(context.Background())
	flushed := commands.flushes()
	require.Len(t, flushed, 1)
	require.Len(t, flushed[0], 1)

	batch := flushed[0][0]
	assert.Equal(t, "jarvis:v1:g:{g1}", batch.base)
	assert.Equal(t, "free", batch.tier)
	assert.Contains(t, batch.args, "vertex/gemini")
	assert.Contains(t, batch.args, "30", "three rounds of ten input tokens")
	assert.Equal(t, []string{guildIndexKey("jarvis", time.Now().UTC().Unix()/86400)}, commands.indexedKeys)
}

func TestRecordUsageIgnoresReportsWithoutAGuild(t *testing.T) {
	commands := &fakeCommander{}
	client := newTestClient(t, testConfig(), commands)

	client.RecordUsage(genai.UsageReport{Provider: "vertex", ModelID: "gemini", Calls: 1})
	client.flush(context.Background())
	assert.Empty(t, commands.flushes())
}

func TestCloseFlushesPendingUsageAndReleasesTheClient(t *testing.T) {
	commands := &fakeCommander{}
	normalized, err := testConfig().normalized()
	require.NoError(t, err)
	client := newClient(commands, normalized)

	client.RecordUsage(genai.UsageReport{
		GuildID: "g1", Tier: "free", Provider: "vertex", ModelID: "gemini", Calls: 1,
		Usage: llm.Usage{TotalTokens: 9},
	})
	require.NoError(t, client.Close())

	assert.Len(t, commands.flushes(), 1, "shutdown must not discard accumulated usage")
	assert.True(t, commands.closed)
	require.NoError(t, client.Close(), "Close must be idempotent")
}

func TestConfigRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "non-positive timeout", mutate: func(c *Config) { c.Timeout = 0 }},
		{name: "non-positive flush interval", mutate: func(c *Config) { c.FlushInterval = 0 }},
		{name: "non-positive retention", mutate: func(c *Config) { c.TokenTTL = 0 }},
		{name: "threshold out of range", mutate: func(c *Config) { c.WarnThreshold = 101 }},
		{name: "default tier undefined", mutate: func(c *Config) { c.DefaultTier = "missing" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			test.mutate(&cfg)
			_, err := cfg.normalized()
			assert.Error(t, err)
		})
	}
}

// TestNewValidatesTheConnection covers what moved out of Config: connection settings are
// New's business, and both failures are caught before anything dials.
func TestNewValidatesTheConnection(t *testing.T) {
	for name, connection := range map[string]Connection{
		"no addresses":          {DialTimeout: time.Second},
		"blank addresses":       {Addresses: []string{"  "}, DialTimeout: time.Second},
		"no dial timeout":       {Addresses: []string{"127.0.0.1:6379"}},
		"negative dial timeout": {Addresses: []string{"127.0.0.1:6379"}, DialTimeout: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(context.Background(), connection, testConfig())
			assert.Error(t, err)
		})
	}
}

// TestNewWithClientNeedsNoConnectionSettings is the regression test for a borrowed client:
// the caller has already dialed, so requiring an address here would demand a field this
// package then ignores.
func TestNewWithClientNeedsNoConnectionSettings(t *testing.T) {
	client, err := NewWithClient(nil, testConfig())

	require.NoError(t, err)
	require.NoError(t, client.Close())
}

func TestConfigAppliesTheDefaultKeyPrefix(t *testing.T) {
	cfg := testConfig()
	cfg.KeyPrefix = "  "
	normalized, err := cfg.normalized()
	require.NoError(t, err)
	assert.Equal(t, defaultKeyPrefix, normalized.KeyPrefix)
}
