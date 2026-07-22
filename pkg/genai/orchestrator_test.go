package genai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/justinswe/jarvis/pkg/llm"
	"github.com/justinswe/jarvis/pkg/websearch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type scriptedHost struct {
	responses []llm.Response
	errors    []error
	requests  []llm.Request
}

type phaseScriptedHost struct {
	orchestration llm.Host
	presentation  llm.Host
}

func (h phaseScriptedHost) Generate(ctx context.Context, request llm.Request) (llm.Response, error) {
	if len(request.Tools) > 0 {
		return h.orchestration.Generate(ctx, request)
	}
	return h.presentation.Generate(ctx, request)
}

func (h *scriptedHost) Generate(_ context.Context, request llm.Request) (llm.Response, error) {
	h.requests = append(h.requests, request)
	index := len(h.requests) - 1
	var response llm.Response
	if index < len(h.responses) {
		response = h.responses[index]
	}
	if index < len(h.errors) && h.errors[index] != nil {
		return response, h.errors[index]
	}
	if index >= len(h.responses) {
		return llm.Response{}, errors.New("unexpected model call")
	}
	return response, nil
}

func neutralHandler(t *testing.T, profiles []llm.Profile, hosts map[string]llm.Host, selection llm.Selection) *Handler {
	t.Helper()
	for index := range profiles {
		if profiles[index].Name == selection.Primary {
			profiles[index].Capabilities.Tools = true
			profiles[index].Capabilities.ToolChoice = true
		}
	}
	registry, err := llm.NewRegistry(profiles, hosts, selection)
	require.NoError(t, err)
	return &Handler{cfg: Config{MaxOutputTokens: 256}, registry: registry}
}

func neutralText(text string) llm.Response {
	return llm.Response{Message: llm.TextMessage(llm.RoleAssistant, text)}
}

func TestNeutralOrchestrationHostsOrdinaryGenerationAcrossProviders(t *testing.T) {
	for _, provider := range []llm.Provider{llm.ProviderGoogleAI, llm.ProviderVertex, llm.ProviderOpenRouter, llm.ProviderNVIDIANIM} {
		t.Run(string(provider), func(t *testing.T) {
			host := &scriptedHost{responses: []llm.Response{neutralText("answer")}}
			profile := llm.Profile{Name: "primary", Provider: provider, ModelID: "model"}
			handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": host}, llm.Selection{Primary: "primary"})
			got, err := handler.Generate(context.Background(), GenerateRequest{
				Messages: []Message{{Role: "user", Content: "hello"}},
				Config:   &RequestConfig{MaxOutputTokens: 256},
			})
			require.NoError(t, err)
			assert.Equal(t, "answer", got.Text)
			assert.Len(t, host.requests, 1)
			assert.Empty(t, host.requests[0].Tools)
		})
	}
}

func TestNeutralOrchestrationHelloSkipsToolRounds(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{neutralText("hello")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "gemma"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	searcher := &fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{searchResponse("unused.example")}}
	handler.webSearchers = []webSearcher{searcher}
	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Tools:    []FunctionTool{testTool(runtimeContextFunctionName, func(context.Context, map[string]any) (any, error) { return nil, nil })},
		Config:   &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello", got.Text)
	assert.Len(t, primary.requests, 1)
	assert.Empty(t, primary.requests[0].Tools)
	assert.Equal(t, llm.ReasoningMedium, primary.requests[0].ReasoningEffort)
	assert.NotContains(t, primary.requests[0].System, "Call get_runtime_context")
	assert.Contains(t, primary.requests[0].System, "No functions are available in this phase")
	assert.Empty(t, searcher.queries)
}

func TestNeutralOrchestrationConfigurationRequestBypassesSearch(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "configuration", Name: "update_server_configuration", Arguments: map[string]any{"web_search_enabled": false}}}}},
		neutralText("configuration tool phase complete"),
	}}
	primary := &scriptedHost{responses: []llm.Response{neutralText("Web search is now disabled.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "text"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: primary,
	}}, llm.Selection{Primary: "primary"})
	searcher := &fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{searchResponse("unused.example")}}
	handler.webSearchers = []webSearcher{searcher}
	executions := 0
	configuration := fakeTool{
		name: "update_server_configuration",
		decl: &llm.ToolDefinition{Name: "update_server_configuration", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(context.Context, map[string]any) (any, error) {
			executions++
			return map[string]bool{"web_search_enabled": false}, nil
		},
	}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Disable web search."}}, Tools: []FunctionTool{configuration},
		Config: &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "Web search is now disabled.", got.Text)
	assert.Equal(t, 1, executions)
	assert.Empty(t, searcher.queries)
}

func TestNeutralOrchestrationReportsAuthoritativeModelIdentity(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{{
		Message: llm.TextMessage(llm.RoleAssistant, "I am Vertex."),
		Metadata: llm.ResponseMetadata{
			ConfiguredProvider: llm.ProviderOpenRouter, ConfiguredProfile: "primary",
			ActualModelID: "nvidia/nemotron-3-super-120b-a12b-20230311:free", UpstreamProvider: "Nvidia",
		},
	}}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "nvidia/nemotron-3-super-120b-a12b:free"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What model are you?"}},
		Config:   &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.NotContains(t, got.Text, "I am Vertex")
	assert.Contains(t, got.Text, "nvidia/nemotron-3-super-120b-a12b-20230311:free")
	assert.Contains(t, got.Text, "Nvidia")
	require.Len(t, primary.requests, 1)
	assert.Contains(t, primary.requests[0].System, "Application-supplied model identity")
}

func TestNeutralOrchestrationCombinedVersionAndModelUsesRuntimeOnce(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{{
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "runtime", Name: runtimeContextFunctionName, Arguments: map[string]any{}}}},
	}}}
	primary := &scriptedHost{responses: []llm.Response{{
		Message:  llm.TextMessage(llm.RoleAssistant, "Jarvis version v0.6.0."),
		Metadata: llm.ResponseMetadata{ActualModelID: "actual/model", UpstreamProvider: "Upstream"},
	}}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "configured/model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: primary,
	}}, llm.Selection{Primary: "primary"})

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Hey, what version and model are you?"}},
		Tools:    []FunctionTool{runtimeTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Contains(t, got.Text, "v0.6.0")
	assert.Contains(t, got.Text, "configured/model")
	assert.Contains(t, got.Text, "actual/model")
	assert.Len(t, toolHost.requests, 1)
	assert.Len(t, primary.requests, 1)
}

