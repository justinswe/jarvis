package discord

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/mcpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRootSeesTheMCPManagementTools(t *testing.T) {
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: testSettings()}}
	message := targetedMessage("message", "attach an MCP server")
	processor := &Processor{client: &fakeClient{}, manager: manager, rootUsers: map[string]struct{}{"u": {}}}

	tools, authorized := processor.configurationTools(context.Background(), message, config.GuildConfig{Settings: testSettings()})
	require.True(t, authorized)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	assert.Contains(t, names, addMCPServerToolName)
	assert.Contains(t, names, removeMCPServerToolName)

	admin := &Processor{client: &fakeClient{}, manager: manager}
	adminTools, authorized := admin.configurationTools(context.Background(), message,
		config.GuildConfig{Settings: testSettings(), AdminUserIDs: []string{"u"}})
	require.True(t, authorized)
	for _, tool := range adminTools {
		assert.NotContains(t, []string{addMCPServerToolName, removeMCPServerToolName}, tool.Name(),
			"MCP management never reaches non-root administrators")
	}
}

func TestMCPServerToolsAreRootOnlyAndTheTokenIsWriteOnly(t *testing.T) {
	ctx := context.Background()
	manager := &fakeConfigManager{value: config.GuildConfig{Settings: testSettings()}}
	base := configurationTool{manager: manager, guildID: "guild", actorID: "u", authorized: true, access: "delegated_admin"}

	_, err := configurationToolWithAction(base, addMCPServerToolName).Execute(ctx,
		map[string]any{"name": "github", "url": "https://mcp.example.com"})
	require.Error(t, err, "a non-root caller is refused even if the tool leaks")
	var execution *genai.ExecutionError
	require.ErrorAs(t, err, &execution)
	assert.Equal(t, "authorization_denied", execution.Code)

	base.root, base.access = true, "root"
	output, err := configurationToolWithAction(base, addMCPServerToolName).Execute(ctx,
		map[string]any{"name": "github", "url": "https://mcp.example.com", "auth_token": "super-secret-token"})
	require.NoError(t, err)
	response, ok := output.(configurationResponse)
	require.True(t, ok)
	require.Len(t, response.MCPServers, 1)
	assert.True(t, response.MCPServers[0].HasAuth)
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "super-secret-token", "the token never appears in any response")

	_, err = configurationToolWithAction(base, addMCPServerToolName).Execute(ctx,
		map[string]any{"name": "Bad Name!", "url": "https://mcp.example.com"})
	require.Error(t, err)
	require.ErrorAs(t, err, &execution)
	assert.Equal(t, "invalid_configuration", execution.Code)

	output, err = configurationToolWithAction(base, removeMCPServerToolName).Execute(ctx, map[string]any{"name": "github"})
	require.NoError(t, err)
	response, ok = output.(configurationResponse)
	require.True(t, ok)
	assert.Empty(t, response.MCPServers)
}

func TestRemoteMCPServersMergeDefaultsAndGuildRows(t *testing.T) {
	processor := &Processor{defaultMCPServers: []config.MCPServer{
		{Name: "github", URL: "https://default.example/github", Enabled: true},
		{Name: "docs", URL: "https://default.example/docs", Enabled: true},
	}}
	guildConfig := config.GuildConfig{MCPServers: []config.MCPServer{
		{Name: "github", URL: "https://guild.example/github", HasAuth: true, Enabled: true},
		{Name: "tickets", URL: "https://guild.example/tickets", Enabled: true},
	}}

	merged := processor.remoteMCPServers("g1", guildConfig)
	byName := map[string]config.MCPServer{}
	for _, server := range merged {
		byName[server.Name] = server
	}
	require.Len(t, merged, 3)
	assert.Equal(t, "https://guild.example/github", byName["github"].URL, "a guild row overrides a default of the same name")
	assert.Equal(t, "https://default.example/docs", byName["docs"].URL)
	assert.Contains(t, byName, "tickets")

	assert.Empty(t, processor.remoteMCPServers("", guildConfig), "DMs get no remote MCP servers")
}

func TestMCPToolsWithoutAConnectorKeepsNativeTools(t *testing.T) {
	processor := &Processor{}
	native := []genai.FunctionTool{processor.runtimeContext()}
	tools, release := processor.mcpTools(context.Background(), targetedMessage("message", "hello"), config.GuildConfig{Settings: testSettings()}, native)
	defer release()
	require.Len(t, tools, 1)
	assert.Equal(t, native[0].Name(), tools[0].Name())
}

// TestMCPToolsPreserveNativeEvidenceProducers is the regression guard for routing native
// tools through MCP: an adapter in front of them satisfies genai.FunctionTool but drops
// EvidenceProducer, and the orchestrator reaches evidence only through that optional
// interface — so accuracy validation silently rejects correct runtime and channel answers.
func TestMCPToolsPreserveNativeEvidenceProducers(t *testing.T) {
	processor := &Processor{mcp: mcpx.New(mcpx.Config{}, nil)}
	native := []genai.FunctionTool{processor.runtimeContext()}
	require.Implements(t, (*genai.EvidenceProducer)(nil), native[0])

	tools, release := processor.mcpTools(context.Background(),
		targetedMessage("message", "what time is it"), config.GuildConfig{Settings: testSettings()}, native)
	defer release()

	require.NotEmpty(t, tools)
	byName := map[string]genai.FunctionTool{}
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	runtimeTool, ok := byName[native[0].Name()]
	require.True(t, ok, "the runtime context tool survives tool assembly")
	assert.Implements(t, (*genai.EvidenceProducer)(nil), runtimeTool,
		"a wired connector must not wrap native tools; evidence collection depends on the concrete type")
}
