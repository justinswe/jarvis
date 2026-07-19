package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	stderrors "github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegenai "google.golang.org/genai"
)

func newTestOpenRouterHost(t *testing.T, baseURL string) Host {
	t.Helper()
	host, _, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: "secret", BaseURL: baseURL})
	require.NoError(t, err)
	host.(*openAICompatibleHost).setWireCapabilities("vendor/model", compatibleWireCapabilities{})
	host.(*openAICompatibleHost).setWireCapabilities("vendor/reasoning", compatibleWireCapabilities{})
	return host
}

func TestParseProfileSplitsOnlyRequiredDelimiters(t *testing.T) {
	profile, err := ParseProfile(" fast = openrouter:vendor/model:free ")
	require.NoError(t, err)
	assert.Equal(t, ProfileSpec{Name: "fast", Provider: ProviderOpenRouter, ModelID: "vendor/model:free"}, profile)
	profile, err = ParseProfile("studio=google-ai:gemini-3.1-flash-lite")
	require.NoError(t, err)
	assert.Equal(t, ProfileSpec{Name: "studio", Provider: ProviderGoogleAI, ModelID: "gemini-3.1-flash-lite"}, profile)

	_, err = ParseProfiles([]string{"same=vertex:a", "same=openrouter:b"})
	assert.ErrorContains(t, err, "duplicate")
}

func TestRegistryValidatesReferencesAndAllowsSameProviderFallback(t *testing.T) {
	profiles := []Profile{
		{Name: "a", Provider: ProviderOpenRouter, ModelID: "one", Capabilities: Capabilities{Tools: true, ToolChoice: true}},
		{Name: "b", Provider: ProviderOpenRouter, ModelID: "two"},
	}
	registry, err := NewRegistry(profiles, nil, Selection{Primary: "a", Fallback: "b"})
	require.NoError(t, err)

	_, err = NewRegistry(profiles, nil, Selection{Primary: "a", Fallback: "a"})
	assert.ErrorContains(t, err, "different")
	_, err = NewRegistry(profiles, nil, Selection{Primary: "missing"})
	assert.ErrorContains(t, err, "does not exist")

	primary, fallback, stale := registry.Resolve("removed", "also-removed")
	assert.True(t, stale)
	assert.Equal(t, "a", primary.Name)
	require.NotNil(t, fallback)
	assert.Equal(t, "b", fallback.Name)

	primary, fallback, stale = registry.Resolve("b", "")
	assert.True(t, stale)
	assert.Equal(t, "a", primary.Name)
	require.NotNil(t, fallback)
	assert.Equal(t, "b", fallback.Name)
}

func TestRegistryRequiresToolCapablePrimaryAndAllowsPresentationFallback(t *testing.T) {
	profiles := []Profile{
		{Name: "primary", Provider: ProviderOpenRouter, ModelID: "host", Capabilities: Capabilities{Tools: true, ToolChoice: true}},
		{Name: "fallback", Provider: ProviderVertex, ModelID: "gemini"},
	}
	_, err := NewRegistry(profiles, nil, Selection{Primary: "primary", Fallback: "fallback"})
	require.NoError(t, err)

	profiles[0].Capabilities.ToolChoice = false
	_, err = NewRegistry(profiles, nil, Selection{Primary: "primary", Fallback: "fallback"})
	assert.ErrorContains(t, err, "tools and tool choice")
}

func TestRetryableErrorClassification(t *testing.T) {
	for _, kind := range []ErrorKind{ErrorTimeout, ErrorRateLimit, ErrorQuota, ErrorService, ErrorMalformed, ErrorEmptyResponse} {
		assert.True(t, Retryable(&Error{Kind: kind, Provider: ProviderOpenRouter}), string(kind))
	}
	for _, kind := range []ErrorKind{ErrorAuthentication, ErrorAuthorization, ErrorCanceled, ErrorInvalidRequest, ErrorContextLimit, ErrorSafety} {
		assert.False(t, Retryable(&Error{Kind: kind, Provider: ProviderOpenRouter}), string(kind))
	}
}

func TestTransportErrorDoesNotRetainRawNetworkDetails(t *testing.T) {
	err := transportError(ProviderOpenRouter, errors.New("Post https://openrouter.example/chat?token=secret: connection failed"))
	assert.ErrorContains(t, err, "provider request failed")
	assert.NotContains(t, err.Error(), "https://")
	assert.NotContains(t, err.Error(), "secret")
	deadlineErr := transportError(ProviderOpenRouter, context.DeadlineExceeded)
	assert.ErrorIs(t, deadlineErr, context.DeadlineExceeded)
}