func TestNeutralOrchestrationRepairsNemotronStylePseudoToolText(t *testing.T) {
	pseudo := neutralText(`{"tool":"get_runtime_context","arguments":{}}`)
	primary := &scriptedHost{responses: []llm.Response{pseudo, neutralText("Hello there.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "nemotron"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Tools:    []FunctionTool{runtimeTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Equal(t, "Hello there.", got.Text)
	require.Len(t, primary.requests, 2)
	assert.Contains(t, primary.requests[1].Messages[len(primary.requests[1].Messages)-1].Text(), "pseudo_tool_call")
}

func TestNeutralOrchestrationRepairsUnsolicitedRuntimeIdentityInGreeting(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{
		neutralText("Hey! I'm Chow. I can't access my runtime version or exact model details, but I'm here to help."),
		neutralText("Hey! What can I help you with?"),
	}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "nemotron"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	var observed []generationDiagnostics
	handler.observeGeneration = func(diagnostics generationDiagnostics) { observed = append(observed, diagnostics) }

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hey"}},
		Tools:    []FunctionTool{runtimeTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Equal(t, "Hey! What can I help you with?", got.Text)
	require.Len(t, primary.requests, 2)
	assert.Contains(t, primary.requests[0].System, "Do not volunteer model, provider, runtime, version")
	assert.Contains(t, primary.requests[1].Messages[len(primary.requests[1].Messages)-1].Text(), "unsolicited_runtime_identity")
	require.Len(t, observed, 1)
	assert.Equal(t, 2, observed[0].modelCalls)
	assert.True(t, observed[0].retryUsed)
}

func TestNeutralOrchestrationRejectsSafetyErrorText(t *testing.T) {
	primary := &scriptedHost{
		responses: []llm.Response{neutralText("I can't help with that.")},
		errors:    []error{&llm.Error{Kind: llm.ErrorSafety, Provider: llm.ProviderOpenRouter, Err: errors.New("provider refusal")}},
	}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{MaxOutputTokens: 256},
	})
	require.Error(t, err)
	var modelErr *llm.Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, llm.ErrorSafety, modelErr.Kind)
	assert.Empty(t, got.Text)
	assert.Len(t, primary.requests, 1)
}

func TestNeutralOrchestrationRejectsBlockedFinishWithText(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{{
		Message: llm.TextMessage(llm.RoleAssistant, "Provider refusal text."),
		Finish:  llm.FinishMetadata{Reason: "stop", Blocked: true},
	}}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{MaxOutputTokens: 256},
	})
	require.Error(t, err)
	var modelErr *llm.Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, llm.ErrorSafety, modelErr.Kind)
	assert.Equal(t, "blocked_response", modelErr.ReasonCode)
	assert.Empty(t, got.Text)
	assert.Len(t, primary.requests, 1)
}

func TestNeutralOrchestrationDoesNotReplayMutationDuringSemanticRepairAndFallback(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "mutation", Name: "update_server_configuration", Arguments: map[string]any{"value": "x"}}}}},
		neutralText("mutation planned"),
	}}
	pseudo := neutralText(`{"tool":"update_server_configuration","arguments":{"value":"x"}}`)
	primary := &scriptedHost{responses: []llm.Response{pseudo, pseudo}}
	fallback := &scriptedHost{responses: []llm.Response{neutralText("The configuration was updated once.")}}
	profiles := []llm.Profile{
		{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "nemotron"},
		{Name: "fallback", Provider: llm.ProviderVertex, ModelID: "gemini"},
	}
	handler := neutralHandler(t, profiles, map[string]llm.Host{
		"primary": phaseScriptedHost{orchestration: toolHost, presentation: primary}, "fallback": fallback,
	}, llm.Selection{Primary: "primary", Fallback: "fallback"})
	executions := 0
	mutation := fakeTool{
		name: "update_server_configuration",
		decl: &llm.ToolDefinition{Name: "update_server_configuration", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(context.Context, map[string]any) (any, error) {
			executions++
			return "updated", nil
		},
	}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Update the server configuration."}},
		Tools:    []FunctionTool{mutation},
		Config:   &RequestConfig{MaxOutputTokens: 256, FallbackModelProfile: "fallback"},
	})
	require.NoError(t, err)
	assert.Equal(t, "The configuration was updated once.", got.Text)
	assert.Equal(t, 1, executions)
	assert.Len(t, toolHost.requests, 2)
	assert.Len(t, primary.requests, 2)
	assert.Len(t, fallback.requests, 1)
	assert.Empty(t, fallback.requests[0].Tools)
}

func TestNeutralToolPhaseClassifierCoversConfigurationAndReactionOnly(t *testing.T) {
	tool := testTool("tool", func(context.Context, map[string]any) (any, error) { return nil, nil })
	for _, request := range []string{"Disable web search.", "Show the server settings.", "React to this with a thumbs up."} {
		assert.True(t, shouldRunNeutralToolPhase(GenerateRequest{
			Messages: []Message{{Role: "user", Content: request}}, Tools: []FunctionTool{tool},
		}, AccuracyPolicy{}), request)
	}
	assert.False(t, shouldRunNeutralToolPhase(GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}}, Tools: []FunctionTool{tool},
	}, AccuracyPolicy{}))
}

func TestNeutralOrchestrationUsesPrimaryForToolsBeforePresentation(t *testing.T) {
	for _, provider := range []llm.Provider{llm.ProviderGoogleAI, llm.ProviderVertex, llm.ProviderOpenRouter} {
		t.Run(string(provider), func(t *testing.T) {
			host := &scriptedHost{responses: []llm.Response{
				{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: runtimeContextFunctionName, Arguments: map[string]any{}}}}},
				neutralText("tool result used"),
			}}
			profile := llm.Profile{Name: "primary", Provider: provider, ModelID: "model"}
			handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": host}, llm.Selection{Primary: "primary"})
			tools := []FunctionTool{testTool(runtimeContextFunctionName, func(context.Context, map[string]any) (any, error) { return "v1.2.3", nil })}
			for i := 1; i < 9; i++ {
				name := fmt.Sprintf("authorized_%d", i)
				tools = append(tools, testTool(name, func(context.Context, map[string]any) (any, error) { return nil, nil }))
			}
			got, err := handler.Generate(context.Background(), GenerateRequest{
				Messages: []Message{{Role: "user", Content: "What version are you?"}}, Tools: tools,
				Config: &RequestConfig{MaxOutputTokens: 256, AccuracyPolicy: AccuracyPolicy{
					RequiredFunctionNames: []string{runtimeContextFunctionName},
				}},
			})
			require.NoError(t, err)
			assert.Equal(t, "tool result used", got.Text)
			require.Len(t, host.requests, 2)
			assert.Equal(t, provider, host.requests[0].Profile.Provider)
			assert.Equal(t, provider, host.requests[1].Profile.Provider)
			assert.Len(t, host.requests[0].Tools, 9)
			assert.Equal(t, llm.ToolChoiceFunction, host.requests[0].ToolChoice.Mode)
			assert.Equal(t, runtimeContextFunctionName, host.requests[0].ToolChoice.FunctionName)
			assert.Empty(t, host.requests[1].Tools)
			assert.Contains(t, host.requests[1].Messages[1].Text(), "application_tool_context")
		})
	}
}

