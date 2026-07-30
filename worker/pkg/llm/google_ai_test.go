package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegenai "google.golang.org/genai"
)

type googleAIRoundTripFunc func(*http.Request) (*http.Response, error)

func (f googleAIRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func googleAIJSONResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestGoogleAIUsesGeminiBackendAndExactCapabilityProbes(t *testing.T) {
	const modelID = "gemini-3.1-flash-lite"
	type recordedRequest struct {
		method, host, path, rawQuery, authorization, apiKey string
		body                                                []byte
	}
	var requests []recordedRequest
	client := &http.Client{Transport: googleAIRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		requests = append(requests, recordedRequest{
			method: request.Method, host: request.URL.Host, path: request.URL.EscapedPath(), rawQuery: request.URL.RawQuery,
			authorization: request.Header.Get("Authorization"), apiKey: request.Header.Get("x-goog-api-key"), body: body,
		})
		if request.Method == http.MethodGet {
			return googleAIJSONResponse(request, http.StatusOK, `{
				"name":"models/gemini-3.1-flash-lite",
				"inputTokenLimit":1048576,
				"outputTokenLimit":65536,
				"supportedGenerationMethods":["generateContent","countTokens"],
				"thinking":true
			}`), nil
		}
		return googleAIJSONResponse(request, http.StatusOK, `{"totalTokens":1}`), nil
	})}

	_, prober, err := NewGoogleAIHost(context.Background(), GoogleAIConfig{APIKey: "secret-key", HTTPClient: client})
	require.NoError(t, err)
	capabilities, err := prober.Probe(context.Background(), Profile{Name: "google", Provider: ProviderGoogleAI, ModelID: modelID})
	require.NoError(t, err)
	assert.Equal(t, Capabilities{
		Tools: true, ToolChoice: true, Images: true, Reasoning: true, ReasoningControls: true,
		MaxInputTokens: 1048576, MaxOutputTokens: 65536,
	}, capabilities)

	require.Len(t, requests, 6)
	assert.Equal(t, http.MethodGet, requests[0].method)
	assert.Equal(t, "/v1beta/models/"+modelID, requests[0].path)
	for _, request := range requests {
		assert.Equal(t, "generativelanguage.googleapis.com", request.host)
		assert.Equal(t, "secret-key", request.apiKey)
		assert.Empty(t, request.authorization)
		assert.Empty(t, request.rawQuery)
		assert.NotContains(t, request.path, "projects/")
	}
	for _, request := range requests[1:] {
		assert.Equal(t, http.MethodPost, request.method)
		assert.Equal(t, "/v1beta/models/"+modelID+":countTokens", request.path)
	}

	assert.JSONEq(t, `{
		"generateContentRequest": {
			"model": "models/gemini-3.1-flash-lite",
			"contents": [{"role":"user","parts":[{"text":"capability probe"}]}],
			"tools": [{"functionDeclarations":[{
				"name":"jarvis_capability_probe",
				"description":"Harmless startup capability declaration.",
				"parametersJsonSchema":{"type":"object","properties":{}}
			}]}],
			"toolConfig":{"functionCallingConfig":{
				"mode":"ANY","allowedFunctionNames":["jarvis_capability_probe"]
			}}
		}
	}`, string(requests[1].body))
	for index, level := range []string{"LOW", "MEDIUM", "HIGH"} {
		expected := `{
			"generateContentRequest": {
				"model":"models/gemini-3.1-flash-lite",
				"contents":[{"role":"user","parts":[{"text":"capability probe"}]}],
				"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}}
			}
		}`
		assert.JSONEq(t, expected, string(requests[index+2].body))
	}
	assert.JSONEq(t, `{
		"contents":[{"role":"user","parts":[{"inlineData":{
			"data":"`+base64.StdEncoding.EncodeToString(vertexCapabilityProbePNG)+`","mimeType":"image/png"
		}}]}]
	}`, string(requests[5].body))
}