func TestOpenRouterDiscoversToolsAndMapsLegacyModelRole(t *testing.T) {
	var chat compatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/models/user":
			_, _ = w.Write([]byte(`{"data":[{"id":"vendor/model","context_length":32768,"supported_parameters":["max_tokens","tools","tool_choice","reasoning"],"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"top_provider":{"max_completion_tokens":4096}}]}`))
		case "/models/vendor/model/endpoints":
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"context_length":32768,"max_completion_tokens":4096,"supported_parameters":["max_tokens","tools","tool_choice","reasoning"]}]}}`))
		case "/chat/completions":
			assert.Equal(t, "enabled", request.Header.Get("X-OpenRouter-Metadata"))
			require.NoError(t, json.NewDecoder(request.Body).Decode(&chat))
			_, _ = w.Write([]byte(`{"id":"gen-123","model":"vendor/upstream-model","provider":"Fallback Provider","openrouter_metadata":{"attempt":2,"endpoints":{"available":[{"provider":"Actual Provider","model":"vendor/actual-model","selected":true}]},"attempts":[{"provider":"First Provider","model":"vendor/first","status":503},{"provider":"Actual Provider","model":"vendor/actual-model","status":200}],"pipeline":[{"type":"context_compression","name":"context-compression"}]},"choices":[{"finish_reason":"stop","native_finish_reason":"eos","message":{"content":"answer"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	host, prober, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: "secret", BaseURL: server.URL})
	require.NoError(t, err)
	profile := Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model"}
	caps, err := prober.Probe(context.Background(), profile)
	require.NoError(t, err)
	assert.Equal(t, Capabilities{Tools: true, ToolChoice: true, Images: true, Reasoning: true, ContextTokens: 32768, MaxOutputTokens: 4096}, caps)
	profile.Capabilities = caps

	response, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{{Role: Role("model"), Parts: []Part{{Text: "prior"}}}, TextMessage(RoleUser, "next")},
		MaxOutputTokens: 123, ReasoningEffort: ReasoningHigh,
	})
	require.NoError(t, err)
	assert.Equal(t, "answer", response.Text())
	assert.Equal(t, FinishMetadata{Reason: "stop", NativeReason: "eos"}, response.Finish)
	require.Len(t, chat.Messages, 2)
	assert.Equal(t, RoleAssistant, chat.Messages[0].Role)
	assert.Equal(t, 123, chat.MaxTokens)
	assert.Nil(t, chat.Reasoning, "generic reasoning support does not confirm effort controls")
	assert.Equal(t, ResponseMetadata{
		ConfiguredProvider: ProviderOpenRouter, ConfiguredProfile: "host", ActualModelID: "vendor/actual-model",
		ProviderRequestID: "gen-123", UpstreamProvider: "Actual Provider", RoutingAttempt: 2,
		RoutingAttempts: []RoutingAttemptMetadata{{Provider: "First Provider", Model: "vendor/first", Status: 503}, {Provider: "Actual Provider", Model: "vendor/actual-model", Status: 200}},
		PipelineStages:  []string{"context_compression:context-compression"},
	}, response.Metadata)
}

func TestOpenRouterRejectsToolsWithoutConfirmedCapability(t *testing.T) {
	requests := 0
	var payload compatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	host := newTestOpenRouterHost(t, server.URL)
	profile := Profile{
		Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model",
	}
	_, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 32,
	})
	require.NoError(t, err)
	assert.Empty(t, payload.Tools)
	assert.Nil(t, payload.ToolChoice)

	_, err = host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "call it")}, MaxOutputTokens: 32,
		Tools: []ToolDefinition{{Name: "read", InputSchema: JSONSchema{"type": "object"}}},
	})
	assert.ErrorContains(t, err, "effective tool routing")
	assert.Equal(t, 1, requests)
}

func TestCatalogCapabilitiesDoNotGuessToolSupport(t *testing.T) {
	assert.False(t, catalogCapabilities([]string{"tool_choice"}, nil).Tools)
	assert.True(t, catalogCapabilities([]string{"tools"}, nil).Tools)
}

