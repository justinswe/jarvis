package main

import (
	"context"
	"testing"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/jarvis/worker/pkg/usage"
	"github.com/justinswe/jarvis/worker/pkg/websearch"
	"github.com/justinswe/std/app"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerConfigDefaults(t *testing.T) {
	command := newRootCommand()
	assert.Nil(t, command.Flags().Lookup("temperature"))
	port, err := command.Flags().GetString("port")
	require.NoError(t, err)
	location, err := command.Flags().GetString("location")
	require.NoError(t, err)
	timeout, err := command.Flags().GetDuration("message-timeout")
	require.NoError(t, err)
	storeDriver, err := command.Flags().GetString("store-driver")
	require.NoError(t, err)
	postgresDSN, err := command.Flags().GetString("postgres-dsn")
	require.NoError(t, err)
	sqlitePath, err := command.Flags().GetString("sqlite-path")
	require.NoError(t, err)
	sweepInterval, err := command.Flags().GetDuration("store-sweep-interval")
	require.NoError(t, err)
	retention, err := command.Flags().GetInt("message-retention-days")
	require.NoError(t, err)
	maxOutputTokens, err := command.Flags().GetInt("max-output-tokens")
	require.NoError(t, err)
	reasoningEffort, err := command.Flags().GetString("reasoning-effort")
	require.NoError(t, err)
	defaultPrompt, err := command.Flags().GetString("default-prompt")
	require.NoError(t, err)
	defaultPromptFlag := command.Flags().Lookup("default-prompt")
	require.NotNil(t, defaultPromptFlag)
	assert.Equal(t, "8080", port)
	assert.Equal(t, "global", location)
	assert.Equal(t, time.Minute, timeout)
	assert.Equal(t, "none", storeDriver)
	assert.Empty(t, postgresDSN)
	assert.Empty(t, sqlitePath)
	assert.Equal(t, time.Hour, sweepInterval)
	assert.Nil(t, command.Flags().Lookup("dynamodb-enabled"))
	assert.Nil(t, command.Flags().Lookup("aws-role-arn"))
	assert.Equal(t, config.DefaultMessageRetentionDays, retention)
	assert.Equal(t, 2048, maxOutputTokens)
	assert.Equal(t, string(llm.ReasoningLow), reasoningEffort)
	assert.Empty(t, defaultPrompt)
	assert.Nil(t, command.Flags().Lookup("model-provider"))
	assert.Nil(t, command.Flags().Lookup("openrouter-model"))
	assert.Nil(t, command.Flags().Lookup("tool-model-profile"))
	assert.Nil(t, command.Flags().Lookup("text-only-model-profile"))
	assert.Nil(t, command.Flags().Lookup("web-search-model-profile"))
	googleAIKey := command.Flags().Lookup("google-ai-api-key")
	require.NotNil(t, googleAIKey)
	assert.Empty(t, googleAIKey.DefValue)
	assert.Contains(t, googleAIKey.Usage, "Google AI Studio")
	providers, err := command.Flags().GetStringSlice("web-search-providers")
	require.NoError(t, err)
	assert.Empty(t, providers)
	assert.Contains(t, defaultPromptFlag.Usage, "may define the assistant name and personality")
}

func TestWorkerMapsGoogleAIKeyEnvironmentToBoundFlag(t *testing.T) {
	t.Setenv("GOOGLE_AI_API_KEY", "bound-key")
	command := newRootCommand()
	command.SetArgs([]string{})
	var got string
	command.RunE = func(command *cobra.Command, _ []string) error {
		var err error
		got, err = command.Flags().GetString("google-ai-api-key")
		return err
	}
	require.NoError(t, app.RunCobraCommand(context.Background(), command))
	assert.Equal(t, "bound-key", got)
}

func TestWorkerDoesNotUseSDKGoogleKeyEnvironmentAliases(t *testing.T) {
	t.Setenv("GOOGLE_AI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "ignored-gemini-key")
	t.Setenv("GOOGLE_API_KEY", "ignored-google-key")
	command := newRootCommand()
	command.SetArgs([]string{})
	var got string
	command.RunE = func(command *cobra.Command, _ []string) error {
		var err error
		got, err = command.Flags().GetString("google-ai-api-key")
		return err
	}
	require.NoError(t, app.RunCobraCommand(context.Background(), command))
	assert.Empty(t, got)
}

func TestWorkerServerSettingsAreRequestScopedDefaults(t *testing.T) {
	cfg := workerConfig{messageTimeout: time.Minute, reasoningEffort: string(llm.ReasoningHigh)}
	settings := cfg.serverSettings()
	assert.True(t, settings.WebSearchEnabled)
	assert.True(t, settings.ChannelSearchEnabled)
	assert.Equal(t, cfg.messageTimeout, settings.MessageTimeout)
	assert.Equal(t, llm.ReasoningHigh, settings.ReasoningEffort)
}

func TestWorkerMapsReasoningEffortEnvironmentToBoundFlag(t *testing.T) {
	t.Setenv("REASONING_EFFORT", "low")
	command := newRootCommand()
	command.SetArgs([]string{})
	var got string
	command.RunE = func(command *cobra.Command, _ []string) error {
		var err error
		got, err = command.Flags().GetString("reasoning-effort")
		return err
	}
	require.NoError(t, app.RunCobraCommand(context.Background(), command))
	assert.Equal(t, "low", got)
}

func TestWorkerRejectsUnsupportedReasoningEffort(t *testing.T) {
	cfg := workerConfig{
		threadMessages: 1, parentMessages: 1, channelMessages: 1, historyRunes: 100,
		maxOutputTokens: 100, messageTimeout: time.Minute,
		messageRetentionDays: config.DefaultMessageRetentionDays, reasoningEffort: "extreme",
	}
	assert.ErrorContains(t, cfg.serverSettings().Validate(), "reasoning effort must be low, medium, or high")
}

func TestWorkerParsesCommaSeparatedAndRepeatedModelProfiles(t *testing.T) {
	command := newRootCommand()
	require.NoError(t, command.Flags().Parse([]string{
		"--model-profile=primary=openrouter:vendor/model,fallback=vertex:gemini",
		"--model-profile=backup=nvidia-nim:meta/model,studio=google-ai:gemini-3.1-flash-lite",
	}))
	profiles, err := command.Flags().GetStringSlice("model-profile")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"primary=openrouter:vendor/model",
		"fallback=vertex:gemini",
		"backup=nvidia-nim:meta/model",
		"studio=google-ai:gemini-3.1-flash-lite",
	}, profiles)
}