func TestNeutralOrchestrationPreservesImagesForPrimaryToolRounds(t *testing.T) {
	host := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "inspect"}}}},
		neutralText("image inspected"),
	}}
	profile := llm.Profile{
		Name: "primary", Provider: llm.ProviderVertex, ModelID: "model",
		Capabilities: llm.Capabilities{Tools: true, ToolChoice: true, Images: true},
	}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": host}, llm.Selection{Primary: "primary"})
	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Inspect this image.", Image: &Image{Data: []byte("image"), MIMEType: "image/png"}}},
		Tools:    []FunctionTool{testTool("inspect", func(context.Context, map[string]any) (any, error) { return "ok", nil })},
		Config: &RequestConfig{MaxOutputTokens: 256, AccuracyPolicy: AccuracyPolicy{
			RequiredFunctionNames: []string{"inspect"},
		}},
	})
	require.NoError(t, err)
	require.Len(t, host.requests, 2)
	require.Len(t, host.requests[0].Messages, 1)
	require.Len(t, host.requests[0].Messages[0].Parts, 2)
	require.NotNil(t, host.requests[0].Messages[0].Parts[0].Image)
	assert.Empty(t, host.requests[1].Tools)
}

func TestNeutralOrchestrationForcesRequiredFunctionsSequentially(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "first"}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "2", Name: "second"}}}},
	}}
	presentation := &scriptedHost{responses: []llm.Response{neutralText("both results used")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "text"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: presentation,
	}}, llm.Selection{Primary: "primary"})
	executions := []string(nil)
	tools := []FunctionTool{
		testTool("first", func(context.Context, map[string]any) (any, error) {
			executions = append(executions, "first")
			return "one", nil
		}),
		testTool("second", func(context.Context, map[string]any) (any, error) {
			executions = append(executions, "second")
			return "two", nil
		}),
	}
	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Run both checks."}}, Tools: tools,
		Config: &RequestConfig{MaxOutputTokens: 256, AccuracyPolicy: AccuracyPolicy{
			RequiredFunctionNames: []string{"first", "second"},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, "both results used", got.Text)
	assert.Equal(t, []string{"first", "second"}, executions)
	require.Len(t, toolHost.requests, 2)
	assert.Equal(t, "first", toolHost.requests[0].ToolChoice.FunctionName)
	assert.Equal(t, "second", toolHost.requests[1].ToolChoice.FunctionName)
	assert.Len(t, toolHost.requests[1].Messages, 3)
}

func TestNeutralOrchestrationCarriesMutationAcrossRetryableFallbackWithoutReplay(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "mutate", Arguments: map[string]any{"value": "x"}}}}},
		neutralText("configuration mutation planned"),
	}}
	primary := &scriptedHost{
		errors: []error{&llm.Error{Kind: llm.ErrorService, Provider: llm.ProviderGoogleAI, Err: errors.New("unavailable")}},
	}
	fallback := &scriptedHost{responses: []llm.Response{neutralText("mutation completed")}}
	profiles := []llm.Profile{
		{Name: "primary", Provider: llm.ProviderGoogleAI, ModelID: "a"},
		{Name: "fallback", Provider: llm.ProviderVertex, ModelID: "b"},
	}
	handler := neutralHandler(t, profiles, map[string]llm.Host{
		"primary": phaseScriptedHost{orchestration: toolHost, presentation: primary}, "fallback": fallback,
	}, llm.Selection{Primary: "primary", Fallback: "fallback"})
	executions := 0
	tool := fakeTool{
		name: "mutate",
		decl: &llm.ToolDefinition{Name: "mutate", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(context.Context, map[string]any) (any, error) { executions++; return "done", nil },
	}
	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Update the server configuration."}}, Tools: []FunctionTool{tool},
		Config: &RequestConfig{MaxOutputTokens: 256, PrimaryModelProfile: "primary", FallbackModelProfile: "fallback"},
	})
	require.NoError(t, err)
	assert.Equal(t, "mutation completed", got.Text)
	assert.Equal(t, 1, executions)
	assert.Len(t, toolHost.requests, 2)
	require.Len(t, fallback.requests, 1)
	assert.Empty(t, primary.requests[0].Tools)
	assert.Empty(t, fallback.requests[0].Tools)
	assert.Contains(t, fallback.requests[0].Messages[1].Text(), "application_tool_context")
}

func TestNeutralOrchestrationAllowsSameMutationWithDifferentArguments(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "mutate", Arguments: map[string]any{"value": "one"}},
			{ID: "2", Name: "mutate", Arguments: map[string]any{"value": "two"}},
		}}},
		neutralText("tool phase complete"),
	}}
	presentation := &scriptedHost{responses: []llm.Response{neutralText("Both changes completed.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "text"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: presentation,
	}}, llm.Selection{Primary: "primary"})
	var values []any
	tool := fakeTool{
		name: "mutate", decl: &llm.ToolDefinition{Name: "mutate", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(_ context.Context, args map[string]any) (any, error) {
			values = append(values, args["value"])
			return "done", nil
		},
	}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Update the server configuration."}}, Tools: []FunctionTool{tool},
		Config: &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Equal(t, "Both changes completed.", got.Text)
	assert.Equal(t, []any{"one", "two"}, values)
	terminal := logs.FilterMessage("Model orchestration completed").All()
	require.Len(t, terminal, 1)
	assert.EqualValues(t, 2, terminal[0].ContextMap()["completed_mutation_count"])
}

func TestNeutralOrchestrationRetriesFailedIdenticalMutation(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "mutate", Arguments: map[string]any{"value": "one"}}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "2", Name: "mutate", Arguments: map[string]any{"value": "one"}}}}},
	}}
	presentation := &scriptedHost{responses: []llm.Response{neutralText("The change completed on retry.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "text"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: presentation,
	}}, llm.Selection{Primary: "primary"})
	executions := 0
	tool := fakeTool{
		name: "mutate", decl: &llm.ToolDefinition{Name: "mutate", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(context.Context, map[string]any) (any, error) {
			executions++
			if executions == 1 {
				return nil, errors.New("transient write failure")
			}
			return "done", nil
		},
	}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Update the server configuration."}}, Tools: []FunctionTool{tool},
		Config: &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Equal(t, "The change completed on retry.", got.Text)
	assert.Equal(t, 2, executions)
}

func TestNeutralOrchestrationExecutesSuccessfulDuplicateMutationOnce(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "provider-call-1", Name: "mutate", Arguments: map[string]any{"nested": map[string]any{"b": 2, "a": 1}}},
			{ID: "provider-call-2", Name: "mutate", Arguments: map[string]any{"nested": map[string]any{"a": 1, "b": 2}}},
		}}},
		neutralText("tool phase complete"),
	}}
	presentation := &scriptedHost{responses: []llm.Response{neutralText("The change completed once.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "text"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: presentation,
	}}, llm.Selection{Primary: "primary"})
	executions := 0
	tool := fakeTool{
		name: "mutate", decl: &llm.ToolDefinition{Name: "mutate", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(context.Context, map[string]any) (any, error) {
			executions++
			return "done", nil
		},
	}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Update the server configuration."}}, Tools: []FunctionTool{tool},
		Config: &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Equal(t, "The change completed once.", got.Text)
	assert.Equal(t, 1, executions)
}