func TestOpenRouterGemmaUsesAdvertisedTokenFieldWithoutUnconfirmedReasoningEffort(t *testing.T) {
	var chat compatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/models/user":
			_, _ = w.Write([]byte(`{"data":[{"id":"google/gemma-4-31b-it:free","context_length":262144,"supported_parameters":["include_reasoning","max_tokens","reasoning","response_format","seed","temperature","tool_choice","tools","top_p"],"architecture":{"input_modalities":["image","text","video"],"output_modalities":["text"]},"top_provider":{"max_completion_tokens":32768},"reasoning":{"mandatory":false,"default_enabled":false}}]}`))
		case "/models/google/gemma-4-31b-it:free/endpoints":
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"context_length":262144,"max_completion_tokens":32768,"supported_parameters":["max_tokens","tools","tool_choice","reasoning"]}]}}`))
		case "/chat/completions":
			require.NoError(t, json.NewDecoder(request.Body).Decode(&chat))
			_, _ = w.Write([]byte(`{"model":"google/gemma-4-31b-it:free","choices":[{"finish_reason":"stop","message":{"content":"Jarvis version"}}]}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	host, prober, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: "secret", BaseURL: server.URL})
	require.NoError(t, err)
	profile := Profile{Name: "default", Provider: ProviderOpenRouter, ModelID: "google/gemma-4-31b-it:free"}
	profile.Capabilities, err = prober.Probe(context.Background(), profile)
	require.NoError(t, err)
	assert.True(t, profile.Capabilities.Tools)
	assert.True(t, profile.Capabilities.Reasoning)
	assert.False(t, profile.Capabilities.ReasoningControls)

	tools := make([]ToolDefinition, 9)
	for i := range tools {
		tools[i] = ToolDefinition{Name: fmt.Sprintf("tool_%d", i), InputSchema: JSONSchema{"type": "object"}}
	}
	_, err = host.Generate(context.Background(), Request{
		Profile: profile, System: "system", Messages: []Message{TextMessage(RoleUser, "what version are you")},
		MaxOutputTokens: 8192, ReasoningEffort: ReasoningHigh, Tools: tools,
	})
	require.NoError(t, err)
	assert.Equal(t, 8192, chat.MaxTokens)
	assert.Nil(t, chat.Reasoning)
	assert.Len(t, chat.Tools, 9)
	require.NotNil(t, chat.Provider)
	assert.True(t, chat.Provider.RequireParameters)
}

func TestOpenRouterProbeUsesUniversalTokenFieldWhenCatalogOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/models/user":
			_, _ = w.Write([]byte(`{"data":[{"id":"vendor/model","supported_parameters":["tools"],"architecture":{"output_modalities":["text"]}}]}`))
		case "/models/vendor/model/endpoints":
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"context_length":8192}]}}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	_, prober, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: "secret", BaseURL: server.URL})
	require.NoError(t, err)
	caps, err := prober.Probe(context.Background(), Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model"})
	require.NoError(t, err)
	assert.Equal(t, 8192, caps.ContextTokens)
}

func TestOpenRouterUsesUniversalMaxTokensAndConfirmedReasoningEffort(t *testing.T) {
	var chat compatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/models/user" {
			_, _ = w.Write([]byte(`{"data":[{"id":"vendor/model","supported_parameters":["max_completion_tokens","reasoning"],"reasoning":{"supported_efforts":["high"]}}]}`))
			return
		}
		if request.URL.Path == "/models/vendor/model/endpoints" {
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"context_length":8192,"max_completion_tokens":1024,"supported_parameters":["max_tokens","reasoning"]}]}}`))
			return
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&chat))
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	host, prober, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: "secret", BaseURL: server.URL})
	require.NoError(t, err)
	profile := Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model"}
	profile.Capabilities, err = prober.Probe(context.Background(), profile)
	require.NoError(t, err)
	assert.True(t, profile.Capabilities.ReasoningControls)
	_, err = host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 32, ReasoningEffort: ReasoningHigh,
	})
	require.NoError(t, err)
	assert.Equal(t, 32, chat.MaxTokens)
	require.NotNil(t, chat.Reasoning)
	assert.Equal(t, ReasoningHigh, chat.Reasoning.Effort)
	require.NotNil(t, chat.Provider)
	assert.True(t, chat.Provider.RequireParameters)
}

func TestOpenRouterProbeUsesOneCredentialVisibleEndpointCoherently(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/models/user":
			_, _ = w.Write([]byte(`{"data":[{"id":"google/gemma-3-27b-it","context_length":262144,"supported_parameters":["max_tokens","tools","tool_choice","reasoning"],"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"top_provider":{"max_completion_tokens":32768}}]}`))
		case "/models/google/gemma-3-27b-it/endpoints":
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"context_length":131072,"max_completion_tokens":16384,"supported_parameters":["max_tokens"]},{"context_length":98304,"max_completion_tokens":0}]}}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	_, prober, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: "secret", BaseURL: server.URL})
	require.NoError(t, err)
	caps, err := prober.Probe(context.Background(), Profile{Name: "gemma", Provider: ProviderOpenRouter, ModelID: "google/gemma-3-27b-it"})
	require.NoError(t, err)
	assert.Equal(t, 131072, caps.ContextTokens)
	assert.Equal(t, 16384, caps.MaxOutputTokens)
	assert.True(t, caps.Images)
	assert.False(t, caps.Tools)
	assert.False(t, caps.ToolChoice)
	assert.False(t, caps.Reasoning)
	assert.False(t, caps.ReasoningControls)
}

