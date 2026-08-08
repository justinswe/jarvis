package mcpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	discordmcp "github.com/justinswe/discord-mcp"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTool is a native genai.FunctionTool for round-trip tests.
type fakeTool struct {
	name   string
	effect llm.ToolEffect
	exec   func(context.Context, map[string]any) (any, error)
}

func (t fakeTool) Name() string { return t.name }
func (t fakeTool) Declaration() *llm.ToolDefinition {
	return &llm.ToolDefinition{
		Name: t.name, Description: "test tool " + t.name, Effect: t.effect,
		InputSchema: llm.JSONSchema{"type": "object", "properties": map[string]any{
			"value": map[string]any{"type": "string"},
		}},
	}
}
func (t fakeTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	return t.exec(ctx, args)
}

func TestSanitizeSchemaProducesValidatableDeclarations(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]any
	}{
		{name: "plain object", schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "text"},
				"limit": map[string]any{"type": "integer"},
			},
			"required": []any{"query"},
		}},
		{name: "anyOf property degrades to string", schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filter": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}, "description": "either"},
			},
		}},
		{name: "ref property degrades to string", schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"target": map[string]any{"$ref": "#/definitions/thing"}},
		}},
		{name: "array without items gains string items", schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"names": map[string]any{"type": "array"}},
		}},
		{name: "required naming an undeclared property is dropped", schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"real": map[string]any{"type": "string"}},
			"required":   []any{"real", "ghost"},
		}},
		{name: "nested object recurses", schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"options": map[string]any{
					"type":       "object",
					"properties": map[string]any{"deep": map[string]any{"oneOf": []any{}}},
					"required":   []any{"deep"},
				},
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schema, ok := SanitizeSchema(test.schema)
			require.True(t, ok)
			require.NoError(t, llm.ValidateToolDefinitions([]llm.ToolDefinition{{
				Name: "mcp_server_tool", InputSchema: schema, Effect: llm.ToolEffectMutation,
			}}), "sanitized schemas must pass the exact provider validation")
		})
	}

	_, ok := SanitizeSchema(map[string]any{"type": "string"})
	assert.False(t, ok, "a non-object root is unrepresentable")
	_, ok = SanitizeSchema("not a schema")
	assert.False(t, ok)
}

func TestModelToolNameForcesTheProviderCharset(t *testing.T) {
	assert.Equal(t, "mcp_github_search_issues", modelToolName("mcp_github_search_issues"))
	sanitized := modelToolName("mcp_srv_do things! (now)")
	assert.NoError(t, llm.ValidateToolDefinitions([]llm.ToolDefinition{{
		Name: sanitized, InputSchema: llm.JSONSchema{"type": "object"},
	}}))
	assert.LessOrEqual(t, len(modelToolName(strings.Repeat("a", 300))), 128)
}

func TestDialRefusesPrivateAndInternalNetworks(t *testing.T) {
	client := newHTTPClient(false)
	for _, target := range []string{"https://127.0.0.1:9/", "https://10.0.0.1:9/", "https://169.254.169.254/", "https://localhost:9/"} {
		_, err := client.Get(target) //nolint:noctx // policy short-circuits before any dial
		require.Error(t, err, target)
		assert.Contains(t, err.Error(), "private or internal", target)
	}

	permissive := newHTTPClient(true)
	_, err := permissive.Get("http://127.0.0.1:9/")
	require.Error(t, err, "nothing listens on port 9")
	assert.NotContains(t, err.Error(), "private or internal", "the escape hatch permits the dial itself")
}

