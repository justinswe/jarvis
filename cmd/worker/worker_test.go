package main

import (
	"testing"
	"time"

	"github.com/justinswe/jarvis/internal/config"
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
	assert.Contains(t, defaultPromptFlag.Usage, "may define the assistant name and personality")
}

func TestWorkerServerSettingsAreRequestScopedDefaults(t *testing.T) {
	cfg := workerConfig{messageTimeout: time.Minute}
	settings := cfg.serverSettings()
	assert.True(t, settings.WebSearchEnabled)
	assert.True(t, settings.ChannelSearchEnabled)
	assert.Equal(t, cfg.messageTimeout, settings.MessageTimeout)
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