func TestNeutralOrchestrationNeverReportsFailedMutationAsSuccessful(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "mutate"}}}},
		neutralText("tool phase complete"),
	}}
	presentation := &scriptedHost{responses: []llm.Response{neutralText("The configuration was updated successfully.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "text"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: presentation,
	}}, llm.Selection{Primary: "primary"})
	tool := fakeTool{
		name: "mutate", decl: &llm.ToolDefinition{Name: "mutate", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(context.Context, map[string]any) (any, error) { return nil, errors.New("write failed") },
	}
	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Update the server configuration."}}, Tools: []FunctionTool{tool},
		Config: &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Contains(t, got.Text, "Could not complete 1 logical mutation call(s) using `mutate`")
	assert.NotContains(t, got.Text, "updated successfully")
}

func TestNeutralOrchestrationKeepsToolSchemasDuringSameHostContinuation(t *testing.T) {
	host := &scriptedHost{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "mutate", Arguments: map[string]any{"value": "x"}}}}},
		neutralText("tool phase complete"),
		neutralText("mutation completed"),
	}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model", Capabilities: llm.Capabilities{Tools: true, ToolChoice: true}}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": host}, llm.Selection{Primary: "primary"})
	executions := 0
	tool := fakeTool{
		name: "mutate",
		decl: &llm.ToolDefinition{Name: "mutate", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectMutation},
		exec: func(context.Context, map[string]any) (any, error) {
			executions++
			return "done", nil
		},
	}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Update the server configuration."}}, Tools: []FunctionTool{tool},
		Config: &RequestConfig{MaxOutputTokens: 256},
	})

	require.NoError(t, err)
	assert.Equal(t, "mutation completed", got.Text)
	assert.Equal(t, 1, executions)
	require.Len(t, host.requests, 3)
	assert.Len(t, host.requests[0].Tools, 1)
	assert.Len(t, host.requests[1].Tools, 1, "schemas must remain stable across OpenAI-compatible tool rounds")
	require.Len(t, host.requests[1].Messages, 3)
	require.NotNil(t, host.requests[1].Messages[2].ToolResult)
	assert.Empty(t, host.requests[2].Tools, "presentation must not receive function schemas")
}

func TestNeutralOrchestrationDoesNotUseFailureFallbackAsCapabilityRouter(t *testing.T) {
	primary := &scriptedHost{}
	fallback := &scriptedHost{responses: []llm.Response{neutralText("I can inspect the image.")}}
	profiles := []llm.Profile{
		{Name: "text", Provider: llm.ProviderOpenRouter, ModelID: "text"},
		{Name: "vision", Provider: llm.ProviderVertex, ModelID: "vision", Capabilities: llm.Capabilities{Images: true}},
	}
	handler := neutralHandler(t, profiles, map[string]llm.Host{"text": primary, "vision": fallback}, llm.Selection{Primary: "text", Fallback: "vision"})

	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What is this?", Image: &Image{Data: []byte("image"), MIMEType: "image/png"}}},
		Config:   &RequestConfig{MaxOutputTokens: 256, PrimaryModelProfile: "text", FallbackModelProfile: "vision"},
	})

	require.Error(t, err)
	assert.Empty(t, primary.requests)
	assert.Empty(t, fallback.requests)
}

func TestNeutralPresentationFallbackAcrossVertexAndOpenRouter(t *testing.T) {
	for _, providers := range [][2]llm.Provider{
		{llm.ProviderVertex, llm.ProviderOpenRouter},
		{llm.ProviderOpenRouter, llm.ProviderVertex},
		{llm.ProviderGoogleAI, llm.ProviderVertex},
		{llm.ProviderVertex, llm.ProviderGoogleAI},
		{llm.ProviderGoogleAI, llm.ProviderOpenRouter},
		{llm.ProviderOpenRouter, llm.ProviderGoogleAI},
	} {
		name := string(providers[0]) + "-to-" + string(providers[1])
		t.Run(name, func(t *testing.T) {
			primary := &scriptedHost{errors: []error{&llm.Error{Kind: llm.ErrorService, Provider: providers[0]}}}
			fallback := &scriptedHost{responses: []llm.Response{neutralText("fallback answer")}}
			profiles := []llm.Profile{
				{Name: "primary", Provider: providers[0], ModelID: "primary"},
				{Name: "fallback", Provider: providers[1], ModelID: "fallback"},
			}
			handler := neutralHandler(t, profiles, map[string]llm.Host{
				"primary": primary, "fallback": fallback,
			}, llm.Selection{Primary: "primary", Fallback: "fallback"})
			got, err := handler.Generate(context.Background(), GenerateRequest{
				Messages: []Message{{Role: "user", Content: "hello"}},
				Config:   &RequestConfig{MaxOutputTokens: 256, FallbackModelProfile: "fallback"},
			})
			require.NoError(t, err)
			assert.Equal(t, "fallback answer", got.Text)
			require.Len(t, primary.requests, 1)
			require.Len(t, fallback.requests, 1)
			assert.Empty(t, primary.requests[0].Tools)
			assert.Empty(t, fallback.requests[0].Tools)
		})
	}
}

func TestNeutralOrchestrationPreservesPartialDraftWhenFallbackFails(t *testing.T) {
	primary := &scriptedHost{
		responses: []llm.Response{neutralText("A useful partial answer.")},
		errors:    []error{&llm.Error{Kind: llm.ErrorService, Provider: llm.ProviderOpenRouter, StatusCode: 502, Err: errors.New("secret upstream detail")}},
	}
	fallback := &scriptedHost{errors: []error{&llm.Error{Kind: llm.ErrorTimeout, Provider: llm.ProviderNVIDIANIM, Err: errors.New("fallback timed out")}}}
	profiles := []llm.Profile{
		{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "a"},
		{Name: "fallback", Provider: llm.ProviderNVIDIANIM, ModelID: "b"},
	}
	handler := neutralHandler(t, profiles, map[string]llm.Host{"primary": primary, "fallback": fallback}, llm.Selection{Primary: "primary", Fallback: "fallback"})

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{MaxOutputTokens: 256, PrimaryModelProfile: "primary", FallbackModelProfile: "fallback"},
	})

	require.NoError(t, err)
	assert.Equal(t, "A useful partial answer.", got.Text)
	assert.Len(t, fallback.requests, 1)
}