func TestOpenRouterProbeRejectsNonTextOutputAndMissingEndpoints(t *testing.T) {
	for _, test := range []struct {
		name, model, endpoints, want string
	}{
		{name: "non-text output", model: `{"id":"vendor/model","architecture":{"output_modalities":["image"]}}`, want: "does not provide text output"},
		{name: "missing endpoints", model: `{"id":"vendor/model","architecture":{"output_modalities":["text"]}}`, endpoints: `{"data":{"endpoints":[]}}`, want: "no available OpenRouter endpoint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/models/user":
					_, _ = w.Write([]byte(`{"data":[` + test.model + `]}`))
				case "/models/vendor/model/endpoints":
					_, _ = w.Write([]byte(test.endpoints))
				default:
					http.NotFound(w, request)
				}
			}))
			defer server.Close()
			_, prober, err := NewOpenRouterHost(OpenAICompatibleConfig{APIKey: "secret", BaseURL: server.URL})
			require.NoError(t, err)
			_, err = prober.Probe(context.Background(), Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model"})
			assert.ErrorContains(t, err, test.want)
		})
	}
}

func TestNVIDIAUsesBearerAndProviderTokenField(t *testing.T) {
	var payload compatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer nv-key", request.Header.Get("Authorization"))
		if request.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"meta/model"}]}`))
			return
		}
		require.Equal(t, "/chat/completions", request.URL.Path)
		require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	host, prober, err := NewNVIDIAHost(OpenAICompatibleConfig{APIKey: "nv-key", BaseURL: server.URL})
	require.NoError(t, err)
	capabilities, err := prober.Probe(context.Background(), Profile{Name: "nim", Provider: ProviderNVIDIANIM, ModelID: "meta/model"})
	require.NoError(t, err)
	assert.Equal(t, Capabilities{}, capabilities)
	_, err = host.Generate(context.Background(), Request{
		Profile:  Profile{Name: "nim", Provider: ProviderNVIDIANIM, ModelID: "meta/model"},
		Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 77,
	})
	require.NoError(t, err)
	assert.Equal(t, 77, payload.MaxTokens)
}

func TestVertexConvertsImagesToolsAndThoughtFreeOutput(t *testing.T) {
	var contents []*googlegenai.Content
	var config *googlegenai.GenerateContentConfig
	host, prober, err := NewVertexHost(context.Background(), VertexConfig{
		Generate: func(_ context.Context, _ string, got []*googlegenai.Content, gotConfig *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			contents, config = got, gotConfig
			return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{
				FinishReason: googlegenai.FinishReasonStop,
				Content:      &googlegenai.Content{Parts: []*googlegenai.Part{{Text: "private", Thought: true}, {Text: "visible"}}},
			}}}, nil
		},
		GetModel: func(context.Context, string, *googlegenai.GetModelConfig) (*googlegenai.Model, error) {
			return &googlegenai.Model{SupportedActions: []string{"generateContent"}, Thinking: true}, nil
		},
	})
	require.NoError(t, err)
	profile := Profile{Name: "vertex", Provider: ProviderVertex, ModelID: "gemini", Capabilities: Capabilities{Tools: true, ToolChoice: true, Images: true, Reasoning: true}}
	caps, err := prober.Probe(context.Background(), profile)
	require.NoError(t, err)
	assert.True(t, caps.Tools)
	response, err := host.Generate(context.Background(), Request{
		Profile:  profile,
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Text: "look"}, {Image: &Image{Data: []byte("image"), MIMEType: "image/png"}}}}},
		Tools:    []ToolDefinition{{Name: "read", InputSchema: JSONSchema{"type": "object"}}}, ReasoningEffort: ReasoningHigh, MaxOutputTokens: 128,
	})
	require.NoError(t, err)
	assert.Equal(t, "visible", response.Text())
	require.Len(t, contents, 1)
	assert.Len(t, contents[0].Parts, 2)
	require.Len(t, config.Tools, 1)
	assert.Equal(t, "read", config.Tools[0].FunctionDeclarations[0].Name)
}

func TestVertexProbeConfirmsToolsWithNonGenerativeCountTokens(t *testing.T) {
	countCalls := 0
	_, prober, err := NewVertexHost(context.Background(), VertexConfig{
		Generate: func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			return nil, errors.New("generation must not run during probe")
		},
		GetModel: func(context.Context, string, *googlegenai.GetModelConfig) (*googlegenai.Model, error) {
			return &googlegenai.Model{Thinking: true}, nil
		},
		CountTokens: func(_ context.Context, _ string, contents []*googlegenai.Content, config *googlegenai.CountTokensConfig) (*googlegenai.CountTokensResponse, error) {
			countCalls++
			require.Len(t, contents, 1)
			if countCalls == 1 {
				require.Len(t, config.Tools, 1)
				require.Len(t, config.Tools[0].FunctionDeclarations, 1)
				assert.Equal(t, "jarvis_capability_probe", config.Tools[0].FunctionDeclarations[0].Name)
			} else {
				assert.Empty(t, config.Tools)
				require.Len(t, contents[0].Parts, 1)
				assert.Equal(t, "image/png", contents[0].Parts[0].InlineData.MIMEType)
			}
			return &googlegenai.CountTokensResponse{TotalTokens: 1}, nil
		},
	})
	require.NoError(t, err)
	caps, err := prober.Probe(context.Background(), Profile{Name: "tool", Provider: ProviderVertex, ModelID: "gemini"})
	require.NoError(t, err)
	assert.Equal(t, 2, countCalls)
	assert.True(t, caps.Tools)
	assert.True(t, caps.ToolChoice)
	assert.True(t, caps.Images)
}

func TestProviderNeutralToolChoiceMapsAcrossAdapters(t *testing.T) {
	openRouter := newTestOpenRouterHost(t, "https://openrouter.invalid").(*openAICompatibleHost)
	profile := Profile{
		Name: "tools", Provider: ProviderOpenRouter, ModelID: "vendor/model",
		Capabilities: Capabilities{Tools: true, ToolChoice: true},
	}
	tool := ToolDefinition{Name: "read", InputSchema: JSONSchema{"type": "object"}}
	automatic, err := openRouter.convertRequest(Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "read")}, MaxOutputTokens: 32,
		Tools: []ToolDefinition{tool}, ToolChoice: ToolChoice{Mode: ToolChoiceAutomatic},
	})
	require.NoError(t, err)
	assert.Equal(t, "auto", automatic.ToolChoice)
	required, err := openRouter.convertRequest(Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "read")}, MaxOutputTokens: 32,
		Tools: []ToolDefinition{tool}, ToolChoice: ToolChoice{Mode: ToolChoiceRequired},
	})
	require.NoError(t, err)
	assert.Equal(t, "required", required.ToolChoice)
	disabled, err := openRouter.convertRequest(Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "read")}, MaxOutputTokens: 32,
		Tools: []ToolDefinition{tool}, ToolChoice: ToolChoice{Mode: ToolChoiceDisabled},
	})
	require.NoError(t, err)
	assert.Equal(t, "none", disabled.ToolChoice)
	named, err := openRouter.convertRequest(Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "read")}, MaxOutputTokens: 32,
		Tools: []ToolDefinition{tool}, ToolChoice: ToolChoice{Mode: ToolChoiceFunction, FunctionName: "read"},
	})
	require.NoError(t, err)
	assert.Equal(t, "read", named.ToolChoice.(compatibleSpecificToolChoice).Function.Name)

	var configs []*googlegenai.GenerateContentConfig
	vertex, _, err := NewVertexHost(context.Background(), VertexConfig{
		Generate: func(_ context.Context, _ string, _ []*googlegenai.Content, config *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			configs = append(configs, config)
			return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{Content: &googlegenai.Content{Parts: []*googlegenai.Part{{Text: "ok"}}}}}}, nil
		},
		GetModel: func(context.Context, string, *googlegenai.GetModelConfig) (*googlegenai.Model, error) {
			return &googlegenai.Model{}, nil
		},
	})
	require.NoError(t, err)
	vertexProfile := profile
	vertexProfile.Provider = ProviderVertex
	for _, choice := range []ToolChoice{
		{Mode: ToolChoiceAutomatic}, {Mode: ToolChoiceRequired}, {Mode: ToolChoiceDisabled},
		{Mode: ToolChoiceFunction, FunctionName: "read"},
	} {
		_, err = vertex.Generate(context.Background(), Request{
			Profile: vertexProfile, Messages: []Message{TextMessage(RoleUser, "read")}, MaxOutputTokens: 32,
			Tools: []ToolDefinition{tool}, ToolChoice: choice,
		})
		require.NoError(t, err)
	}
	require.Len(t, configs, 4)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAuto, configs[0].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAny, configs[1].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, configs[2].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAny, configs[3].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, []string{"read"}, configs[3].ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
}

func TestOpenRouterPreservesReasoningAndNullContentAcrossToolRound(t *testing.T) {
	var requests []compatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload compatibleRequest
		require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		requests = append(requests, payload)
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"model":"vendor/reasoning","choices":[{"finish_reason":"tool_calls","message":{"content":null,"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque","index":0}],"tool_calls":[{"id":"call-1","type":"function","extra_content":{"google":{"thought_signature":"signed"}},"function":{"name":"read","arguments":"{\"key\":\"value\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"vendor/reasoning","choices":[{"finish_reason":"stop","message":{"content":"final answer"}}]}`))
	}))
	defer server.Close()

	host := newTestOpenRouterHost(t, server.URL)
	profile := Profile{Name: "reasoning", Provider: ProviderOpenRouter, ModelID: "vendor/reasoning", Capabilities: Capabilities{
		Tools: true, ToolChoice: true, Reasoning: true,
	}}
	tool := ToolDefinition{Name: "read", InputSchema: JSONSchema{"type": "object"}, Effect: ToolEffectReadOnly}
	first, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "read it")}, MaxOutputTokens: 128,
		ReasoningEffort: ReasoningMedium, Tools: []ToolDefinition{tool},
	})
	require.NoError(t, err)
	require.Len(t, first.Message.ToolCalls, 1)
	require.NotNil(t, first.Message.Continuation)
	assert.Empty(t, first.Message.Text())

	result := ToolResult{CallID: "call-1", Name: "read", Output: map[string]any{"value": "done"}}
	second, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{
			TextMessage(RoleUser, "read it"), first.Message, {Role: RoleTool, ToolResult: &result},
		},
		MaxOutputTokens: 128, ReasoningEffort: ReasoningMedium, Tools: []ToolDefinition{tool},
	})
	require.NoError(t, err)
	assert.Equal(t, "final answer", second.Text())
	require.Len(t, requests, 2)
	require.Len(t, requests[1].Messages, 3)
	assert.Nil(t, requests[1].Messages[1].Content, "assistant tool-call content must remain JSON null")
	assert.JSONEq(t, `[{"type":"reasoning.encrypted","data":"opaque","index":0}]`, string(requests[1].Messages[1].ReasoningDetails))
	require.Len(t, requests[1].Messages[1].ToolCalls, 1)
	assert.JSONEq(t, `{"google":{"thought_signature":"signed"}}`, string(requests[1].Messages[1].ToolCalls[0].ExtraContent))
	assert.Len(t, requests[1].Tools, 1, "tool schemas must remain available on continuation rounds")
}