func TestGoogleAIModelValidationRequiresExplicitGeminiThreeOrNewer(t *testing.T) {
	accepted := []string{
		"gemini-3",
		"gemini-3.0",
		"gemini-3.1-flash-lite",
		"gemini-4-pro-preview",
		"gemini-12.2-flash",
	}
	for _, modelID := range accepted {
		t.Run("accept_"+modelID, func(t *testing.T) {
			err := validateGoogleAIModel(modelID, &googlegenai.Model{
				Name: "models/" + modelID, SupportedActions: []string{"generateContent"},
			})
			require.NoError(t, err)
		})
	}

	rejected := []string{
		"gemini-2.5-flash",
		"gemini-flash-latest",
		"gemini-3-latest",
		"gemini-three-flash",
		"gemini-03-flash",
		"opaque-model",
		"gemini-3-",
		"gemini-3.1-flash..preview",
	}
	for _, modelID := range rejected {
		t.Run("reject_"+modelID, func(t *testing.T) {
			err := validateGoogleAIModel(modelID, &googlegenai.Model{
				Name: "models/" + modelID, SupportedActions: []string{"generateContent"},
			})
			assert.ErrorContains(t, err, "explicit Gemini 3")
			var modelErr *Error
			require.ErrorAs(t, err, &modelErr)
			assert.Equal(t, ProviderGoogleAI, modelErr.Provider)
		})
	}

	assert.Error(t, validateGoogleAIModel("tunedModels/custom", &googlegenai.Model{
		Name: "tunedModels/custom", SupportedActions: []string{"generateContent"},
	}))
	assert.ErrorContains(t, validateGoogleAIModel("gemini-3.1-flash-lite", &googlegenai.Model{
		Name: "models/gemini-3.1-pro", SupportedActions: []string{"generateContent"},
	}), "does not match")
	assert.ErrorContains(t, validateGoogleAIModel("gemini-3.1-flash-lite", &googlegenai.Model{
		Name: "models/gemini-3.1-flash-lite",
	}), "generateContent")

	err := validateGoogleAIModel("gemini-3.1-flash-lite", &googlegenai.Model{})
	var modelErr *Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, ErrorMalformed, modelErr.Kind)
}

func TestGoogleAIConversionsMetadataAndContinuationStayProviderPinned(t *testing.T) {
	profile := Profile{
		Name: "google", Provider: ProviderGoogleAI, ModelID: "gemini-3.1-flash-lite",
		Capabilities: Capabilities{Tools: true, ToolChoice: true, Images: true, Reasoning: true, ReasoningControls: true},
	}
	var calls int
	var capturedContents [][]*googlegenai.Content
	var capturedConfigs []*googlegenai.GenerateContentConfig
	host := &googleGenAIHost{
		provider: ProviderGoogleAI, continuationFormat: googleAIContinuationFormat,
		generate: func(_ context.Context, modelID string, contents []*googlegenai.Content, config *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			calls++
			assert.Equal(t, profile.ModelID, modelID)
			capturedContents = append(capturedContents, contents)
			capturedConfigs = append(capturedConfigs, config)
			if calls == 1 {
				return &googlegenai.GenerateContentResponse{
					Candidates: []*googlegenai.Candidate{{
						FinishReason: googlegenai.FinishReasonStop,
						Content: &googlegenai.Content{Role: "model", Parts: []*googlegenai.Part{
							{Text: "private", Thought: true, ThoughtSignature: []byte("thought-signature")},
							{Text: "working"},
							{FunctionCall: &googlegenai.FunctionCall{ID: "call-1", Name: "read", Args: map[string]any{"key": "value"}}, ThoughtSignature: []byte("call-signature")},
						}},
					}},
					ModelVersion: "gemini-3.1-flash-lite-001", ResponseID: "request-123",
					UsageMetadata: &googlegenai.GenerateContentResponseUsageMetadata{
						PromptTokenCount: 10, CandidatesTokenCount: 4, ThoughtsTokenCount: 3, TotalTokenCount: 17,
					},
				}, nil
			}
			return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{
				FinishReason: googlegenai.FinishReasonStop,
				Content:      &googlegenai.Content{Role: "model", Parts: []*googlegenai.Part{{Text: "final"}}},
			}}}, nil
		},
	}
	tool := ToolDefinition{Name: "read", Description: "Read a value.", InputSchema: JSONSchema{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}}}
	first, err := host.Generate(context.Background(), Request{
		Profile: profile, System: "system instruction",
		Messages:        []Message{{Role: RoleUser, Parts: []Part{{Text: "read it"}, {Image: &Image{Data: []byte("image"), MIMEType: "image/png"}}}}},
		MaxOutputTokens: 128, ReasoningEffort: ReasoningHigh, Tools: []ToolDefinition{tool},
		ToolChoice: ToolChoice{Mode: ToolChoiceFunction, FunctionName: "read"},
	})
	require.NoError(t, err)
	assert.Equal(t, "working", first.Text())
	assert.Equal(t, Usage{InputTokens: 10, OutputTokens: 4, ReasoningTokens: 3, TotalTokens: 17}, first.Usage)
	assert.Equal(t, ResponseMetadata{
		ConfiguredProvider: ProviderGoogleAI, ConfiguredProfile: "google", ActualModelID: "gemini-3.1-flash-lite-001",
		ProviderRequestID: "request-123", UpstreamProvider: "google-ai",
	}, first.Metadata)
	require.NotNil(t, first.Message.Continuation)
	assert.Equal(t, googlegenai.ThinkingLevelHigh, capturedConfigs[0].ThinkingConfig.ThinkingLevel)
	assert.Equal(t, "system instruction", capturedConfigs[0].SystemInstruction.Parts[0].Text)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAny, capturedConfigs[0].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, []string{"read"}, capturedConfigs[0].ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
	require.Len(t, capturedContents[0][0].Parts, 2)

	result := ToolResult{CallID: "call-1", Name: "read", Output: map[string]any{"value": "done"}}
	second, err := host.Generate(context.Background(), Request{
		Profile: profile, Messages: []Message{
			TextMessage(RoleUser, "read it"), first.Message, {Role: RoleTool, ToolResult: &result},
		},
		MaxOutputTokens: 128, ReasoningEffort: ReasoningMedium, Tools: []ToolDefinition{tool},
	})
	require.NoError(t, err)
	assert.Equal(t, "final", second.Text())
	require.Len(t, capturedContents[1], 3)
	replayed := capturedContents[1][1]
	require.Len(t, replayed.Parts, 3)
	assert.Equal(t, []byte("thought-signature"), replayed.Parts[0].ThoughtSignature)
	assert.Equal(t, []byte("call-signature"), replayed.Parts[2].ThoughtSignature)
	require.NotNil(t, capturedContents[1][2].Parts[0].FunctionResponse)
	assert.Equal(t, "call-1", capturedContents[1][2].Parts[0].FunctionResponse.ID)

	vertexProfile := profile
	vertexProfile.Provider = ProviderVertex
	assert.False(t, first.Message.Continuation.ReusableWith(vertexProfile))
	otherProfile := profile
	otherProfile.Name = "other"
	assert.False(t, first.Message.Continuation.ReusableWith(otherProfile))
}