func TestNeutralOrchestrationLogsSafeAttemptAndTerminalDiagnostics(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	providerErr := &llm.Error{
		Kind: llm.ErrorInvalidRequest, Provider: llm.ProviderOpenRouter, StatusCode: 400,
		ProviderStatusCode: 400, ErrorType: "invalid_request", ProviderCode: "invalid_prompt",
		ReasonCode: "invalid_prompt", ProviderRequestID: "gen-123", Scope: "response",
		Request: llm.RequestDiagnostics{
			TokenLimitField: "max_tokens", EffectiveMaxOutputTokens: 256, ReasoningRequested: true,
			PayloadBytes: 1234, MessageCount: 2, UserMessageCount: 1, SystemMessageCount: 1,
			ToolSchemaCount: 9, ToolSchemaFingerprint: "0123456789abcdef",
		},
		Err: errors.New("secret prompt and provider payload"),
	}
	primary := &scriptedHost{errors: []error{providerErr}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "vendor/model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})

	_, err := handler.Generate(context.Background(), GenerateRequest{
		RequestID: "request-1", ChannelID: "channel-1", Messages: []Message{{Role: "user", Content: "private prompt"}},
		Config: &RequestConfig{MaxOutputTokens: 256},
	})
	require.Error(t, err)

	attempts := logs.FilterMessage("Model host round failed").All()
	require.Len(t, attempts, 1)
	attemptFields := attempts[0].ContextMap()
	assert.Equal(t, "openrouter", attemptFields["provider"])
	assert.Equal(t, "primary", attemptFields["profile"])
	assert.Equal(t, "invalid-request", attemptFields["error_kind"])
	assert.EqualValues(t, 400, attemptFields["http_status"])
	assert.Equal(t, "invalid_prompt", attemptFields["provider_code"])
	assert.Equal(t, "invalid_prompt", attemptFields["provider_reason"])
	assert.Equal(t, "gen-123", attemptFields["provider_request_id"])
	assert.Equal(t, "max_tokens", attemptFields["token_limit_field"])
	assert.EqualValues(t, 256, attemptFields["effective_max_output_tokens"])
	assert.Equal(t, true, attemptFields["reasoning_requested"])
	assert.Equal(t, false, attemptFields["reasoning_sent"])
	assert.EqualValues(t, 1234, attemptFields["provider_payload_bytes"])
	assert.EqualValues(t, 9, attemptFields["provider_tool_schema_count"])
	assert.Equal(t, "0123456789abcdef", attemptFields["provider_tool_schema_fingerprint"])
	assert.Equal(t, false, attemptFields["fallback_will_be_attempted"])

	terminal := logs.FilterMessage("Model orchestration completed").All()
	require.Len(t, terminal, 1)
	terminalFields := terminal[0].ContextMap()
	assert.Equal(t, "failed", terminalFields["orchestration_outcome"])
	assert.EqualValues(t, 1, terminalFields["model_attempt_count"])
	assert.Equal(t, false, terminalFields["fallback_attempted"])
	assert.Equal(t, "not-used", terminalFields["search_outcome"])

	formatted := fmt.Sprint(logs.All())
	assert.NotContains(t, formatted, "private prompt")
	assert.NotContains(t, formatted, "secret prompt")
	assert.NotContains(t, formatted, "provider payload")
}

func TestNeutralOrchestrationLogsConfiguredRolesAndActualResponder(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	primary := &scriptedHost{responses: []llm.Response{{
		Message: llm.TextMessage(llm.RoleAssistant, "answer"),
		Metadata: llm.ResponseMetadata{
			ConfiguredProvider: llm.ProviderOpenRouter, ConfiguredProfile: "primary", ActualModelID: "upstream/model",
			ProviderRequestID: "gen-123", UpstreamProvider: "Upstream", RoutingAttempt: 2,
		},
	}}}
	profiles := []llm.Profile{
		{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "configured"},
		{Name: "fallback", Provider: llm.ProviderNVIDIANIM, ModelID: "fallback"},
	}
	handler := neutralHandler(t, profiles, map[string]llm.Host{"primary": primary}, llm.Selection{
		Primary: "primary", Fallback: "fallback",
	})
	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{MaxOutputTokens: 256, PrimaryModelProfile: "primary", FallbackModelProfile: "fallback"},
	})
	require.NoError(t, err)
	entries := logs.FilterMessage("Model orchestration completed").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	assert.Equal(t, "primary", fields["configured_primary_profile"])
	assert.NotContains(t, fields, "configured_tool_profile")
	assert.Empty(t, fields["configured_web_search_providers"])
	assert.Equal(t, "fallback", fields["configured_fallback_profile"])
	assert.Equal(t, "primary", fields["actual_responder_profile"])
	assert.Equal(t, "upstream/model", fields["actual_model_id"])
	assert.Equal(t, "Upstream", fields["actual_upstream_provider"])
	assert.EqualValues(t, 2, fields["provider_routing_attempt"])
}

func TestNeutralOrchestrationObservesOrdinaryChatOnce(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{neutralText("hello")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderGoogleAI, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	var observed []generationDiagnostics
	handler.observeGeneration = func(diagnostics generationDiagnostics) { observed = append(observed, diagnostics) }

	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}}, Config: &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	require.Len(t, observed, 1)
	diagnostics := observed[0]
	assert.False(t, diagnostics.searchRequired)
	assert.False(t, diagnostics.searchAttempted)
	assert.Equal(t, searchTriggerNone, diagnostics.searchTrigger)
	assert.Equal(t, searchResultNotUsed, diagnostics.searchResult)
	assert.Equal(t, 1, diagnostics.modelCalls)
	assert.False(t, diagnostics.retryUsed)
	assert.Equal(t, sourceAvailabilityNotUsed, diagnostics.sourceAvailability)
	assert.Equal(t, terminalFallbackNone, diagnostics.terminalFallbackReason)
	assert.Positive(t, diagnostics.duration)
}

type fakeWebSearcher struct {
	provider     websearch.Provider
	responses    []websearch.Response
	errors       []error
	queries      []string
	events       *[]string
	beforeSearch func()
}

func (s *fakeWebSearcher) Provider() websearch.Provider { return s.provider }

func (s *fakeWebSearcher) Search(_ context.Context, query string) (websearch.Response, error) {
	s.queries = append(s.queries, query)
	if s.events != nil {
		*s.events = append(*s.events, "search:"+string(s.provider))
	}
	if s.beforeSearch != nil {
		s.beforeSearch()
	}
	index := len(s.queries) - 1
	var response websearch.Response
	if index < len(s.responses) {
		response = s.responses[index]
	}
	if index < len(s.errors) {
		return response, s.errors[index]
	}
	return response, nil
}

func searchResponse(domain string) websearch.Response {
	return websearch.Response{
		Results:     []websearch.Result{{Title: "Current result", URL: "https://" + domain + "/article", Domain: domain, Snippet: "Current source summary."}},
		Diagnostics: websearch.Diagnostics{ReturnedResults: 1, AcceptedResults: 1, ResponseBodyBytes: 123, HTTPStatus: 200, ParserOutcome: "ok"},
	}
}