func TestVertexPreservesSignedContentAcrossToolRound(t *testing.T) {
	profile := Profile{Name: "vertex", Provider: ProviderVertex, ModelID: "gemini", Capabilities: Capabilities{
		Tools: true, ToolChoice: true, Images: true, Reasoning: true,
	}}
	var calls int
	host, _, err := NewVertexHost(context.Background(), VertexConfig{
		Generate: func(_ context.Context, _ string, contents []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			calls++
			if calls == 1 {
				return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{
					FinishReason: googlegenai.FinishReasonStop,
					Content: &googlegenai.Content{Role: "model", Parts: []*googlegenai.Part{
						{Text: "private", Thought: true, ThoughtSignature: []byte("thought-signature")},
						{Text: "Working."},
						{FunctionCall: &googlegenai.FunctionCall{ID: "call-1", Name: "read", Args: map[string]any{"key": "value"}}, ThoughtSignature: []byte("call-signature")},
					}},
				}}}, nil
			}
			require.Len(t, contents, 3)
			replayed := contents[1]
			assert.Equal(t, "model", replayed.Role)
			require.Len(t, replayed.Parts, 3)
			assert.True(t, replayed.Parts[0].Thought)
			assert.Equal(t, []byte("thought-signature"), replayed.Parts[0].ThoughtSignature)
			assert.Equal(t, "Working.", replayed.Parts[1].Text)
			require.NotNil(t, replayed.Parts[2].FunctionCall)
			assert.Equal(t, []byte("call-signature"), replayed.Parts[2].ThoughtSignature)
			return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{
				FinishReason: googlegenai.FinishReasonStop,
				Content:      &googlegenai.Content{Role: "model", Parts: []*googlegenai.Part{{Text: "final"}}},
			}}}, nil
		},
		GetModel: func(context.Context, string, *googlegenai.GetModelConfig) (*googlegenai.Model, error) {
			return &googlegenai.Model{}, nil
		},
	})
	require.NoError(t, err)
	tool := ToolDefinition{Name: "read", InputSchema: JSONSchema{"type": "object"}}
	first, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "read it")}, MaxOutputTokens: 128, ReasoningEffort: ReasoningMedium, Tools: []ToolDefinition{tool},
	})
	require.NoError(t, err)
	assert.Equal(t, "Working.", first.Text())
	require.NotNil(t, first.Message.Continuation)
	result := ToolResult{CallID: "call-1", Name: "read", Output: "done"}
	second, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "read it"), first.Message, {Role: RoleTool, ToolResult: &result}},
		MaxOutputTokens: 128, ReasoningEffort: ReasoningMedium, Tools: []ToolDefinition{tool},
	})
	require.NoError(t, err)
	assert.Equal(t, "final", second.Text())
}

