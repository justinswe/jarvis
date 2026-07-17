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
	addErr    error
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
	if m.addErr != nil {
		return config.GuildConfig{}, m.addErr
	}
	m.lastActor = actor
	if !slices.Contains(m.value.AdminUserIDs, userID) {
		m.value.AdminUserIDs = append(m.value.AdminUserIDs, userID)
		m.value.Version++
	}
	return m.value, nil
}

func TestRootAddAdminCommandPersistsBeforeConfirming(t *testing.T) {
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: testSettings()}}
	generator := &fakeGenerator{}
	var sent string
	processor := &Processor{botID: "bot", generator: generator, client: &fakeClient{sendMessage: func(_ context.Context, _ string, content string) (*discordgo.Message, error) {
		sent = content
		return &discordgo.Message{}, nil
	}}, manager: manager, rootUsers: map[string]struct{}{"u": {}}}
	m := targetedMessage("message", "add <@!123456789012345678> as a Jarvis administrator.")
	m.Mentions = append(m.Mentions, &discordgo.User{ID: "123456789012345678"})
	require.NoError(t, processor.Process(context.Background(), m))
	assert.Contains(t, manager.value.AdminUserIDs, "123456789012345678")
	assert.Equal(t, "u", manager.lastActor)
	assert.Contains(t, sent, "is now a Jarvis administrator")
	assert.Nil(t, generator.request)
}

func TestRootAddAdminCommandDoesNotClaimDatabaseFailureAsSuccess(t *testing.T) {
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: testSettings()}, addErr: errors.New("database failed")}
	var sent string
	processor := &Processor{botID: "bot", client: &fakeClient{sendMessage: func(_ context.Context, _ string, content string) (*discordgo.Message, error) {
		sent = content
		return &discordgo.Message{}, nil
	}}, manager: manager, rootUsers: map[string]struct{}{"u": {}}}
	m := targetedMessage("message", "add <@123456789012345678> as admin")
	m.Mentions = append(m.Mentions, &discordgo.User{ID: "123456789012345678"})
	require.NoError(t, processor.Process(context.Background(), m))
	assert.NotContains(t, sent, "is now")
	assert.Contains(t, sent, "could not persist")
}

func TestUserCreatedThreadUsesRollingActivationEvidence(t *testing.T) {
	current := targetedMessage("current", "next question")
	current.Mentions = nil
	client := &fakeClient{messages: func(context.Context, string, int, string) ([]*discordgo.Message, error) {
		return []*discordgo.Message{{ID: "prior", Author: &discordgo.User{ID: "bot", Bot: true}, Content: "prior answer"}}, nil
	}}
	processor := &Processor{botID: "bot", client: client}
	assert.True(t, processor.isTargeted(context.Background(), current, &discordgo.Channel{Type: discordgo.ChannelTypeGuildPublicThread, OwnerID: "user"}))
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
		name          string
		guildConfig   config.GuildConfig
		rootUsers     map[string]struct{}
		permissions   int64
		permissionErr error
		want          bool
		wantTools     int
	}{
		{name: "ordinary user"},
		{name: "delegated administrator", guildConfig: config.GuildConfig{Settings: testSettings(), AdminUserIDs: []string{"u"}}, want: true, wantTools: 2},
		{name: "root user", rootUsers: map[string]struct{}{"u": {}}, want: true, wantTools: 5},
		{name: "Discord administrator", permissions: discordgo.PermissionAdministrator, want: true, wantTools: 2},
		{name: "Discord guild manager", permissions: discordgo.PermissionManageGuild, want: true, wantTools: 2},
		{name: "Discord owner permission set", permissions: discordgo.PermissionAll, want: true, wantTools: 2},
		{name: "permission lookup failure", permissionErr: errors.New("lookup failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeClient{permissions: func(context.Context, string, string) (int64, error) {
				return test.permissions, test.permissionErr
			}}
			processor := &Processor{client: client, manager: manager, rootUsers: test.rootUsers}
			tools, authorized := processor.configurationTools(context.Background(), message, test.guildConfig)
			assert.Equal(t, test.want, authorized)
			if test.want {
				assert.Len(t, tools, test.wantTools)
			} else {
				assert.Empty(t, tools)
			}
		})
	}
}

func TestConfigurationToolSchemasLimitProtectedSettingsToRootUsers(t *testing.T) {
	base := configurationTool{action: updateServerConfigurationToolName, authorized: true}
	assert.Empty(t, base.Declaration().Parameters.Properties)
	base.root = true
	maxOutputTokens := base.Declaration().Parameters.Properties["max_output_tokens"]
	require.NotNil(t, maxOutputTokens.Maximum)
	assert.Equal(t, float64(genai.MaxOutputTokensLimit), *maxOutputTokens.Maximum)
	assert.Contains(t, maxOutputTokens.Description, "including thinking")
	for _, field := range []string{"prompt", "thread_context_window", "parent_context_window", "message_retention_days"} {
		assert.Contains(t, base.Declaration().Parameters.Properties, field)
	}
	assert.NotContains(t, base.Declaration().Parameters.Properties, "temperature")
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
		"thread_context_window":  int64(20),
	})
	require.NoError(t, err)
	response, ok := result.(configurationResponse)
	require.True(t, ok)
	assert.Equal(t, 90, response.MessageRetentionDays)
	assert.Equal(t, 6, response.ParentMessages)
	assert.Equal(t, "Root-controlled Jarvis", response.Prompt)
	assert.Equal(t, 20, response.ThreadMessages)
	assert.Equal(t, []string{
		"message_retention_days", "parent_context_window", "prompt", "thread_context_window",
	}, response.ChangedFields)
	assert.Equal(t, "123456789012345678", manager.lastActor)

	_, err = root.Execute(context.Background(), map[string]any{"temperature": float32(0.5)})
	var removedSetting *genai.ExecutionError
	require.ErrorAs(t, err, &removedSetting)
	assert.Equal(t, "invalid_configuration", removedSetting.Code)

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
	nonRoot.action = addServerAdminToolName
	_, err = nonRoot.Execute(context.Background(), map[string]any{"user_id": "123456789012345679"})
	var denied *genai.ExecutionError
	require.ErrorAs(t, err, &denied)
	assert.Equal(t, "authorization_denied", denied.Code)

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
	assert.Empty(t, response.ChangedFields)
	assert.Empty(t, response.Prompt)

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
		history: &fakeHistory{}, manager: manager, rootUsers: map[string]struct{}{"u": {}},
	}
	require.NoError(t, processor.Process(context.Background(), targetedMessage("message", "show the configuration")))
	require.NotNil(t, generator.request)
	assert.Equal(t, googlegenai.ThinkingLevelHigh, generator.request.Config.ThinkingLevel)
	require.Len(t, generator.request.Tools, 8)
	assert.Equal(t, getServerConfigurationToolName, generator.request.Tools[3].Name())
	assert.Equal(t, setGuildPromptToolName, generator.request.Tools[5].Name())
}