func TestRequiredSearchUsesSerperThenConfiguredFallback(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{neutralText("Current answer.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	serper := &fakeWebSearcher{
		provider:  websearch.ProviderSerper,
		errors:    []error{&websearch.Error{Kind: websearch.ErrorService, Provider: websearch.ProviderSerper, StatusCode: 503}},
		responses: []websearch.Response{{Diagnostics: websearch.Diagnostics{HTTPStatus: 503, ResponseBodyBytes: 10}}},
	}
	tavily := &fakeWebSearcher{provider: websearch.ProviderTavily, responses: []websearch.Response{searchResponse("source.example")}}
	handler.webSearchers = []webSearcher{serper, tavily}
	var observed []generationDiagnostics
	handler.observeGeneration = func(diagnostics generationDiagnostics) { observed = append(observed, diagnostics) }

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What is the latest Go release?"}},
		Config:   &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "Current answer.", got.Text)
	require.Equal(t, searchResponse("source.example").Results, got.Sources)
	webEvidence, ok := evidenceByKind(got.Evidence, EvidenceKindWeb)
	require.True(t, ok)
	assert.Equal(t, string(websearch.ProviderTavily), webEvidence.Tool)
	assert.Equal(t, []string{"What is the latest Go release?"}, serper.queries)
	assert.Equal(t, serper.queries, tavily.queries)
	require.Len(t, primary.requests, 1)
	assert.Contains(t, primary.requests[0].System, `"version":1`)
	assert.Contains(t, primary.requests[0].System, `"status":"sources-available"`)
	assert.Contains(t, primary.requests[0].System, "Current source summary.")
	require.Len(t, observed, 1)
	assert.Equal(t, 1, observed[0].searchInvocationCount)
	assert.Equal(t, 2, observed[0].searchProviderCalls)
	assert.Equal(t, websearch.ProviderSerper, observed[0].primaryProvider)
	assert.Equal(t, websearch.ProviderTavily, observed[0].recoveryProvider)
	assert.Equal(t, searchResultSourcesAvailable, observed[0].searchResult)
	assert.Equal(t, searchResultSourcesAvailable, observed[0].recoveryResult)
	assert.Equal(t, 1, observed[0].modelCalls, "HTTP search calls are not model calls")
	assert.True(t, observed[0].retryUsed)
	assert.True(t, observed[0].sourceAvailable)
}

func TestRequiredSearchBoundsPreviousContextForEllipticalFollowup(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{neutralText("Current answer.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	serper := &fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{searchResponse("source.example")}}
	handler.webSearchers = []webSearcher{serper}
	current := "What about today's price?"

	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: current}},
		Intent: &IntentContext{
			CurrentRequest:      current,
			PreviousUserRequest: strings.Repeat("界", 500),
		},
		Config: &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true, AccuracyPolicy: AccuracyPolicy{WebSearchRequired: true}},
	})
	require.NoError(t, err)
	require.Len(t, serper.queries, 1)
	assert.LessOrEqual(t, len([]rune(serper.queries[0])), 500)
	assert.True(t, strings.HasSuffix(serper.queries[0], "Current follow-up: "+current))
}

func TestRequiredSearchRetriesSameProviderWithoutFallback(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{neutralText("Current answer.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	serper := &fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{{}, searchResponse("recovered.example")}}
	handler.webSearchers = []webSearcher{serper}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Research the latest release."}},
		Config:   &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Len(t, serper.queries, 2)
	assert.Equal(t, "recovered.example", got.Sources[0].Domain)
}

func TestOptionalSearchRecovers(t *testing.T) {
	for _, test := range []struct {
		name             string
		searchers        func() (*fakeWebSearcher, *fakeWebSearcher)
		want             websearch.Provider
		wantPrimaryCalls int
	}{
		{
			name: "configured fallback",
			searchers: func() (*fakeWebSearcher, *fakeWebSearcher) {
				return &fakeWebSearcher{
					provider: websearch.ProviderSerper,
					errors:   []error{&websearch.Error{Kind: websearch.ErrorService, Provider: websearch.ProviderSerper}},
				}, &fakeWebSearcher{provider: websearch.ProviderTavily, responses: []websearch.Response{searchResponse("fallback.example")}}
			},
			want:             websearch.ProviderTavily,
			wantPrimaryCalls: 1,
		},
		{
			name: "same provider retry",
			searchers: func() (*fakeWebSearcher, *fakeWebSearcher) {
				return &fakeWebSearcher{
					provider:  websearch.ProviderSerper,
					responses: []websearch.Response{{}, searchResponse("retry.example")},
				}, nil
			},
			want:             websearch.ProviderSerper,
			wantPrimaryCalls: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := &scriptedHost{responses: []llm.Response{
				{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "search", Name: webSearchFunctionName, Arguments: map[string]any{"query": "ignored"}}}}},
				neutralText("Stable answer."),
			}}
			profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
			handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": host}, llm.Selection{Primary: "primary"})
			primary, fallback := test.searchers()
			handler.webSearchers = []webSearcher{primary}
			if fallback != nil {
				handler.webSearchers = append(handler.webSearchers, fallback)
			}
			var observed []generationDiagnostics
			handler.observeGeneration = func(diagnostics generationDiagnostics) { observed = append(observed, diagnostics) }

			got, err := handler.Generate(context.Background(), GenerateRequest{
				Messages: []Message{{Role: "user", Content: "Explain compiler bootstrapping."}},
				Config:   &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true},
			})
			require.NoError(t, err)
			assert.Equal(t, "Stable answer.", got.Text)
			assert.Equal(t, test.want, websearch.Provider(got.Evidence[0].Tool))
			assert.Len(t, primary.queries, test.wantPrimaryCalls)
			if fallback != nil {
				assert.Equal(t, primary.queries, fallback.queries)
			}
			require.Len(t, host.requests, 2)
			assert.Len(t, host.requests[0].Tools, 1)
			assert.Empty(t, host.requests[1].Tools)
			require.Len(t, observed, 1)
			assert.Equal(t, 1, observed[0].searchInvocationCount)
			assert.Equal(t, 2, observed[0].searchProviderCalls)
			assert.Equal(t, websearch.ProviderSerper, observed[0].primaryProvider)
			assert.Equal(t, test.want, observed[0].recoveryProvider)
			assert.Equal(t, searchResultSourcesAvailable, observed[0].recoveryResult)
			assert.Equal(t, 2, observed[0].modelCalls, "recovery does not add a model call")
		})
	}
}