func TestOpenRouterUsesKnownLimitsAndConfirmedOptionalParameters(t *testing.T) {
	var payload compatibleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	host := newTestOpenRouterHost(t, server.URL)
	profile := Profile{Name: "limited", Provider: ProviderOpenRouter, ModelID: "vendor/model", Capabilities: Capabilities{Tools: true, MaxOutputTokens: 64}}
	_, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 100, ReasoningEffort: ReasoningHigh,
		Tools: []ToolDefinition{{Name: "read", InputSchema: JSONSchema{"type": "object"}}},
	})
	require.NoError(t, err)
	assert.Equal(t, 64, payload.MaxTokens)
	assert.Nil(t, payload.Reasoning, "unconfirmed reasoning must not be requested")
	assert.Empty(t, payload.ToolChoice, "unconfirmed tool_choice must not be requested")
}

func TestOpenAICompatibleRejectsInvalidToolSchemaLocally(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	host := newTestOpenRouterHost(t, server.URL)
	_, err := host.Generate(context.Background(), Request{
		Profile:  Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model", Capabilities: Capabilities{Tools: true}},
		Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 32,
		Tools: []ToolDefinition{{Name: "read", InputSchema: JSONSchema{
			"type": "object", "required": []string{"missing"}, "properties": map[string]any{},
		}}},
	})
	var modelErr *Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, "invalid_tool_schema", modelErr.ErrorType)
	assert.False(t, called)
}