func TestGoogleAIClassifiesFailuresWithoutProviderBodiesOrCredentials(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind ErrorKind
	}{
		{name: "canceled", err: context.Canceled, kind: ErrorCanceled},
		{name: "timeout", err: context.DeadlineExceeded, kind: ErrorTimeout},
		{name: "authentication", err: googlegenai.APIError{Code: http.StatusUnauthorized, Status: "UNAUTHENTICATED", Message: "secret key rejected"}, kind: ErrorAuthentication},
		{name: "authorization", err: googlegenai.APIError{Code: http.StatusForbidden, Status: "PERMISSION_DENIED", Message: "secret policy"}, kind: ErrorAuthorization},
		{name: "quota", err: googlegenai.APIError{Code: http.StatusTooManyRequests, Status: "RESOURCE_EXHAUSTED", Message: "secret quota exhausted"}, kind: ErrorQuota},
		{name: "rate limit", err: googlegenai.APIError{Code: http.StatusTooManyRequests, Status: "RESOURCE_EXHAUSTED", Message: "secret request rate"}, kind: ErrorRateLimit},
		{name: "invalid", err: googlegenai.APIError{Code: http.StatusBadRequest, Status: "INVALID_ARGUMENT", Message: "secret request body"}, kind: ErrorInvalidRequest},
		{name: "service", err: googlegenai.APIError{Code: http.StatusServiceUnavailable, Status: "UNAVAILABLE", Message: "secret backend"}, kind: ErrorService},
		{name: "malformed", err: errors.New("deserializeUnaryResponse: error unmarshalling response: secret body"), kind: ErrorMalformed},
		{name: "transport", err: errors.New("Post https://generativelanguage.googleapis.com?key=secret: connection failed"), kind: ErrorService},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyGoogleGenAIError(ProviderGoogleAI, test.err)
			var modelErr *Error
			require.ErrorAs(t, err, &modelErr)
			assert.Equal(t, ProviderGoogleAI, modelErr.Provider)
			assert.Equal(t, test.kind, modelErr.Kind)
			assert.NotContains(t, err.Error(), "secret")
			assert.NotContains(t, err.Error(), "https://")
		})
	}
}

func TestGoogleAIClassifiesSafetyResponses(t *testing.T) {
	profile := Profile{Name: "google", Provider: ProviderGoogleAI, ModelID: "gemini-3.1-flash-lite"}

	_, err := googleGenAIResponse(&googlegenai.GenerateContentResponse{
		PromptFeedback: &googlegenai.GenerateContentResponsePromptFeedback{BlockReason: googlegenai.BlockedReasonSafety},
	}, profile, ProviderGoogleAI, googleAIContinuationFormat)
	var modelErr *Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, ErrorSafety, modelErr.Kind)
	assert.Equal(t, ProviderGoogleAI, modelErr.Provider)

	_, err = googleGenAIResponse(&googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{
		FinishReason: googlegenai.FinishReasonSafety,
		Content:      &googlegenai.Content{Role: "model"},
	}}}, profile, ProviderGoogleAI, googleAIContinuationFormat)
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, ErrorSafety, modelErr.Kind)
}