func TestSearchRecoversEveryProviderError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "invalid input", err: &websearch.Error{Kind: websearch.ErrorInvalidInput, Provider: websearch.ProviderSerper}},
		{name: "authentication", err: &websearch.Error{Kind: websearch.ErrorAuthentication, Provider: websearch.ProviderSerper}},
		{name: "quota", err: &websearch.Error{Kind: websearch.ErrorQuota, Provider: websearch.ProviderSerper}},
		{name: "rate limit", err: &websearch.Error{Kind: websearch.ErrorRateLimit, Provider: websearch.ProviderSerper}},
		{name: "timeout", err: &websearch.Error{Kind: websearch.ErrorTimeout, Provider: websearch.ProviderSerper}},
		{name: "malformed response", err: &websearch.Error{Kind: websearch.ErrorMalformedResponse, Provider: websearch.ProviderSerper}},
		{name: "transport", err: &websearch.Error{Kind: websearch.ErrorTransport, Provider: websearch.ProviderSerper}},
		{name: "service", err: &websearch.Error{Kind: websearch.ErrorService, Provider: websearch.ProviderSerper}},
		{name: "untyped", err: errors.New("untyped provider failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			primary := &fakeWebSearcher{provider: websearch.ProviderSerper, errors: []error{test.err}, events: &events}
			recovery := &fakeWebSearcher{provider: websearch.ProviderTavily, responses: []websearch.Response{searchResponse("source.example")}, events: &events}
			handler := &Handler{webSearchers: []webSearcher{primary, recovery}}
			state := &searchState{sourceAvailability: sourceAvailabilityNotUsed}

			handler.runWebSearch(context.Background(), GenerateRequest{}, "original query", state)

			assert.Equal(t, []string{"search:serper", "search:tavily"}, events)
			require.Len(t, state.calls, 2)
			assert.Equal(t, websearch.ProviderSerper, state.calls[0].provider)
			assert.Equal(t, websearch.ProviderTavily, state.calls[1].provider)
			assert.Equal(t, []string{"original query"}, primary.queries)
			assert.Equal(t, primary.queries, recovery.queries)
			assert.Equal(t, websearch.ProviderTavily, state.resultProvider)
			assert.Equal(t, searchResultSourcesAvailable, state.recoveryResult)
		})
	}
}

func TestSearchDoesNotRecoverAfterRequestContextEnds(t *testing.T) {
	for _, test := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
	}{
		{
			name: "canceled",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
		},
		{
			name: "expired deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.context()
			defer cancel()
			primary := &fakeWebSearcher{provider: websearch.ProviderSerper, errors: []error{errors.New("provider failure")}}
			recovery := &fakeWebSearcher{provider: websearch.ProviderTavily, responses: []websearch.Response{searchResponse("unused.example")}}
			handler := &Handler{webSearchers: []webSearcher{primary, recovery}}
			state := &searchState{sourceAvailability: sourceAvailabilityNotUsed}

			handler.runWebSearch(ctx, GenerateRequest{}, "query", state)

			assert.Len(t, primary.queries, 1)
			assert.Empty(t, recovery.queries)
			assert.Len(t, state.calls, 1)
			assert.False(t, state.recoveryAttempted)
		})
	}
}

func TestSearchRecoveryStopsAfterTwoFailedProviderCalls(t *testing.T) {
	var events []string
	first := &fakeWebSearcher{provider: websearch.ProviderSerper, errors: []error{errors.New("first failure")}, events: &events}
	second := &fakeWebSearcher{provider: websearch.ProviderTavily, errors: []error{errors.New("second failure")}, events: &events}
	third := &fakeWebSearcher{provider: websearch.ProviderFirecrawl, responses: []websearch.Response{searchResponse("unused.example")}, events: &events}
	handler := &Handler{webSearchers: []webSearcher{first, second, third}}
	state := &searchState{sourceAvailability: sourceAvailabilityNotUsed}

	handler.runWebSearch(context.Background(), GenerateRequest{}, "query", state)

	assert.Equal(t, []string{"search:serper", "search:tavily"}, events)
	assert.Len(t, state.calls, 2)
	assert.Empty(t, third.queries)
	assert.Error(t, state.err)
	assert.False(t, state.sourceAvailable())
}

func TestSearchDoesNotRecoverAfterUsableSources(t *testing.T) {
	primary := &fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{searchResponse("primary.example")}}
	recovery := &fakeWebSearcher{provider: websearch.ProviderTavily, responses: []websearch.Response{searchResponse("unused.example")}}
	handler := &Handler{webSearchers: []webSearcher{primary, recovery}}
	state := &searchState{sourceAvailability: sourceAvailabilityNotUsed}

	handler.runWebSearch(context.Background(), GenerateRequest{}, "query", state)

	assert.Len(t, primary.queries, 1)
	assert.Empty(t, recovery.queries)
	assert.Len(t, state.calls, 1)
	assert.False(t, state.recoveryAttempted)
	assert.Equal(t, websearch.ProviderSerper, state.resultProvider)
}

func TestSearchRecoveryKeepsBestProviderResultWithoutMerging(t *testing.T) {
	primaryResponse := searchResponse("primary.example")
	primaryResponse.Results = append(primaryResponse.Results, websearch.Result{
		Title: "Second result", URL: "https://second.example/article", Domain: "second.example", Snippet: "Second source summary.",
	})
	primary := &fakeWebSearcher{
		provider: websearch.ProviderSerper, responses: []websearch.Response{primaryResponse}, errors: []error{errors.New("primary failure")},
	}
	recovery := &fakeWebSearcher{
		provider: websearch.ProviderTavily, responses: []websearch.Response{searchResponse("recovery.example")}, errors: []error{errors.New("recovery failure")},
	}
	handler := &Handler{webSearchers: []webSearcher{primary, recovery}}
	state := &searchState{sourceAvailability: sourceAvailabilityNotUsed}

	handler.runWebSearch(context.Background(), GenerateRequest{}, "query", state)

	require.Len(t, state.calls, 2)
	assert.Equal(t, primaryResponse.Results, state.response.Results)
	assert.Len(t, state.response.Results, 2)
	assert.Equal(t, websearch.ProviderSerper, state.resultProvider)
	assert.Equal(t, searchResultProviderFailed, state.recoveryResult)
}