func TestOpenRouterUntypedBadRequestCarriesSafeShapeAndFallbackHint(t *testing.T) {
	metadataHeader := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		metadataHeader = request.Header.Get("X-OpenRouter-Metadata")
		w.Header().Set("X-Generation-Id", "gen-123")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Provider returned error"}}`))
	}))
	defer server.Close()
	host := newTestOpenRouterHost(t, server.URL)
	_, err := host.Generate(context.Background(), Request{
		Profile: Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model", Capabilities: Capabilities{Tools: true, Reasoning: true}},
		System:  "private system", Messages: []Message{TextMessage(RoleUser, "private prompt")}, MaxOutputTokens: 32,
		ReasoningEffort: ReasoningHigh, Tools: []ToolDefinition{{Name: "read", InputSchema: JSONSchema{"type": "object"}}},
	})
	var modelErr *Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, "enabled", metadataHeader)
	assert.Equal(t, ErrorInvalidRequest, modelErr.Kind)
	assert.Equal(t, "request_rejected", modelErr.ReasonCode)
	assert.Equal(t, "gen-123", modelErr.ProviderRequestID)
	assert.True(t, CrossProviderFallback(err))
	assert.Equal(t, "max_tokens", modelErr.Request.TokenLimitField)
	assert.Equal(t, 32, modelErr.Request.EffectiveMaxOutputTokens)
	assert.True(t, modelErr.Request.ReasoningRequested)
	assert.False(t, modelErr.Request.ReasoningSent)
	assert.Equal(t, 2, modelErr.Request.MessageCount)
	assert.Equal(t, 1, modelErr.Request.SystemMessageCount)
	assert.Equal(t, 1, modelErr.Request.UserMessageCount)
	assert.Equal(t, 1, modelErr.Request.ToolSchemaCount)
	assert.NotEmpty(t, modelErr.Request.ToolSchemaFingerprint)
	require.Len(t, modelErr.Request.ToolSchemas, 1)
	assert.Equal(t, "read", modelErr.Request.ToolSchemas[0].Name)
	assert.NotEmpty(t, modelErr.Request.ToolSchemas[0].Fingerprint)
	assert.Positive(t, modelErr.Request.ProviderMessageBytes)
	assert.NotEmpty(t, modelErr.Request.ProviderMessageHash)
	assert.Positive(t, modelErr.Request.PayloadBytes)
	assert.NotContains(t, fmt.Sprint(modelErr.Request), "private")
}

func TestStandardWrappedErrorsPreserveCauseAndVerboseStack(t *testing.T) {
	cause := errors.New("sentinel cause")
	wrapped := stderrors.Wrap(cause, "validate provider request")
	modelErr := &Error{Kind: ErrorInvalidRequest, Provider: ProviderOpenRouter, Err: wrapped}
	assert.ErrorIs(t, modelErr, cause)
	assert.Contains(t, modelErr.Error(), "caused by")
	assert.Contains(t, fmt.Sprintf("%+v", wrapped), "llm_test.go")
}

func TestOpenRouterClassifiesRefusalAndFinishErrorWithSafeDraft(t *testing.T) {
	for _, test := range []struct {
		name, body, text string
		kind             ErrorKind
	}{
		{
			name: "refusal", body: `{"choices":[{"finish_reason":"content_filter","native_finish_reason":"safety","message":{"content":null,"refusal":"I can't help with that."}}]}`,
			text: "I can't help with that.", kind: ErrorSafety,
		},
		{
			name: "finish error without typed choice error", body: `{"choices":[{"finish_reason":"error","native_finish_reason":"upstream_disconnect","message":{"content":"A partial answer."}}]}`,
			text: "A partial answer.", kind: ErrorService,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			host := newTestOpenRouterHost(t, server.URL)
			response, err := host.Generate(context.Background(), Request{
				Profile:  Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model"},
				Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 32,
			})
			require.Error(t, err)
			assert.Equal(t, test.text, response.Text())
			assert.NotEmpty(t, response.Finish.NativeReason)
			var modelErr *Error
			require.ErrorAs(t, err, &modelErr)
			assert.Equal(t, test.kind, modelErr.Kind)
		})
	}
}