func TestServeDiscordRegistersOnlyTheScopedSubset(t *testing.T) {
	dm, err := discordmcp.New(discordmcp.Options{Token: "test-token"})
	require.NoError(t, err)
	connector := New(Config{}, nil)

	tools, release, err := connector.ServeDiscord(context.Background(), dm,
		"123456789012345678", "223456789012345678", []string{"read_messages", "get_message"})
	require.NoError(t, err)
	defer release()

	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	assert.ElementsMatch(t, []string{"read_messages", "get_message"}, names)
	assert.NotContains(t, names, "send_message", "posting stays with Jarvis's reply pipeline")
	assert.NotContains(t, names, "list_guilds", "the guild pin leaves nothing to discover")
	assert.NotContains(t, names, "list_channels", "guild-wide discovery exceeds the channel-search consent")
	assert.NotContains(t, names, "search_messages", "guild-wide search exceeds the channel-search consent")
}

// TestServeDiscordWithoutAGuildServesNothing keeps DMs off the Discord MCP path: there is
// no guild to pin a view to.
func TestServeDiscordWithoutAGuildServesNothing(t *testing.T) {
	dm, err := discordmcp.New(discordmcp.Options{Token: "test-token"})
	require.NoError(t, err)
	connector := New(Config{}, nil)

	tools, release, err := connector.ServeDiscord(context.Background(), dm, "", "223456789012345678",
		[]string{"read_messages"})
	require.NoError(t, err)
	defer release()
	assert.Empty(t, tools)
}

// echoServer builds an MCP server with one read-only echo tool and one huge-output tool.
func echoServer(t *testing.T) *mcp.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Echoes the input.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + string(request.Params.Arguments)}}}, nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "flood",
		Description: "Returns more text than the model context should carry.",
		InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", maxResultBytes+100)}}}, nil
	})
	return server
}

type staticAuth string

func (a staticAuth) MCPServerAuth(context.Context, string, string) (string, error) {
	return string(a), nil
}

func TestGuildToolsConnectsOverStreamableHTTPWithBearerAuth(t *testing.T) {
	var authHeaders []string
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return echoServer(t) }, nil)
	testServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authHeaders = append(authHeaders, request.Header.Get("Authorization"))
		handler.ServeHTTP(writer, request)
	}))
	defer testServer.Close()

	connector := New(Config{AllowPrivateNetworks: true}, staticAuth("guild-secret"))
	tools, release := connector.GuildTools(context.Background(), "42", []config.MCPServer{
		{Name: "remote", URL: testServer.URL, HasAuth: true, Enabled: true},
		{Name: "down", URL: "http://127.0.0.1:9/", Enabled: true},
		{Name: "disabled", URL: testServer.URL, Enabled: false},
	})
	defer release()

	byName := map[string]genai.FunctionTool{}
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	require.Contains(t, byName, "mcp_remote_echo", "third-party tools are namespaced")
	require.Contains(t, byName, "mcp_remote_flood")
	assert.Len(t, byName, 2, "the unreachable and disabled servers are skipped, never fatal")
	assert.Equal(t, llm.ToolEffectReadOnly, byName["mcp_remote_echo"].Declaration().Effect)
	assert.Equal(t, llm.ToolEffectMutation, byName["mcp_remote_flood"].Declaration().Effect, "unknown effects default to mutation")

	output, err := byName["mcp_remote_echo"].Execute(context.Background(), map[string]any{"text": "hi"})
	require.NoError(t, err)
	assert.Contains(t, output.(string), `"text":"hi"`)
	for _, header := range authHeaders {
		assert.Equal(t, "Bearer guild-secret", header, "every request carries the guild's token")
	}

	flooded, err := byName["mcp_remote_flood"].Execute(context.Background(), map[string]any{})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(flooded.(string)), maxResultBytes+64)
	assert.Contains(t, flooded.(string), "[truncated by the application]")
}

func TestGuildToolsRefusesNonHTTPSWithoutTheEscapeHatch(t *testing.T) {
	connector := New(Config{}, nil)
	tools, release := connector.GuildTools(context.Background(), "42", []config.MCPServer{
		{Name: "plain", URL: "http://mcp.example.com/", Enabled: true},
	})
	defer release()
	assert.Empty(t, tools)
}