func TestWorkerParsesCommaSeparatedWebSearchProviders(t *testing.T) {
	command := newRootCommand()
	require.NoError(t, command.Flags().Parse([]string{"--web-search-providers=serper,tavily"}))
	providers, err := command.Flags().GetStringSlice("web-search-providers")
	require.NoError(t, err)
	assert.Equal(t, []string{"serper", "tavily"}, providers)
}

func TestWorkerValidatesSelectedWebSearchProviderKeysAndOrder(t *testing.T) {
	clients, err := (workerConfig{serperAPIKey: "ignored"}).webSearchClients()
	require.NoError(t, err)
	assert.Empty(t, clients, "unselected keys are ignored")

	clients, err = (workerConfig{
		webSearchProviders: []string{"serper", "firecrawl"},
		serperAPIKey:       "serper-key",
		firecrawlAPIKey:    "firecrawl-key",
	}).webSearchClients()
	require.NoError(t, err)
	require.Len(t, clients, 2)
	assert.Equal(t, websearch.ProviderSerper, clients[0].Provider())
	assert.Equal(t, websearch.ProviderFirecrawl, clients[1].Provider())

	for _, cfg := range []workerConfig{
		{webSearchProviders: []string{"serper"}},
		{webSearchProviders: []string{"tavily", "serper"}, serperAPIKey: "key", tavilyAPIKey: "key"},
		{webSearchProviders: []string{"tavily", "tavily"}, tavilyAPIKey: "key"},
		{webSearchProviders: []string{"unknown"}},
		{webSearchProviders: []string{"serper", "tavily", "firecrawl"}},
	} {
		_, err := cfg.webSearchClients()
		assert.Error(t, err)
	}
}

func TestWorkerAddress(t *testing.T) {
	for _, test := range []struct {
		host, port, want string
	}{
		{port: "8080", want: ":8080"},
		{host: "127.0.0.1", port: "8081", want: "127.0.0.1:8081"},
		{host: "::1", port: "8081", want: "[::1]:8081"},
	} {
		assert.Equal(t, test.want, (workerConfig{host: test.host, port: test.port}).address())
	}
}

func TestValidRootUserID(t *testing.T) {
	assert.True(t, validRootUserID("12345678901234567"))
	assert.True(t, validRootUserID(" 12345678901234567890 "))
	assert.False(t, validRootUserID("123"))
	assert.False(t, validRootUserID("1234567890123456x"))
}

