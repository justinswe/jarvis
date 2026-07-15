package discord

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegenai "google.golang.org/genai"
)

type fakeConfigManager struct {
	value     config.GuildConfig
	loadErr   error
	lastActor string
}

func (m *fakeConfigManager) Load(context.Context, string) (config.GuildConfig, error) {
	return m.value, m.loadErr
}

func (m *fakeConfigManager) Update(_ context.Context, _ string, actor string, patch config.Patch) (config.GuildConfig, error) {
	m.lastActor = actor
	updated, err := patch.Apply(m.value)
	if err != nil {
		return config.GuildConfig{}, err
	}
	updated.Version++
	m.value = updated
	return updated, nil
}

func (m *fakeConfigManager) AddAdmin(_ context.Context, _ string, actor, userID string) (config.GuildConfig, error) {
	m.lastActor = actor
	if !slices.Contains(m.value.AdminUserIDs, userID) {
		m.value.AdminUserIDs = append(m.value.AdminUserIDs, userID)
		m.value.Version++
	}
	return m.value, nil
}

func (m *fakeConfigManager) RemoveAdmin(_ context.Context, _ string, actor, userID string) (config.GuildConfig, error) {
	m.lastActor = actor
	m.value.AdminUserIDs = slices.DeleteFunc(m.value.AdminUserIDs, func(candidate string) bool { return candidate == userID })
	m.value.Version++
	return m.value, nil
}

func TestConfigurationToolsAreExposedOnlyToAdministrators(t *testing.T) {
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: testSettings()}}
	message := targetedMessage("message", "show the configuration")
	tests := []struct {
		name        string
		guildConfig config.GuildConfig
		rootUsers   map[string]struct{}
		permissions int64
		want        bool
	}{
		{name: "ordinary user"},
		{name: "delegated administrator", guildConfig: config.GuildConfig{Settings: testSettings(), AdminUserIDs: []string{"u"}}, want: true},
		{name: "root user", rootUsers: map[string]struct{}{"u": {}}, want: true},
		{name: "Discord administrator", permissions: discordgo.PermissionAdministrator, want: true},
		{name: "Discord guild manager", permissions: discordgo.PermissionManageGuild, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeClient{permissions: func(context.Context, string, string) (int64, error) {
				return test.permissions, nil
			}}
			processor := &Processor{client: client, manager: manager, rootUsers: test.rootUsers}
			tools, authorized := processor.configurationTools(context.Background(), message, test.guildConfig)
			assert.Equal(t, test.want, authorized)
			if test.want {
				assert.Len(t, tools, 5)
			} else {
				assert.Empty(t, tools)
			}
		})
	}
}

func TestConfigurationToolSchemasLimitProtectedSettingsToRootUsers(t *testing.T) {
	base := configurationTool{action: updateServerConfigurationToolName, authorized: true}
	maxOutputTokens := base.Declaration().Parameters.Properties["max_output_tokens"]
	require.NotNil(t, maxOutputTokens.Maximum)
	assert.Equal(t, float64(genai.MaxOutputTokensLimit), *maxOutputTokens.Maximum)
	assert.Contains(t, maxOutputTokens.Description, "including thinking")
	for _, field := range []string{"prompt", "thread_context_window", "parent_context_window", "message_retention_days"} {
		assert.NotContains(t, base.Declaration().Parameters.Properties, field)
	}
	assert.Contains(t, base.Declaration().Parameters.Properties, "channel_context_window")
	base.root = true
	for _, field := range []string{"prompt", "thread_context_window", "parent_context_window", "message_retention_days"} {
		assert.Contains(t, base.Declaration().Parameters.Properties, field)
	}
}

