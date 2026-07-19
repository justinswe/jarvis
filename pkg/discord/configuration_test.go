package discord

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/jarvis/pkg/llm"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Empty(t, schemaProperties(t, base.Declaration()))
	base.root = true
	properties := schemaProperties(t, base.Declaration())
	maxOutputTokens := schemaProperty(t, properties, "max_output_tokens")
	assert.Equal(t, genai.MaxOutputTokensLimit, maxOutputTokens["maximum"])
	assert.Contains(t, maxOutputTokens["description"], "including thinking")
	prompt := schemaProperty(t, properties, "prompt")
	assert.Contains(t, prompt["description"], "may define the assistant's name and personality")
	assert.NotContains(t, prompt["description"], "Jarvis's core identity")
	for _, field := range []string{"prompt", "thread_context_window", "parent_context_window", "message_retention_days"} {
		assert.Contains(t, properties, field)
	}
	assert.NotContains(t, properties, "temperature")
	assert.NotContains(t, properties, "tool_model_profile")
	assert.NotContains(t, properties, "web_search_model_profile")
}

func TestRootConfigurationReadsAndUpdatesModelProfiles(t *testing.T) {
	models := testConfigurationModels(t)
	settings := testSettings()
	settings.PrimaryModelProfile = "fast"
	settings.FallbackModelProfile = "presentation"
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: settings}}
	root := configurationTool{
		manager: manager, models: models, webSearchProviders: []string{"serper", "tavily"}, guildID: "guild", actorID: "123456789012345678",
		authorized: true, root: true, access: "root", action: getServerConfigurationToolName,
	}

	result, err := root.Execute(context.Background(), nil)
	require.NoError(t, err)
	response := result.(configurationResponse)
	assert.Equal(t, "fast", response.PrimaryModelProfile)
	assert.Equal(t, "presentation", response.FallbackModelProfile)
	assert.Equal(t, []string{"serper", "tavily"}, response.WebSearchProviders)
	assert.Equal(t, []configurationProfile{
		{Name: "fast", Provider: string(llm.ProviderOpenRouter), Tools: true, ToolChoice: true},
		{Name: "google", Provider: string(llm.ProviderGoogleAI)},
		{Name: "presentation", Provider: string(llm.ProviderVertex)},
		{Name: "quality", Provider: string(llm.ProviderNVIDIANIM), Tools: true, ToolChoice: true},
	}, response.AvailableProfiles)

	root.action = updateServerConfigurationToolName
	properties := schemaProperties(t, root.Declaration())
	assert.Equal(t, []string{"fast", "quality"}, schemaProperty(t, properties, "primary_model_profile")["enum"])
	assert.Equal(t, []string{"", "fast", "google", "presentation", "quality"}, schemaProperty(t, properties, "fallback_model_profile")["enum"])
	result, err = root.Execute(context.Background(), map[string]any{
		"primary_model_profile":  "quality",
		"fallback_model_profile": "",
	})
	require.NoError(t, err)
	response = result.(configurationResponse)
	assert.Equal(t, "quality", response.PrimaryModelProfile)
	assert.Empty(t, response.FallbackModelProfile)
	assert.Equal(t, []string{"fallback_model_profile", "primary_model_profile"}, response.ChangedFields)
}

func TestRootConfigurationRejectsInvalidModelPairs(t *testing.T) {
	models := testConfigurationModels(t)
	settings := testSettings()
	settings.PrimaryModelProfile = "quality"
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: settings}}
	root := configurationTool{
		manager: manager, models: models, guildID: "guild", actorID: "123456789012345678",
		authorized: true, root: true, action: updateServerConfigurationToolName,
	}

	for _, args := range []map[string]any{
		{"primary_model_profile": "missing"},
		{"primary_model_profile": "presentation"},
		{"primary_model_profile": "fast", "fallback_model_profile": "fast"},
	} {
		_, err := root.Execute(context.Background(), args)
		var executionErr *genai.ExecutionError
		require.ErrorAs(t, err, &executionErr)
		assert.Equal(t, "invalid_configuration", executionErr.Code)
	}
}