func TestSearchProviderFailureLogIsSafeAndPrecedesRecovery(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	diagnostics := websearch.Diagnostics{
		ReturnedResults: 2, AcceptedResults: 0, MissingURLResults: 1, InvalidURLResults: 1,
		DuplicateURLResults: 1, MissingSnippetResults: 1, ResponseBodyBytes: 321,
		HTTPStatus: 401, Latency: 25 * time.Millisecond, ParserOutcome: "not_attempted",
	}
	providerErr := errors.Join(errors.New("provider-secret"), &websearch.Error{
		Kind: websearch.ErrorAuthentication, Provider: websearch.ProviderSerper, StatusCode: 401,
	})
	primary := &fakeWebSearcher{
		provider: websearch.ProviderSerper, responses: []websearch.Response{{Diagnostics: diagnostics}}, errors: []error{providerErr},
	}
	recovery := &fakeWebSearcher{
		provider: websearch.ProviderTavily,
		beforeSearch: func() {
			entries := logs.FilterMessage("Web search provider call completed").All()
			require.Len(t, entries, 1)
			assert.Equal(t, zap.WarnLevel, entries[0].Level)
		},
		responses: []websearch.Response{searchResponse("source.example")},
	}
	handler := &Handler{webSearchers: []webSearcher{primary, recovery}}
	state := &searchState{sourceAvailability: sourceAvailabilityNotUsed}

	handler.runWebSearch(context.Background(), GenerateRequest{RequestID: "request-1", ChannelID: "channel-1"}, "private-query credential-secret", state)

	entries := logs.FilterMessage("Web search provider call completed").All()
	require.Len(t, entries, 2)
	assert.Equal(t, zap.WarnLevel, entries[0].Level)
	assert.Equal(t, zap.InfoLevel, entries[1].Level)
	fields := entries[0].ContextMap()
	assert.Equal(t, string(websearch.ProviderSerper), fields["provider"])
	assert.EqualValues(t, 1, fields["provider_call_number"])
	assert.Equal(t, attemptInitial, fields["search_attempt"])
	assert.Equal(t, string(websearch.ErrorAuthentication), fields["error_kind"])
	assert.EqualValues(t, 401, fields["http_status"])
	assert.Equal(t, 25*time.Millisecond, fields["latency"])
	assert.Equal(t, "not_attempted", fields["parser_outcome"])
	assert.EqualValues(t, 2, fields["returned_result_count"])
	assert.EqualValues(t, 0, fields["accepted_result_count"])
	assert.EqualValues(t, 1, fields["missing_url_count"])
	assert.EqualValues(t, 1, fields["invalid_url_count"])
	assert.EqualValues(t, 1, fields["duplicate_url_count"])
	assert.EqualValues(t, 1, fields["missing_snippet_count"])
	formatted := fmt.Sprint(logs.All())
	assert.NotContains(t, formatted, "private-query")
	assert.NotContains(t, formatted, "credential-secret")
	assert.NotContains(t, formatted, "provider-secret")
}

func TestEmptySuccessfulSearchLogRemainsInformational(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	previous := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(previous) })

	primary := &fakeWebSearcher{
		provider:  websearch.ProviderSerper,
		responses: []websearch.Response{{Diagnostics: websearch.Diagnostics{HTTPStatus: 200, ParserOutcome: "ok"}}},
	}
	recovery := &fakeWebSearcher{
		provider: websearch.ProviderTavily,
		beforeSearch: func() {
			entries := logs.FilterMessage("Web search provider call completed").All()
			require.Len(t, entries, 1)
			assert.Equal(t, zap.InfoLevel, entries[0].Level)
		},
		responses: []websearch.Response{searchResponse("source.example")},
	}
	handler := &Handler{webSearchers: []webSearcher{primary, recovery}}

	handler.runWebSearch(context.Background(), GenerateRequest{}, "query", &searchState{})

	entries := logs.FilterMessage("Web search provider call completed").All()
	require.Len(t, entries, 2)
	assert.Equal(t, zap.InfoLevel, entries[0].Level)
}

func TestRuntimeToolPrecedesSearchAndProviderReceivesOnlyResolvedRequest(t *testing.T) {
	var events []string
	toolHost := &scriptedHost{responses: []llm.Response{{Message: llm.Message{
		Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "runtime", Name: runtimeContextFunctionName, Arguments: map[string]any{}}},
	}}}}
	primary := &scriptedHost{responses: []llm.Response{neutralText("Current answer.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderGoogleAI, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: primary,
	}}, llm.Selection{Primary: "primary"})
	serper := &fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{searchResponse("source.example")}, events: &events}
	handler.webSearchers = []webSearcher{serper}
	runtime := evidenceTestTool{
		fakeTool: testTool(runtimeContextFunctionName, func(context.Context, map[string]any) (any, error) {
			events = append(events, "runtime")
			return map[string]string{"private": "runtime output must not become the query"}, nil
		}),
		evidence: Evidence{Kind: EvidenceKindRuntimeContext, Tool: runtimeContextFunctionName, Attributes: map[string]string{
			"version": "v0.8.0", "timezone": "UTC", "current_time": "2026-07-19T12:00:00Z", "weekday": "Sunday",
		}},
	}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What is today's latest release?"}}, Tools: []FunctionTool{runtime},
		Config: &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"runtime", "search:serper"}, events)
	assert.Equal(t, []string{"What is today's latest release?"}, serper.queries)
	assert.NotContains(t, serper.queries[0], "runtime output")
	assert.NotEmpty(t, got.Sources)
}

func TestRequiredSourceLessNewsIsRepairedIntoQualifiedProse(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{
		neutralText("Here are today's headlines: A, B, and C."),
		neutralText("I couldn't confirm today's headlines from usable web sources."),
	}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	handler.webSearchers = []webSearcher{&fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{{}, {}}}}

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What are today's headlines?"}},
		Config:   &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "I couldn't confirm today's headlines from usable web sources.", got.Text)
	assert.Equal(t, EvidenceStatusWebUnconfirmed, got.EvidenceStatus)
	assert.Len(t, primary.requests, 2)
	assert.Contains(t, primary.requests[0].System, `"results":[]`)
	assert.Contains(t, primary.requests[1].Messages[len(primary.requests[1].Messages)-1].Text(), "unsupported_current_claim")
}

func TestRequiredDisabledSearchAlsoRejectsConfidentNews(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{
		neutralText("Here are today's headlines."),
		neutralText("I couldn't confirm today's headlines because web search is unavailable."),
	}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})

	got, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What are today's headlines?"}},
		Config:   &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Contains(t, got.Text, "couldn't confirm")
	assert.Equal(t, EvidenceStatusWebUnconfirmed, got.EvidenceStatus)
}

func TestSearchRecoveryDoesNotReplayCompletedMutation(t *testing.T) {
	toolHost := &scriptedHost{responses: []llm.Response{{Message: llm.Message{
		Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "mutation", Name: "mutate", Arguments: map[string]any{"enabled": true}}},
	}}}}
	primary := &scriptedHost{responses: []llm.Response{neutralText("Updated and checked current sources.")}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": phaseScriptedHost{
		orchestration: toolHost, presentation: primary,
	}}, llm.Selection{Primary: "primary"})
	searcher := &fakeWebSearcher{provider: websearch.ProviderSerper, responses: []websearch.Response{{}, searchResponse("source.example")}}
	handler.webSearchers = []webSearcher{searcher}
	var observed []generationDiagnostics
	handler.observeGeneration = func(diagnostics generationDiagnostics) { observed = append(observed, diagnostics) }
	executions := 0
	mutation := testTool("mutate", func(context.Context, map[string]any) (any, error) {
		executions++
		return map[string]bool{"enabled": true}, nil
	})
	mutation.decl.Effect = llm.ToolEffectMutation

	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Enable web search and check today's news."}}, Tools: []FunctionTool{mutation},
		Config: &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: true, AccuracyPolicy: AccuracyPolicy{
			RequiredFunctionNames: []string{"mutate"}, WebSearchRequired: true,
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, executions)
	assert.Len(t, searcher.queries, 2)
	require.Len(t, observed, 1)
	assert.Equal(t, 1, observed[0].searchInvocationCount)
	assert.Equal(t, 2, observed[0].searchProviderCalls)
	assert.Equal(t, 2, observed[0].modelCalls)
}
