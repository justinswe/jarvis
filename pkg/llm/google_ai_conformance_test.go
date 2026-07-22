package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGoogleAIConformance exercises the Gemini Developer API without logging payloads.
func TestGoogleAIConformance(t *testing.T) {
	apiKey := strings.TrimSpace(manualTestOptions.googleAIAPIKey)
	modelID := strings.TrimSpace(manualTestOptions.googleAIModel)
	if apiKey == "" || modelID == "" {
		t.Skip("GOOGLE_AI_API_KEY and an explicit GOOGLE_AI_CONFORMANCE_MODEL were not both supplied")
	}
	host, prober, err := NewGoogleAIHost(context.Background(), GoogleAIConfig{APIKey: apiKey})
	require.NoError(t, err)
	profile := Profile{Name: "google-ai-conformance", Provider: ProviderGoogleAI, ModelID: modelID}
	profile.Capabilities, err = prober.Probe(context.Background(), profile)
	require.NoError(t, err)
	require.True(t, profile.ToolsEnabled(), "conformance model must support tools and named tool choice")
	require.True(t, profile.Capabilities.ReasoningControls, "conformance model must support low, medium, and high thinking levels")
	require.True(t, profile.Capabilities.Images, "conformance model must support inline image input")

	ctx := context.Background()
	base := Request{
		Profile: profile, System: "Answer as a concise Discord assistant. Return only the requested user-facing text.",
		Messages: []Message{TextMessage(RoleUser, "Reply with the single word hello.")}, MaxOutputTokens: 64,
	}
	plain, err := host.Generate(ctx, base)
	require.NoError(t, err)
	require.NotEmpty(t, strings.TrimSpace(plain.Text()))

	for _, effort := range []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh} {
		request := base
		request.ReasoningEffort = effort
		_, err = host.Generate(ctx, request)
		require.NoError(t, err, "reasoning level %s failed", effort)
	}

	tool := ToolDefinition{
		Name: "get_runtime_context", Description: "Return fixed runtime context.",
		InputSchema: JSONSchema{"type": "object", "properties": map[string]any{}}, Effect: ToolEffectReadOnly,
	}
	forced := base
	forced.Messages = []Message{TextMessage(RoleUser, "Call get_runtime_context once.")}
	forced.Tools = []ToolDefinition{tool}
	forced.ToolChoice = ToolChoice{Mode: ToolChoiceFunction, FunctionName: tool.Name}
	first, err := host.Generate(ctx, forced)
	require.NoError(t, err)
	require.NotEmpty(t, first.Message.ToolCalls)
	call := first.Message.ToolCalls[0]
	require.Equal(t, tool.Name, call.Name)

	result := ToolResult{CallID: call.ID, Name: call.Name, Output: map[string]any{"status": "ok"}}
	continuation := forced
	continuation.Messages = []Message{forced.Messages[0], first.Message, {Role: RoleTool, ToolResult: &result}}
	continuation.ToolChoice = ToolChoice{Mode: ToolChoiceAutomatic}
	continued, err := host.Generate(ctx, continuation)
	require.NoError(t, err)
	require.NotEmpty(t, strings.TrimSpace(continued.Text()))

	image := base
	image.Messages = []Message{{Role: RoleUser, Parts: []Part{
		{Text: "Briefly describe whether this image is light or dark."},
		{Image: &Image{Data: vertexCapabilityProbePNG, MIMEType: "image/png"}},
	}}}
	imageResponse, err := host.Generate(ctx, image)
	require.NoError(t, err)
	require.NotEmpty(t, strings.TrimSpace(imageResponse.Text()))
}
