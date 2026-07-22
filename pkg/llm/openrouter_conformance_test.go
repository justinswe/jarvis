package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpenRouterConformance exercises the production chat-completions shapes without logging payloads.
func TestOpenRouterConformance(t *testing.T) {
	if manualTestOptions.openRouterAllTextModels &&
		strings.TrimSpace(manualTestOptions.openRouterModels) == "" &&
		strings.TrimSpace(manualTestOptions.openRouterModel) == "" {
		t.Skip("catalog-only conformance requested without an explicit live-generation model list")
	}
	apiKey := manualTestOptions.openRouterAPIKey
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY was not explicitly supplied")
	}
	host, prober, err := NewOpenRouterHost(OpenAICompatibleConfig{
		APIKey:  apiKey,
		BaseURL: manualTestOptions.openRouterBaseURL,
	})
	require.NoError(t, err)
	for _, modelID := range openRouterConformanceModels() {
		t.Run(modelID, func(t *testing.T) {
			profile := Profile{Name: "conformance", Provider: ProviderOpenRouter, ModelID: modelID}
			profile.Capabilities, err = prober.Probe(context.Background(), profile)
			require.NoError(t, err)

			ctx := context.Background()
			base := Request{
				Profile: profile, System: "Answer as a concise Discord assistant. Return only the requested user-facing text.",
				Messages: []Message{TextMessage(RoleUser, "Reply with the single word hello.")}, MaxOutputTokens: 64,
			}
			plain, generateErr := host.Generate(ctx, base)
			require.NoError(t, generateErr, "no-tool conformance request failed")
			require.NotEmpty(t, strings.TrimSpace(plain.Text()), "no-tool conformance returned no visible text")
			require.Empty(t, plain.Message.ToolCalls, "no-tool conformance unexpectedly returned a native tool call")

			if !profile.Capabilities.Tools || !profile.Capabilities.ToolChoice {
				t.Log("model does not advertise tool routing; presentation conformance passed")
				return
			}
			runOpenRouterToolConformance(t, ctx, host, base)
		})
	}
}

func runOpenRouterToolConformance(t *testing.T, ctx context.Context, host Host, base Request) {
	t.Helper()
	ping := ToolDefinition{
		Name: "ping", Description: "Return a fixed health value.",
		InputSchema: JSONSchema{"type": "object", "properties": map[string]any{}},
		Effect:      ToolEffectReadOnly,
	}
	oneTool := base
	oneTool.Messages = []Message{TextMessage(RoleUser, "Use the ping function if it is useful, then answer briefly.")}
	oneTool.Tools = []ToolDefinition{ping}
	oneTool.ToolChoice = ToolChoice{Mode: ToolChoiceAutomatic}
	_, err := host.Generate(ctx, oneTool)
	require.NoError(t, err, "one-tool automatic request failed")

	bundle := conformanceToolBundle()
	productionBundle := base
	productionBundle.Messages = []Message{TextMessage(RoleUser, "Choose an appropriate harmless function if needed, then answer briefly.")}
	productionBundle.Tools = bundle
	productionBundle.ToolChoice = ToolChoice{Mode: ToolChoiceAutomatic}
	_, err = host.Generate(ctx, productionBundle)
	require.NoError(t, err, "production tool-bundle request failed")

	forced := base
	forced.Messages = []Message{TextMessage(RoleUser, "Call get_runtime_context once.")}
	forced.Tools = bundle
	forced.ToolChoice = ToolChoice{Mode: ToolChoiceFunction, FunctionName: "get_runtime_context"}
	first, err := host.Generate(ctx, forced)
	require.NoError(t, err, "forced tool-choice request failed")
	require.NotEmpty(t, first.Message.ToolCalls, "forced tool choice returned no function call")
	call := first.Message.ToolCalls[0]
	require.Equal(t, "get_runtime_context", call.Name)

	result := ToolResult{CallID: call.ID, Name: call.Name, Output: map[string]any{"status": "ok"}}
	continuation := forced
	continuation.Messages = []Message{
		forced.Messages[0], first.Message, {Role: RoleTool, ToolResult: &result},
	}
	continuation.ToolChoice = ToolChoice{Mode: ToolChoiceAutomatic}
	_, err = host.Generate(ctx, continuation)
	require.NoError(t, err, "tool-result continuation failed")
}

func TestOpenRouterCatalogConformance(t *testing.T) {
	if !manualTestOptions.openRouterAllTextModels {
		t.Skip("OPENROUTER_CONFORMANCE_ALL_TEXT_MODELS=1 was not explicitly supplied")
	}
	apiKey := manualTestOptions.openRouterAPIKey
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY was not explicitly supplied")
	}
	hostValue, _, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: apiKey, BaseURL: manualTestOptions.openRouterBaseURL})
	require.NoError(t, err)
	host := hostValue.(*openAICompatibleHost)
	var catalog modelCatalog
	require.NoError(t, host.getCompatibleJSON(context.Background(), "/models/user", "conformance-catalog", &catalog))
	limit := openRouterConformanceModelLimit(len(catalog.Data))
	checked := 0
	for _, model := range catalog.Data {
		if len(model.Architecture.OutputModalities) > 0 && !containsFold(model.Architecture.OutputModalities, "text") {
			continue
		}
		if checked >= limit {
			break
		}
		checked++
		t.Run(model.ID, func(t *testing.T) {
			_, _, _, endpointErr := host.probeOpenRouterEndpoint(context.Background(), model.ID)
			require.NoError(t, endpointErr)
			_, wireErr := host.catalogWireCapabilities(model.SupportedParameters, model.Reasoning.SupportedEfforts)
			require.NoError(t, wireErr)
		})
	}
	require.Positive(t, checked, "credential-filtered catalog contained no text models")
}

func openRouterConformanceModels() []string {
	value := strings.TrimSpace(manualTestOptions.openRouterModels)
	if value == "" {
		value = strings.TrimSpace(manualTestOptions.openRouterModel)
	}
	if value == "" {
		value = "google/gemma-4-31b-it:free"
	}
	seen := make(map[string]struct{})
	var result []string
	for _, item := range strings.Split(value, ",") {
		modelID := strings.TrimSpace(item)
		if modelID == "" {
			continue
		}
		if _, ok := seen[modelID]; ok {
			continue
		}
		seen[modelID] = struct{}{}
		result = append(result, modelID)
	}
	return result
}

func openRouterConformanceModelLimit(catalogSize int) int {
	if manualTestOptions.openRouterMaxModels == 0 {
		return catalogSize
	}
	if manualTestOptions.openRouterMaxModels < 1 {
		return 1
	}
	return manualTestOptions.openRouterMaxModels
}

func conformanceToolBundle() []ToolDefinition {
	tools := make([]ToolDefinition, 0, 9)
	for _, name := range []string{
		"get_runtime_context", "add_message_reaction", "search_current_channel", "get_server_configuration",
		"update_server_configuration", "set_guild_prompt", "add_server_admin", "remove_server_admin", "search_web",
	} {
		tools = append(tools, ToolDefinition{
			Name: name, Description: "Harmless conformance declaration.",
			InputSchema: JSONSchema{"type": "object", "properties": map[string]any{}},
			Effect:      ToolEffectReadOnly,
		})
	}
	return tools
}