func TestOpenRouterCapturesRetryAfterWithoutProviderBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"private rate detail","metadata":{"error_type":"rate_limit_exceeded"}}}`))
	}))
	defer server.Close()
	host := newTestOpenRouterHost(t, server.URL)
	_, err := host.Generate(context.Background(), Request{
		Profile:  Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model"},
		Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 32,
	})
	require.Error(t, err)
	var modelErr *Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, 17, modelErr.RetryAfterSeconds)
	assert.NotContains(t, err.Error(), "private rate detail")
}

func TestOpenRouterClassifiesTypedAndChoiceErrorsWithoutLeakingMetadata(t *testing.T) {
	tests := []struct {
		name, body, errorType, providerCode, partial string
		status, providerStatus                       int
		kind                                         ErrorKind
		retryable                                    bool
	}{
		{
			name: "body rate limit with outer success", status: http.StatusOK, providerStatus: http.StatusTooManyRequests,
			body:      `{"error":{"code":429,"message":"secret prompt value","metadata":{"error_type":"rate_limit_exceeded","provider_code":"rate_limited","raw":"secret prompt value"}}}`,
			errorType: "rate_limit_exceeded", providerCode: "rate_limited", kind: ErrorRateLimit, retryable: true,
		},
		{
			name: "choice service failure preserves partial", status: http.StatusOK, providerStatus: http.StatusBadGateway,
			body:      `{"choices":[{"finish_reason":"error","message":{"content":"usable draft"},"error":{"code":502,"message":"secret response","metadata":{"error_type":"provider_unavailable"}}}]}`,
			errorType: "provider_unavailable", kind: ErrorService, retryable: true, partial: "usable draft",
		},
		{
			name: "invalid prompt is permanent", status: http.StatusBadRequest, providerStatus: http.StatusBadRequest,
			body:      `{"error":{"code":400,"message":"Provider returned error","metadata":{"error_type":"invalid_prompt","provider_code":{"secret":"must not appear"}}}}`,
			errorType: "invalid_prompt", kind: ErrorInvalidRequest,
		},
		{
			name: "undecodable bad request remains permanent", status: http.StatusBadRequest, providerStatus: http.StatusBadRequest,
			body:      `<html>secret upstream response</html>`,
			errorType: "undecodable_error_response", kind: ErrorInvalidRequest,
		},
		{
			name: "URL-shaped provider code is discarded", status: http.StatusBadRequest, providerStatus: http.StatusBadRequest,
			body:      `{"error":{"code":400,"message":"Provider returned error","metadata":{"error_type":"invalid_prompt","provider_code":"https://secret.example/payload"}}}`,
			errorType: "invalid_prompt", kind: ErrorInvalidRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			host := newTestOpenRouterHost(t, server.URL)
			response, err := host.Generate(context.Background(), Request{
				Profile:  Profile{Name: "host", Provider: ProviderOpenRouter, ModelID: "vendor/model"},
				Messages: []Message{TextMessage(RoleUser, "hello")}, MaxOutputTokens: 32,
			})
			require.Error(t, err)
			var modelErr *Error
			require.ErrorAs(t, err, &modelErr)
			assert.Equal(t, test.kind, modelErr.Kind)
			assert.Equal(t, test.status, modelErr.StatusCode)
			assert.Equal(t, test.providerStatus, modelErr.ProviderStatusCode)
			assert.Equal(t, test.errorType, modelErr.ErrorType)
			assert.Equal(t, test.providerCode, modelErr.ProviderCode)
			assert.Equal(t, test.retryable, modelErr.Retryable())
			assert.Equal(t, test.partial, response.Text())
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

type failingProber struct{ err error }

func (p failingProber) Probe(context.Context, Profile) (Capabilities, error) {
	return Capabilities{}, p.err
}

func TestProbeProfilesAggregatesIndependentFailures(t *testing.T) {
	profiles := []Profile{{Name: "a", Provider: ProviderOpenRouter, ModelID: "a"}, {Name: "b", Provider: ProviderNVIDIANIM, ModelID: "b"}}
	_, err := ProbeProfiles(context.Background(), profiles, map[Provider]Prober{
		ProviderOpenRouter: failingProber{err: errors.New("catalog unavailable")},
		ProviderNVIDIANIM:  failingProber{err: errors.New("authentication failed")},
	}, time.Second)
	assert.ErrorContains(t, err, "openrouter:a")
	assert.ErrorContains(t, err, "nvidia-nim:b")
}
