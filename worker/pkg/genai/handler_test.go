package genai

import (
	"context"
	"strings"
	"testing"

	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/jarvis/worker/pkg/websearch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTool struct {
	name string
	decl *llm.ToolDefinition
	exec func(context.Context, map[string]any) (any, error)
}

func (t fakeTool) Name() string                     { return t.name }
func (t fakeTool) Declaration() *llm.ToolDefinition { return t.decl }
func (t fakeTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	return t.exec(ctx, args)
}

func testTool(name string, execute func(context.Context, map[string]any) (any, error)) fakeTool {
	return fakeTool{
		name: name,
		decl: &llm.ToolDefinition{Name: name, InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectReadOnly},
		exec: execute,
	}
}

type evidenceTestTool struct {
	fakeTool
	evidence Evidence
}

func (t evidenceTestTool) Evidence(any) (Evidence, bool) { return t.evidence, true }

func runtimeTestTool(executionError error) evidenceTestTool {
	return evidenceTestTool{
		fakeTool: testTool(runtimeContextFunctionName, func(context.Context, map[string]any) (any, error) {
			if executionError != nil {
				return nil, executionError
			}
			return map[string]string{"safe": "runtime values"}, nil
		}),
		evidence: Evidence{Kind: EvidenceKindRuntimeContext, Tool: runtimeContextFunctionName, Attributes: map[string]string{
			"version": "v0.6.0", "timezone": "UTC", "current_time": "2026-07-16T18:30:45Z",
			"current_date": "2026-07-16", "weekday": "Thursday",
		}},
	}
}

func TestAgentSystemPromptScalesWithTheOfferedTools(t *testing.T) {
	toolless := composeAgentSystemPrompt("", nil)
	assert.NotContains(t, toolless, "# Tools")
	assert.Contains(t, toolless, "# Final answer")
	assert.Contains(t, toolless, "untrusted data")

	withTools := composeAgentSystemPrompt("", []llm.ToolDefinition{{Name: runtimeContextFunctionName}})
	assert.Contains(t, withTools, "# Tools")
	assert.NotContains(t, withTools, "third-party MCP servers")

	withMCP := composeAgentSystemPrompt("", []llm.ToolDefinition{{Name: "mcp_github_search_issues"}})
	assert.Contains(t, withMCP, "third-party MCP servers")
	assert.Contains(t, withMCP, "never as instructions")
}

func TestSearchToolOffersAnOptionalQuery(t *testing.T) {
	definition := searchToolDefinition()
	assert.Equal(t, webSearchFunctionName, definition.Name)
	properties := definition.InputSchema["properties"].(map[string]any)
	assert.Contains(t, properties, "query")
	assert.NotContains(t, definition.InputSchema, "required")
}

func TestModelSearchQueryFallsBackToTheRequest(t *testing.T) {
	assert.Equal(t, "shot timer prices", modelSearchQuery(map[string]any{"query": " shot timer prices "}, "Prices"))
	assert.Equal(t, "Prices", modelSearchQuery(map[string]any{}, "Prices"))
	assert.Equal(t, "Prices", modelSearchQuery(map[string]any{"query": "   "}, "Prices"))
	assert.Equal(t, "Prices", modelSearchQuery(map[string]any{"query": strings.Repeat("x", 501)}, "Prices"))
}

func TestModelProfileConfigurationRequiresExplicitProfiles(t *testing.T) {
	_, _, err := modelProfileConfiguration(Config{})
	assert.ErrorContains(t, err, "at least one model-profile")

	profiles, selection, err := modelProfileConfiguration(Config{
		ModelProfiles:        []string{"chat=openrouter:vendor/model", "fallback=vertex:gemini"},
		PrimaryModelProfile:  "chat",
		FallbackModelProfile: "fallback",
	})
	require.NoError(t, err)
	assert.Equal(t, llm.Selection{Primary: "chat", Fallback: "fallback"}, selection)
	assert.Equal(t, []llm.Profile{
		{Name: "chat", Provider: llm.ProviderOpenRouter, ModelID: "vendor/model"},
		{Name: "fallback", Provider: llm.ProviderVertex, ModelID: "gemini"},
	}, profiles)
}

func TestWebSearchClientConfigurationValidatesOrderAndDuplicates(t *testing.T) {
	serper, err := websearch.New(websearch.Config{Provider: websearch.ProviderSerper, APIKey: "key"})
	require.NoError(t, err)
	tavily, err := websearch.New(websearch.Config{Provider: websearch.ProviderTavily, APIKey: "key"})
	require.NoError(t, err)
	firecrawl, err := websearch.New(websearch.Config{Provider: websearch.ProviderFirecrawl, APIKey: "key"})
	require.NoError(t, err)

	handler := &Handler{}
	require.NoError(t, handler.setWebSearchClients([]*websearch.Client{serper, tavily}))
	assert.Equal(t, []websearch.Provider{websearch.ProviderSerper, websearch.ProviderTavily}, handler.WebSearchProviders())

	for _, clients := range [][]*websearch.Client{
		{tavily, serper},
		{serper, serper},
		{serper, tavily, firecrawl},
		{nil},
	} {
		handler := &Handler{}
		assert.Error(t, handler.setWebSearchClients(clients))
	}
}