func TestConfigurationToolsUpdateAndDelegate(t *testing.T) {
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: testSettings()}}
	root := configurationTool{
		manager: manager, guildID: "guild", actorID: "123456789012345678", authorized: true, root: true,
		action: updateServerConfigurationToolName,
	}
	result, err := root.Execute(context.Background(), map[string]any{
		"message_retention_days": int64(90),
		"parent_context_window":  int64(6),
		"prompt":                 "Root-controlled Jarvis",
		"temperature":            float32(0.5),
		"thread_context_window":  int64(20),
	})
	require.NoError(t, err)
	response, ok := result.(configurationResponse)
	require.True(t, ok)
	assert.Equal(t, 90, response.MessageRetentionDays)
	assert.Equal(t, 6, response.ParentMessages)
	assert.Equal(t, "Root-controlled Jarvis", response.Prompt)
	assert.Equal(t, float32(0.5), response.Temperature)
	assert.Equal(t, 20, response.ThreadMessages)
	assert.Equal(t, []string{
		"message_retention_days", "parent_context_window", "prompt", "temperature", "thread_context_window",
	}, response.ChangedFields)
	assert.Equal(t, "123456789012345678", manager.lastActor)

	nonRoot := root
	nonRoot.root = false
	for _, args := range []map[string]any{
		{"prompt": "new base"},
		{"thread_context_window": float64(10)},
		{"parent_context_window": float64(10)},
		{"message_retention_days": float64(30)},
	} {
		_, err = nonRoot.Execute(context.Background(), args)
		var executionErr *genai.ExecutionError
		require.ErrorAs(t, err, &executionErr)
		assert.Equal(t, "authorization_denied", executionErr.Code)
	}

	add := root
	add.action = addServerAdminToolName
	result, err = add.Execute(context.Background(), map[string]any{"user_id": "123456789012345679"})
	require.NoError(t, err)
	response = result.(configurationResponse)
	assert.Equal(t, []string{"123456789012345679"}, response.AdminUserIDs)

	remove := root
	remove.action = removeServerAdminToolName
	result, err = remove.Execute(context.Background(), map[string]any{"user_id": "123456789012345679"})
	require.NoError(t, err)
	response = result.(configurationResponse)
	assert.Empty(t, response.AdminUserIDs)
}

func TestSetGuildPromptToolSetsAndClearsPrompt(t *testing.T) {
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: testSettings()}}
	tool := configurationTool{
		manager: manager, guildID: "guild", actorID: "123456789012345678", authorized: true,
		action: setGuildPromptToolName,
	}
	declaration := tool.Declaration()
	require.NotNil(t, declaration.Parameters.Properties["guild_prompt"].MaxLength)
	assert.Equal(t, int64(config.MaxGuildPromptRunes), *declaration.Parameters.Properties["guild_prompt"].MaxLength)

	result, err := tool.Execute(context.Background(), map[string]any{"guild_prompt": "  Use guild terminology.  "})
	require.NoError(t, err)
	response := result.(configurationResponse)
	assert.Equal(t, "Use guild terminology.", response.GuildPrompt)
	assert.Equal(t, []string{"guild_prompt"}, response.ChangedFields)
	assert.Equal(t, "Jarvis", response.Prompt)

	result, err = tool.Execute(context.Background(), map[string]any{"guild_prompt": ""})
	require.NoError(t, err)
	assert.Empty(t, result.(configurationResponse).GuildPrompt)

	_, err = tool.Execute(context.Background(), map[string]any{"guild_prompt": strings.Repeat("x", config.MaxGuildPromptRunes+1)})
	var executionErr *genai.ExecutionError
	require.ErrorAs(t, err, &executionErr)
	assert.Equal(t, "invalid_configuration", executionErr.Code)
}

func TestConfigurationToolReturnsSafeDatabaseFailure(t *testing.T) {
	tool := configurationTool{
		manager: &fakeConfigManager{loadErr: errors.New("secret database details")}, guildID: "guild", actorID: "user",
		authorized: true, action: getServerConfigurationToolName,
	}
	_, err := tool.Execute(context.Background(), nil)
	var executionErr *genai.ExecutionError
	require.ErrorAs(t, err, &executionErr)
	assert.Equal(t, "database_unavailable", executionErr.Code)
	assert.NotContains(t, executionErr.Message, "secret")
}

func TestProcessUsesHighThinkingWhenAdminToolsAreExposed(t *testing.T) {
	settings := testSettings()
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: settings}}
	processor := &Processor{
		botID: "bot", generator: generator, client: &fakeClient{}, configs: &countingProvider{settings: settings},
		manager: manager, rootUsers: map[string]struct{}{"u": {}},
	}
	require.NoError(t, processor.Process(context.Background(), targetedMessage("message", "show the configuration")))
	require.NotNil(t, generator.request)
	assert.Equal(t, googlegenai.ThinkingLevelHigh, generator.request.Config.ThinkingLevel)
	require.Len(t, generator.request.Tools, 7)
	assert.Equal(t, getServerConfigurationToolName, generator.request.Tools[2].Name())
	assert.Equal(t, setGuildPromptToolName, generator.request.Tools[4].Name())
}