func TestRootConfigurationReadPreservesStaleProfileAlias(t *testing.T) {
	settings := testSettings()
	settings.PrimaryModelProfile = "removed-profile"
	tool := configurationTool{
		manager: &fakeConfigManager{value: config.GuildConfig{Settings: settings}}, models: testConfigurationModels(t),
		guildID: "guild", actorID: "123456789012345678", authorized: true, root: true, access: "root",
		action: getServerConfigurationToolName,
	}
	result, err := tool.Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "removed-profile", result.(configurationResponse).PrimaryModelProfile)
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
	guildPrompt := schemaProperty(t, schemaProperties(t, declaration), "guild_prompt")
	assert.Equal(t, config.MaxGuildPromptRunes, guildPrompt["maxLength"])
	assert.Contains(t, declaration.Description, "cannot assign or change the assistant's name")
	assert.Contains(t, declaration.Description, "override root-controlled customization")
	assert.Contains(t, guildPrompt["description"], "cannot assign or change the assistant's name")

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
	assert.Equal(t, llm.ReasoningHigh, generator.request.Config.ReasoningEffort)
	require.Len(t, generator.request.Tools, 8)
	assert.Equal(t, getServerConfigurationToolName, generator.request.Tools[3].Name())
	assert.Equal(t, setGuildPromptToolName, generator.request.Tools[5].Name())
}

func TestRootVersionRequestPreservesRuntimeToolRouteAndRequestShape(t *testing.T) {
	settings := testSettings()
	generator := &fakeGenerator{response: genai.GenerateResponse{Text: "ok"}}
	processor := &Processor{
		botID: "bot", generator: generator, client: &fakeClient{}, configs: &countingProvider{settings: settings},
		history: &fakeHistory{}, manager: &fakeConfigManager{value: config.GuildConfig{Settings: settings}},
		rootUsers: map[string]struct{}{"u": {}}, version: "v0.6.0",
	}
	require.NoError(t, processor.Process(context.Background(), targetedMessage("message", "what version are you")))
	require.NotNil(t, generator.request)
	assert.Equal(t, "CURRENT REQUEST:\nwhat version are you", generator.request.Messages[len(generator.request.Messages)-1].Content)
	assert.Equal(t, llm.ReasoningHigh, generator.request.Config.ReasoningEffort)
	assert.Equal(t, genai.AccuracyPolicy{
		RequiredFunctionNames: []string{runtimeContextToolName}, RuntimeContextRelevant: true,
	}, generator.request.Config.AccuracyPolicy)
	assert.Len(t, generator.request.Tools, 8)
}

func schemaProperties(t *testing.T, declaration *llm.ToolDefinition) map[string]any {
	t.Helper()
	properties, ok := declaration.InputSchema["properties"].(map[string]any)
	require.True(t, ok)
	return properties
}

func schemaProperty(t *testing.T, properties map[string]any, name string) llm.JSONSchema {
	t.Helper()
	property, ok := properties[name].(llm.JSONSchema)
	require.True(t, ok)
	return property
}

func testConfigurationModels(t *testing.T) *llm.Registry {
	t.Helper()
	registry, err := llm.NewRegistry([]llm.Profile{
		{Name: "fast", Provider: llm.ProviderOpenRouter, ModelID: "fast", Capabilities: llm.Capabilities{Tools: true, ToolChoice: true}},
		{Name: "google", Provider: llm.ProviderGoogleAI, ModelID: "gemini-3.1-flash-lite"},
		{Name: "presentation", Provider: llm.ProviderVertex, ModelID: "presentation"},
		{Name: "quality", Provider: llm.ProviderNVIDIANIM, ModelID: "quality", Capabilities: llm.Capabilities{Tools: true, ToolChoice: true}},
	}, nil, llm.Selection{Primary: "fast", Fallback: "presentation"})
	require.NoError(t, err)
	return registry
}