// TestWorkerValidatesValkeyDurations covers the durations that fail quietly rather than
// loudly. A non-positive config-cache TTL does not disable caching: cache.Set omits the
// expiry entirely, so every guild configuration would be cached forever and a missed
// invalidation could never age out. Both must be rejected before anything dials.
func TestWorkerValidatesValkeyDurations(t *testing.T) {
	valid := func() workerConfig {
		cfg := newWorkerConfig()
		cfg.discordBotToken = "token"
		cfg.valkeyEnabled = true
		cfg.valkeyAddresses = []string{"127.0.0.1:6379"}
		return cfg
	}
	for name, test := range map[string]struct {
		mutate func(*workerConfig)
		want   string
	}{
		"zero config cache TTL":     {func(c *workerConfig) { c.valkeyConfigCacheTTL = 0 }, "configuration cache TTL must be positive"},
		"negative config cache TTL": {func(c *workerConfig) { c.valkeyConfigCacheTTL = -time.Second }, "configuration cache TTL must be positive"},
		"zero dial timeout":         {func(c *workerConfig) { c.valkeyDialTimeout = 0 }, "dial timeout must be positive"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			test.mutate(&cfg)
			assert.ErrorContains(t, cfg.validate(), test.want)
		})
	}
	assert.NoError(t, valid().validate(), "the unmodified configuration must be valid")
}

// TestWorkerLeavesValkeyDurationsAloneWhenDisabled keeps the checks scoped the way the
// Valkey-address check already is: they must not fail a deployment that never uses Valkey.
func TestWorkerLeavesValkeyDurationsAloneWhenDisabled(t *testing.T) {
	cfg := newWorkerConfig()
	cfg.discordBotToken = "token"
	cfg.valkeyConfigCacheTTL = 0
	cfg.valkeyDialTimeout = 0

	assert.NoError(t, cfg.validate())
}

func TestWorkerDialTimeoutDefaultCoversAHandshake(t *testing.T) {
	dialTimeout, err := newRootCommand().Flags().GetDuration("valkey-dial-timeout")
	require.NoError(t, err)
	requestTimeout, err := newRootCommand().Flags().GetDuration("valkey-timeout")
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, dialTimeout)
	assert.Greater(t, dialTimeout, requestTimeout, "startup must not inherit the request-path deadline")
}

func TestGuildTiersParsesTheDeclaredTable(t *testing.T) {
	cfg := workerConfig{
		guildTierLimits:  []string{"free=1:2:200000", "pro=10:20:20000000"},
		defaultGuildTier: "free",
	}
	tiers, err := cfg.guildTiers()
	require.NoError(t, err)
	assert.Equal(t, usage.Limits{RequestsPerSecond: 1, Burst: 2, TokensPerHour: 200000}, tiers["free"])
	assert.Equal(t, usage.Limits{RequestsPerSecond: 10, Burst: 20, TokensPerHour: 20000000}, tiers["pro"])
	assert.Equal(t, []string{"free", "pro"}, guildTierNames(tiers))
}

func TestGuildTiersAllowsAnEmptyMeterOnlyTable(t *testing.T) {
	for _, entries := range [][]string{nil, {""}} {
		cfg := workerConfig{guildTierLimits: entries, defaultGuildTier: "free"}
		tiers, err := cfg.guildTiers()
		require.NoError(t, err)
		assert.Empty(t, tiers, "an empty table means meter-only, not misconfiguration")
	}
}

func TestGuildTiersRejectsInvalidTables(t *testing.T) {
	tests := []struct {
		name     string
		entries  []string
		fallback string
	}{
		{name: "missing name separator", entries: []string{"1:2:3"}, fallback: "free"},
		{name: "wrong field count", entries: []string{"free=1:2"}, fallback: "free"},
		{name: "too many fields", entries: []string{"free=1:2:3:4"}, fallback: "free"},
		{name: "non-numeric field", entries: []string{"free=1:two:3"}, fallback: "free"},
		{name: "negative value", entries: []string{"free=-1:2:3"}, fallback: "free"},
		{name: "uppercase name", entries: []string{"Free=1:2:3"}, fallback: "Free"},
		{name: "duplicate tier", entries: []string{"free=1:2:3", "free=4:5:6"}, fallback: "free"},
		{name: "zero burst with a rate", entries: []string{"free=1:0:3"}, fallback: "free"},
		{name: "default tier undeclared", entries: []string{"pro=1:2:3"}, fallback: "free"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := workerConfig{guildTierLimits: test.entries, defaultGuildTier: test.fallback}
			_, err := cfg.guildTiers()
			assert.Error(t, err)
		})
	}
}

func TestGuildTiersAllowsUnmeteredTiers(t *testing.T) {
	cfg := workerConfig{guildTierLimits: []string{"unlimited=0:0:0"}, defaultGuildTier: "unlimited"}
	tiers, err := cfg.guildTiers()
	require.NoError(t, err)
	assert.Equal(t, usage.Limits{}, tiers["unlimited"], "zero means recorded but unenforced")
}