func TestGoogleAIToolChoiceUsesSharedGoogleWire(t *testing.T) {
	var configs []*googlegenai.GenerateContentConfig
	host := &googleGenAIHost{
		provider: ProviderGoogleAI, continuationFormat: googleAIContinuationFormat,
		generate: func(_ context.Context, _ string, _ []*googlegenai.Content, config *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			configs = append(configs, config)
			return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{Content: &googlegenai.Content{Parts: []*googlegenai.Part{{Text: "ok"}}}}}}, nil
		},
	}
	profile := Profile{Name: "google", Provider: ProviderGoogleAI, ModelID: "gemini-3.1-flash-lite", Capabilities: Capabilities{Tools: true, ToolChoice: true}}
	tool := ToolDefinition{Name: "read", InputSchema: JSONSchema{"type": "object"}}
	for _, choice := range []ToolChoice{
		{Mode: ToolChoiceAutomatic}, {Mode: ToolChoiceRequired}, {Mode: ToolChoiceDisabled},
		{Mode: ToolChoiceFunction, FunctionName: "read"},
	} {
		_, err := host.Generate(context.Background(), Request{
			Profile: profile, Messages: []Message{TextMessage(RoleUser, "read")}, MaxOutputTokens: 32,
			Tools: []ToolDefinition{tool}, ToolChoice: choice,
		})
		require.NoError(t, err)
	}
	require.Len(t, configs, 4)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAuto, configs[0].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAny, configs[1].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, configs[2].ToolConfig.FunctionCallingConfig.Mode)
	assert.Equal(t, []string{"read"}, configs[3].ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
}

func TestGoogleAIProbeDowngradesOnlyUnsupportedCapabilities(t *testing.T) {
	countCalls := 0
	host := &googleGenAIHost{
		provider: ProviderGoogleAI, continuationFormat: googleAIContinuationFormat,
		getModel: func(context.Context, string, *googlegenai.GetModelConfig) (*googlegenai.Model, error) {
			return &googlegenai.Model{
				Name: "models/gemini-3.1-flash-lite", SupportedActions: []string{"generateContent"}, Thinking: true,
			}, nil
		},
		countTokens: func(_ context.Context, _ string, _ []*googlegenai.Content, config *googlegenai.CountTokensConfig) (*googlegenai.CountTokensResponse, error) {
			countCalls++
			if countCalls == 3 {
				return nil, googlegenai.APIError{Code: http.StatusBadRequest, Message: "thinking level is not supported"}
			}
			if countCalls == 5 {
				return nil, googlegenai.APIError{Code: http.StatusBadRequest, Message: "image input is not supported"}
			}
			assert.NotNil(t, config)
			return &googlegenai.CountTokensResponse{TotalTokens: 1}, nil
		},
	}
	capabilities, err := host.Probe(context.Background(), Profile{
		Name: "google", Provider: ProviderGoogleAI, ModelID: "gemini-3.1-flash-lite",
	})
	require.NoError(t, err)
	assert.True(t, capabilities.Tools)
	assert.True(t, capabilities.ToolChoice)
	assert.True(t, capabilities.Reasoning)
	assert.False(t, capabilities.ReasoningControls)
	assert.False(t, capabilities.Images)
	assert.Equal(t, 5, countCalls)

	host.countTokens = func(context.Context, string, []*googlegenai.Content, *googlegenai.CountTokensConfig) (*googlegenai.CountTokensResponse, error) {
		return nil, googlegenai.APIError{Code: http.StatusUnauthorized, Status: "UNAUTHENTICATED", Message: "private key rejected"}
	}
	_, err = host.Probe(context.Background(), Profile{
		Name: "google", Provider: ProviderGoogleAI, ModelID: "gemini-3.1-flash-lite",
	})
	var modelErr *Error
	require.ErrorAs(t, err, &modelErr)
	assert.Equal(t, ErrorAuthentication, modelErr.Kind)
	assert.NotContains(t, err.Error(), "private")
}

func TestGoogleAICredentialsAreRequiredAndRedacted(t *testing.T) {
	_, _, err := NewGoogleAIHost(context.Background(), GoogleAIConfig{})
	assert.ErrorContains(t, err, "API key is required")
	assert.NotContains(t, err.Error(), "GEMINI_API_KEY")

	request, err := http.NewRequest(http.MethodPost, googleAIBaseURL, bytes.NewReader(nil))
	require.NoError(t, err)
	providerErr := classifyGoogleGenAIError(ProviderGoogleAI, googlegenai.APIError{
		Code: http.StatusUnauthorized, Status: "UNAUTHENTICATED", Message: "credential secret-value failed",
	})
	encoded, err := json.Marshal(struct {
		URL string `json:"url"`
	}{URL: request.URL.String()})
	require.NoError(t, err)
	assert.NotContains(t, providerErr.Error()+string(encoded), "secret-value")
}
