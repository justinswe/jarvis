package genai

import (
	"context"
	"strings"
	"testing"

	"github.com/justinswe/jarvis/pkg/llm"
	"github.com/stretchr/testify/require"
)

type conformanceRuntimeTool struct{}

func (conformanceRuntimeTool) Name() string { return runtimeContextFunctionName }

func (conformanceRuntimeTool) Declaration() *llm.ToolDefinition {
	return &llm.ToolDefinition{
		Name:        runtimeContextFunctionName,
		Description: "Return fixed, non-secret conformance runtime context.",
		InputSchema: llm.JSONSchema{"type": "object", "properties": map[string]any{}},
		Effect:      llm.ToolEffectReadOnly,
	}
}

func (conformanceRuntimeTool) Execute(context.Context, map[string]any) (any, error) {
	return map[string]string{
		"version":      "v0.0.0-conformance",
		"timezone":     "UTC",
		"current_time": "2026-01-02T03:04:05Z",
		"current_date": "2026-01-02",
		"weekday":      "Friday",
	}, nil
}

func (conformanceRuntimeTool) Evidence(any) (Evidence, bool) {
	return Evidence{Kind: EvidenceKindRuntimeContext, Tool: runtimeContextFunctionName, Attributes: map[string]string{
		"version":      "v0.0.0-conformance",
		"timezone":     "UTC",
		"current_time": "2026-01-02T03:04:05Z",
		"current_date": "2026-01-02",
		"weekday":      "Friday",
	}}, true
}

// TestOpenRouterOrchestrationConformance exercises real presentation models through the production neutral orchestrator.
func TestOpenRouterOrchestrationConformance(t *testing.T) {
	apiKey := manualTestOptions.openRouterAPIKey
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY was not explicitly supplied")
	}
	for _, modelID := range openRouterConformanceModelIDs() {
		t.Run(modelID, func(t *testing.T) {
			primaryHost, prober, err := llm.NewOpenRouterHost(llm.OpenAICompatibleConfig{
				APIKey: apiKey, BaseURL: manualTestOptions.openRouterBaseURL,
			})
			require.NoError(t, err)
			primary := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: modelID}
			primary.Capabilities, err = prober.Probe(context.Background(), primary)
			require.NoError(t, err)
			require.True(t, primary.ToolsEnabled(), "conformance primary must advertise tools and tool choice")
			registry, err := llm.NewRegistry(
				[]llm.Profile{primary},
				map[string]llm.Host{"primary": primaryHost},
				llm.Selection{Primary: "primary"},
			)
			require.NoError(t, err)
			handler := &Handler{cfg: Config{MaxOutputTokens: 256}, registry: registry}

			toolDefinition := *conformanceRuntimeTool{}.Declaration()
			requests := []struct {
				name     string
				messages []Message
				contains []string
			}{
				{name: "ordinary chat", messages: []Message{{Role: "user", Content: "Reply with a short friendly hello."}}},
				{name: "model identity", messages: []Message{{Role: "user", Content: "What model are you?"}}, contains: []string{modelID, "openrouter"}},
				{name: "combined runtime identity", messages: []Message{{Role: "user", Content: "What version and model are you?"}}, contains: []string{"v0.0.0-conformance", modelID}},
				{name: "thread follow-up", messages: []Message{
					{Role: "user", Content: "Tell me about your runtime."},
					{Role: "assistant", Content: "Which part would you like to know?"},
					{Role: "user", Content: "Which model is responding in this thread?"},
				}, contains: []string{modelID}},
			}
			for _, test := range requests {
				t.Run(test.name, func(t *testing.T) {
					got, generateErr := handler.Generate(context.Background(), GenerateRequest{
						Messages: test.messages,
						Tools:    []FunctionTool{conformanceRuntimeTool{}},
						Config:   &RequestConfig{MaxOutputTokens: 256},
					})
					require.NoError(t, generateErr)
					require.NotEmpty(t, strings.TrimSpace(got.Text))
					require.Empty(t, pseudoToolName(got.Text, []llm.ToolDefinition{toolDefinition}), "pseudo-tool envelope reached the user")
					for _, expected := range test.contains {
						require.Contains(t, strings.ToLower(got.Text), strings.ToLower(expected))
					}
				})
			}
		})
	}
}

func openRouterConformanceModelIDs() []string {
	value := strings.TrimSpace(manualTestOptions.openRouterModels)
	if value == "" {
		value = strings.TrimSpace(manualTestOptions.openRouterModel)
	}
	if value == "" {
		value = "google/gemma-4-31b-it:free"
	}
	seen := make(map[string]struct{})
	var models []string
	for _, item := range strings.Split(value, ",") {
		modelID := strings.TrimSpace(item)
		if modelID == "" {
			continue
		}
		if _, ok := seen[modelID]; ok {
			continue
		}
		seen[modelID] = struct{}{}
		models = append(models, modelID)
	}
	return models
}
