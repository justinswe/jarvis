package main

import (
	"context"
	"testing"
	"time"

	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/websearch"
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
	databaseEnabled, err := command.Flags().GetBool("dynamodb-enabled")
	require.NoError(t, err)
	table, err := command.Flags().GetString("dynamodb-table")
	require.NoError(t, err)
	roleARN, err := command.Flags().GetString("aws-role-arn")
	require.NoError(t, err)
	audience, err := command.Flags().GetString("aws-web-identity-audience")
	require.NoError(t, err)
	retention, err := command.Flags().GetInt("message-retention-days")
	require.NoError(t, err)
	maxOutputTokens, err := command.Flags().GetInt("max-output-tokens")
	require.NoError(t, err)
	defaultPrompt, err := command.Flags().GetString("default-prompt")
	require.NoError(t, err)
	defaultPromptFlag := command.Flags().Lookup("default-prompt")
	require.NotNil(t, defaultPromptFlag)
	assert.Equal(t, "8080", port)
	assert.Equal(t, "global", location)
	assert.Equal(t, time.Minute, timeout)
	assert.False(t, databaseEnabled)
	assert.Equal(t, "jarvis", table)
	assert.Empty(t, roleARN)
	assert.Empty(t, audience)
	assert.Equal(t, config.DefaultMessageRetentionDays, retention)
	assert.Equal(t, 2048, maxOutputTokens)
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
	cfg := workerConfig{messageTimeout: time.Minute}
	settings := cfg.serverSettings()
	assert.True(t, settings.WebSearchEnabled)
	assert.True(t, settings.ChannelSearchEnabled)
	assert.Equal(t, cfg.messageTimeout, settings.MessageTimeout)
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
