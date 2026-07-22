package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/justinswe/std/errors"
	googlegenai "google.golang.org/genai"
)

// VertexGenerateFunc is the Google SDK generation boundary used by the adapter.
type VertexGenerateFunc func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error)

// VertexGetModelFunc is the non-generative model lookup boundary used at startup.
type VertexGetModelFunc func(context.Context, string, *googlegenai.GetModelConfig) (*googlegenai.Model, error)

// VertexCountTokensFunc is the non-generative tool capability probe boundary used at startup.
type VertexCountTokensFunc func(context.Context, string, []*googlegenai.Content, *googlegenai.CountTokensConfig) (*googlegenai.CountTokensResponse, error)

// VertexConfig configures the Vertex adapter. Function fields support deterministic tests.
type VertexConfig struct {
	ProjectID   string
	Location    string
	Generate    VertexGenerateFunc
	GetModel    VertexGetModelFunc
	CountTokens VertexCountTokensFunc
}

// GoogleAIConfig configures the Gemini Developer API adapter.
type GoogleAIConfig struct {
	APIKey     string
	HTTPClient *http.Client
}

const (
	googleAIBaseURL            = "https://generativelanguage.googleapis.com/"
	googleAIAPIVersion         = "v1beta"
	vertexContinuationFormat   = "vertex-content-v1"
	googleAIContinuationFormat = "google-ai-content-v1"
)

// NewVertexHost creates a Vertex host and active model prober.
func NewVertexHost(ctx context.Context, config VertexConfig) (Host, Prober, error) {
	if config.Generate == nil || config.GetModel == nil {
		if strings.TrimSpace(config.ProjectID) == "" {
			return nil, nil, errors.New("project ID is required for Vertex profiles")
		}
		location := strings.TrimSpace(config.Location)
		if location == "" {
			location = "global"
		}
		client, err := googlegenai.NewClient(ctx, &googlegenai.ClientConfig{
			Project: config.ProjectID, Location: location, Backend: googlegenai.BackendVertexAI,
		})
		if err != nil {
			return nil, nil, &Error{Kind: ErrorAuthentication, Provider: ProviderVertex, Err: err}
		}
		config.Generate = client.Models.GenerateContent
		config.GetModel = client.Models.Get
		config.CountTokens = client.Models.CountTokens
	}
	host := &googleGenAIHost{
		provider: ProviderVertex, continuationFormat: vertexContinuationFormat,
		generate: config.Generate, getModel: config.GetModel, countTokens: config.CountTokens,
	}
	return host, host, nil
}

// NewGoogleAIHost creates a Gemini Developer API host and active model prober.
func NewGoogleAIHost(ctx context.Context, config GoogleAIConfig) (Host, Prober, error) {
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return nil, nil, errors.New("Google AI API key is required for Google AI profiles")
	}
	client, err := googlegenai.NewClient(ctx, &googlegenai.ClientConfig{
		APIKey: apiKey, Backend: googlegenai.BackendGeminiAPI, HTTPClient: config.HTTPClient,
		HTTPOptions: googlegenai.HTTPOptions{BaseURL: googleAIBaseURL, APIVersion: googleAIAPIVersion},
	})
	if err != nil {
		return nil, nil, &Error{
			Kind: ErrorAuthentication, Provider: ProviderGoogleAI, Scope: "client",
			Err: errors.New("initialize Google AI client"),
		}
	}
	host := &googleGenAIHost{
		provider: ProviderGoogleAI, continuationFormat: googleAIContinuationFormat,
		generate: client.Models.GenerateContent, getModel: client.Models.Get, countTokens: client.Models.CountTokens,
	}
	return host, host, nil
}

type googleGenAIHost struct {
	provider           Provider
	continuationFormat string
	generate           VertexGenerateFunc
	getModel           VertexGetModelFunc
	countTokens        VertexCountTokensFunc
}

var explicitGeminiModelPattern = regexp.MustCompile(`^gemini-([1-9][0-9]*)(?:\.[0-9]+)?(?:-[a-z0-9]+(?:[.-][a-z0-9]+)*)?$`)

var vertexCapabilityProbePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x04, 0x00, 0x00, 0x00, 0xb5, 0x1c, 0x0c, 0x02, 0x00, 0x00, 0x00,
	0x0b, 0x49, 0x44, 0x41, 0x54, 0x78, 0xda, 0x63, 0xfc, 0xff, 0x1f, 0x00,
	0x02, 0xeb, 0x01, 0xf5, 0x8f, 0x59, 0x42, 0x67, 0x00, 0x00, 0x00, 0x00,
	0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func (h *googleGenAIHost) Generate(ctx context.Context, request Request) (Response, error) {
	if request.Profile.Provider != h.provider {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Err: errors.New("profile provider mismatch")}
	}
	if err := validateTools(request.Tools); err != nil {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "invalid_tool_schema", ReasonCode: "invalid_tool_schema", Scope: "request", Err: err}
	}
	choice, err := normalizeToolChoice(request.ToolChoice, request.Tools)
	if err != nil {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "invalid_tool_choice", Scope: "request", Err: err}
	}
	if len(request.Tools) > 0 && !request.Profile.Capabilities.Tools {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "unsupported_tools", Scope: "capability", Err: errors.New("selected model profile does not support effective tool routing")}
	}
	if choice.Mode != "" && choice.Mode != ToolChoiceDisabled && !request.Profile.Capabilities.ToolChoice {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "unsupported_tool_choice", Scope: "capability", Err: errors.New("selected model profile does not support tool choice")}
	}
	contents, system, err := googleGenAIContents(request.Messages, request.System, request.Profile, h.continuationFormat)
	if err != nil {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: "request", Err: err}
	}
	maxOutputTokens := effectiveMaxOutputTokens(request.Profile, request.MaxOutputTokens)
	if maxOutputTokens <= 0 {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: "request", Err: errors.New("max output tokens must be positive")}
	}
	config := &googlegenai.GenerateContentConfig{
		MaxOutputTokens: int32(maxOutputTokens),
	}
	if request.Profile.Capabilities.ReasoningControls {
		config.ThinkingConfig = &googlegenai.ThinkingConfig{ThinkingLevel: googleGenAIThinkingLevel(request.ReasoningEffort)}
	}
	if strings.TrimSpace(system) != "" {
		config.SystemInstruction = googlegenai.NewContentFromText(system, googlegenai.RoleUser)
	}
	if len(request.Tools) > 0 {
		declarations := make([]*googlegenai.FunctionDeclaration, 0, len(request.Tools))
		for _, tool := range request.Tools {
			declarations = append(declarations, &googlegenai.FunctionDeclaration{
				Name: tool.Name, Description: tool.Description, ParametersJsonSchema: map[string]any(tool.InputSchema),
			})
		}
		config.Tools = []*googlegenai.Tool{{FunctionDeclarations: declarations}}
		functionConfig := &googlegenai.FunctionCallingConfig{Mode: googlegenai.FunctionCallingConfigModeAuto}
		switch choice.Mode {
		case ToolChoiceRequired:
			functionConfig.Mode = googlegenai.FunctionCallingConfigModeAny
		case ToolChoiceDisabled:
			functionConfig.Mode = googlegenai.FunctionCallingConfigModeNone
		case ToolChoiceFunction:
			functionConfig.Mode = googlegenai.FunctionCallingConfigModeAny
			functionConfig.AllowedFunctionNames = []string{choice.FunctionName}
		}
		config.ToolConfig = &googlegenai.ToolConfig{FunctionCallingConfig: functionConfig}
	}
	response, err := h.generate(ctx, request.Profile.ModelID, contents, config)
	if err != nil {
		return Response{}, classifyGoogleGenAIError(h.provider, err)
	}
	return googleGenAIResponse(response, request.Profile, h.provider, h.continuationFormat)
}

func googleGenAIContents(messages []Message, system string, profile Profile, continuationFormat string) ([]*googlegenai.Content, string, error) {
	contents := make([]*googlegenai.Content, 0, len(messages))
	for _, message := range messages {
		role, ok := NormalizeRole(string(message.Role))
		if !ok {
			return nil, "", errors.Errorf("unsupported message role %q", message.Role)
		}
		if role == RoleSystem {
			if text := strings.TrimSpace(message.Text()); text != "" {
				if strings.TrimSpace(system) != "" {
					system += "\n\n"
				}
				system += text
			}
			continue
		}
		if role == RoleTool {
			if message.ToolResult == nil {
				return nil, "", errors.New("tool message is missing its result")
			}
			result := message.ToolResult
			response := map[string]any{"output": result.Output}
			if result.Error != nil {
				response = map[string]any{"error": map[string]any{"code": result.Error.Code, "message": result.Error.Message}}
			}
			contents = append(contents, &googlegenai.Content{Role: "user", Parts: []*googlegenai.Part{{FunctionResponse: &googlegenai.FunctionResponse{
				ID: result.CallID, Name: result.Name, Response: response,
			}}}})
			continue
		}
		if role == RoleAssistant && message.Continuation != nil {
			var original googlegenai.Content
			if message.Continuation.decode(profile, continuationFormat, &original) {
				if original.Role != "model" || len(original.Parts) == 0 {
					return nil, "", errors.New("provider continuation is not an assistant content")
				}
				contents = append(contents, &original)
				continue
			}
		}
		vertexRole := "user"
		if role == RoleAssistant {
			vertexRole = "model"
		}
		parts := make([]*googlegenai.Part, 0, len(message.Parts)+len(message.ToolCalls))
		for _, part := range message.Parts {
			if part.Image != nil {
				if !profile.Capabilities.Images {
					return nil, "", errors.New("selected model profile does not support image input")
				}
				parts = append(parts, googlegenai.NewPartFromBytes(part.Image.Data, part.Image.MIMEType))
			}
			if part.Text != "" {
				parts = append(parts, googlegenai.NewPartFromText(part.Text))
			}
		}
		for _, call := range message.ToolCalls {
			parts = append(parts, &googlegenai.Part{FunctionCall: &googlegenai.FunctionCall{ID: call.ID, Name: call.Name, Args: call.Arguments}})
		}
		contents = append(contents, &googlegenai.Content{Role: vertexRole, Parts: parts})
	}
	if len(contents) == 0 {
		return nil, "", errors.New("request contains no messages")
	}
	return contents, system, nil
}

func googleGenAIResponse(response *googlegenai.GenerateContentResponse, profile Profile, provider Provider, continuationFormat string) (Response, error) {
	if response == nil || len(response.Candidates) == 0 || response.Candidates[0] == nil {
		if response != nil && response.PromptFeedback != nil && response.PromptFeedback.BlockReason != "" {
			return Response{}, &Error{Kind: ErrorSafety, Provider: provider, Err: errors.New("provider safety refusal")}
		}
		return Response{}, &Error{Kind: ErrorEmptyResponse, Provider: provider, Err: errors.New("provider returned no candidates")}
	}
	candidate := response.Candidates[0]
	message := Message{Role: RoleAssistant}
	preserveContinuation := false
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part == nil {
				continue
			}
			if part.Thought || len(part.ThoughtSignature) > 0 {
				preserveContinuation = true
			}
			if part.Text != "" && !part.Thought {
				message.Parts = append(message.Parts, Part{Text: part.Text})
			}
			if part.FunctionCall != nil {
				preserveContinuation = true
				message.ToolCalls = append(message.ToolCalls, ToolCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Arguments: part.FunctionCall.Args})
			}
		}
		if preserveContinuation {
			encoded, err := json.Marshal(candidate.Content)
			if err != nil {
				return Response{Message: message}, &Error{Kind: ErrorMalformed, Provider: provider, Scope: "choice", Err: errors.New("encode provider continuation")}
			}
			message.Continuation = newContinuationState(profile, continuationFormat, encoded)
		}
	}
	blocked := googleGenAISafetyFinish(candidate.FinishReason)
	if strings.TrimSpace(message.Text()) == "" && len(message.ToolCalls) == 0 {
		kind := ErrorEmptyResponse
		if blocked {
			kind = ErrorSafety
		}
		return Response{}, &Error{Kind: kind, Provider: provider, Err: errors.New("provider returned no usable output")}
	}
	actualModel := safeRoutingIdentity(response.ModelVersion)
	result := Response{
		Message: message, ModelID: actualModel, Finish: FinishMetadata{Reason: string(candidate.FinishReason), Blocked: blocked},
		Metadata: ResponseMetadata{
			ConfiguredProvider: profile.Provider, ConfiguredProfile: profile.Name, ActualModelID: actualModel,
			ProviderRequestID: safeDiagnosticCode(response.ResponseID), UpstreamProvider: string(provider),
		},
	}
	if response.UsageMetadata != nil {
		result.Usage = Usage{
			InputTokens:     int(response.UsageMetadata.PromptTokenCount),
			OutputTokens:    int(response.UsageMetadata.CandidatesTokenCount),
			ReasoningTokens: int(response.UsageMetadata.ThoughtsTokenCount),
			TotalTokens:     int(response.UsageMetadata.TotalTokenCount),
		}
	}
	return result, nil
}

func googleGenAIThinkingLevel(effort ReasoningEffort) googlegenai.ThinkingLevel {
	switch effort {
	case ReasoningLow:
		return googlegenai.ThinkingLevelLow
	case ReasoningHigh:
		return googlegenai.ThinkingLevelHigh
	default:
		return googlegenai.ThinkingLevelMedium
	}
}

func googleGenAISafetyFinish(reason googlegenai.FinishReason) bool {
	switch reason {
	case googlegenai.FinishReasonSafety, googlegenai.FinishReasonBlocklist, googlegenai.FinishReasonProhibitedContent,
		googlegenai.FinishReasonSPII, googlegenai.FinishReasonImageSafety, googlegenai.FinishReasonImageProhibitedContent:
		return true
	default:
		return false
	}
}

func classifyGoogleGenAIError(provider Provider, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return transportError(provider, err)
	}
	var apiError googlegenai.APIError
	if errors.As(err, &apiError) {
		message := strings.ToLower(apiError.Message)
		kind := errorKindForStatus(apiError.Code)
		reasonCode := "provider_error"
		switch {
		case provider == ProviderGoogleAI && apiError.Code == http.StatusForbidden:
			kind, reasonCode = ErrorAuthorization, "permission_denied"
		case provider == ProviderGoogleAI && apiError.Code == http.StatusPaymentRequired:
			kind, reasonCode = ErrorQuota, "quota_exhausted"
		case strings.Contains(message, "context length"), strings.Contains(message, "maximum context"), strings.Contains(message, "too many tokens"):
			kind, reasonCode = ErrorContextLimit, "context_limit"
		case strings.Contains(message, "safety"), strings.Contains(message, "content policy"):
			kind, reasonCode = ErrorSafety, "safety_refusal"
		case provider == ProviderGoogleAI && apiError.Code == http.StatusTooManyRequests && strings.Contains(message, "quota"):
			kind, reasonCode = ErrorQuota, "quota_exhausted"
		case apiError.Code == http.StatusTooManyRequests:
			kind, reasonCode = ErrorRateLimit, "rate_limited"
		}
		return &Error{
			Kind: kind, Provider: provider, StatusCode: apiError.Code, ProviderStatusCode: apiError.Code,
			ErrorType: safeDiagnosticCode(apiError.Status), ReasonCode: reasonCode, Scope: "provider",
			Err: errors.New("provider returned an error"),
		}
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unmarshal") || strings.Contains(message, "decode") || strings.Contains(message, "maptostruct") {
		return &Error{Kind: ErrorMalformed, Provider: provider, ErrorType: "malformed_response", Scope: "decode", Err: errors.New("provider returned a malformed response")}
	}
	return transportError(provider, err)
}

func (h *googleGenAIHost) Probe(ctx context.Context, profile Profile) (Capabilities, error) {
	if profile.Provider != h.provider {
		return Capabilities{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: "profile", Err: errors.New("profile provider mismatch")}
	}
	model, err := h.getModel(ctx, profile.ModelID, nil)
	if err != nil {
		return Capabilities{}, classifyGoogleGenAIError(h.provider, err)
	}
	if model == nil {
		return Capabilities{}, &Error{Kind: ErrorMalformed, Provider: h.provider, Err: errors.New("model lookup returned no model")}
	}
	if h.provider == ProviderGoogleAI {
		if err := validateGoogleAIModel(profile.ModelID, model); err != nil {
			return Capabilities{}, err
		}
	}
	capabilities := Capabilities{
		Reasoning: model.Thinking, ReasoningControls: h.provider == ProviderVertex && model.Thinking,
		MaxInputTokens: int(model.InputTokenLimit), MaxOutputTokens: int(model.OutputTokenLimit),
	}
	if h.countTokens == nil {
		// Injected test adapters predating the active probe retain metadata discovery.
		for _, action := range model.SupportedActions {
			if strings.EqualFold(action, "generateContent") {
				capabilities.Tools = true
				capabilities.ToolChoice = true
			}
		}
		return capabilities, nil
	}
	toolContents, toolConfig := googleGenAIToolProbe(profile.ModelID, h.provider)
	response, err := h.countTokens(ctx, profile.ModelID, toolContents, toolConfig)
	if err == nil && response == nil {
		return Capabilities{}, malformedProbeResponse(h.provider)
	}
	if err == nil {
		capabilities.Tools = true
		capabilities.ToolChoice = true
	} else if !googleGenAIFeatureUnsupported(err, "tool", "function") {
		return Capabilities{}, classifyGoogleGenAIError(h.provider, err)
	}

	if h.provider == ProviderGoogleAI && model.Thinking {
		reasoningControls := true
		for _, effort := range []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh} {
			response, err = h.countTokens(ctx, profile.ModelID, nil, googleAIReasoningProbe(profile.ModelID, effort))
			if err == nil && response == nil {
				return Capabilities{}, malformedProbeResponse(h.provider)
			}
			if err == nil {
				continue
			}
			if googleGenAIFeatureUnsupported(err, "thinking", "reasoning") {
				reasoningControls = false
				continue
			}
			return Capabilities{}, classifyGoogleGenAIError(h.provider, err)
		}
		capabilities.ReasoningControls = reasoningControls
	}

	response, err = h.countTokens(ctx, profile.ModelID, []*googlegenai.Content{{Role: "user", Parts: []*googlegenai.Part{{
		InlineData: &googlegenai.Blob{Data: vertexCapabilityProbePNG, MIMEType: "image/png"},
	}}}}, &googlegenai.CountTokensConfig{})
	if err == nil && response == nil {
		return Capabilities{}, malformedProbeResponse(h.provider)
	}
	if err == nil {
		capabilities.Images = true
		return capabilities, nil
	}
	if googleGenAIFeatureUnsupported(err, "image", "inline data", "multimodal") {
		return capabilities, nil
	}
	return Capabilities{}, classifyGoogleGenAIError(h.provider, err)
}

func validateGoogleAIModel(requested string, model *googlegenai.Model) error {
	requested = strings.TrimPrefix(strings.TrimSpace(requested), "models/")
	canonical := strings.TrimPrefix(strings.TrimSpace(model.Name), "models/")
	if canonical == "" {
		return &Error{Kind: ErrorMalformed, Provider: ProviderGoogleAI, Scope: "model-metadata", Err: errors.New("model metadata did not include a canonical name")}
	}
	if canonical != requested {
		return &Error{Kind: ErrorInvalidRequest, Provider: ProviderGoogleAI, Scope: "model-metadata", Err: errors.New("model metadata name does not match the configured model ID")}
	}
	match := explicitGeminiModelPattern.FindStringSubmatch(canonical)
	major := 0
	if len(match) > 1 {
		major, _ = strconv.Atoi(match[1])
	}
	if major < 3 || containsModelSegment(canonical, "latest") {
		return &Error{Kind: ErrorInvalidRequest, Provider: ProviderGoogleAI, Scope: "model-metadata", Err: errors.New("Google AI profiles require an explicit Gemini 3 or newer model ID")}
	}
	for _, action := range model.SupportedActions {
		if strings.EqualFold(strings.TrimSpace(action), "generateContent") {
			return nil
		}
	}
	return &Error{Kind: ErrorInvalidRequest, Provider: ProviderGoogleAI, Scope: "model-metadata", Err: errors.New("Google AI model does not support generateContent")}
}

func containsModelSegment(modelID, segment string) bool {
	for _, part := range strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool { return r == '-' || r == '.' }) {
		if part == segment {
			return true
		}
	}
	return false
}

func googleGenAIToolProbe(modelID string, provider Provider) ([]*googlegenai.Content, *googlegenai.CountTokensConfig) {
	declaration := map[string]any{
		"name": "jarvis_capability_probe", "description": "Harmless startup capability declaration.",
		"parametersJsonSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
	if provider == ProviderGoogleAI {
		return nil, &googlegenai.CountTokensConfig{HTTPOptions: &googlegenai.HTTPOptions{ExtraBody: map[string]any{
			"generateContentRequest": map[string]any{
				"model":    googleAIModelResource(modelID),
				"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "capability probe"}}}},
				"tools":    []any{map[string]any{"functionDeclarations": []any{declaration}}},
				"toolConfig": map[string]any{"functionCallingConfig": map[string]any{
					"mode": "ANY", "allowedFunctionNames": []any{"jarvis_capability_probe"},
				}},
			},
		}}}
	}
	return googlegenai.Text("capability probe"), &googlegenai.CountTokensConfig{
		Tools: []*googlegenai.Tool{{FunctionDeclarations: []*googlegenai.FunctionDeclaration{{
			Name: "jarvis_capability_probe", Description: "Harmless startup capability declaration.",
			ParametersJsonSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}}}},
	}
}

func googleAIReasoningProbe(modelID string, effort ReasoningEffort) *googlegenai.CountTokensConfig {
	return &googlegenai.CountTokensConfig{HTTPOptions: &googlegenai.HTTPOptions{ExtraBody: map[string]any{
		"generateContentRequest": map[string]any{
			"model":    googleAIModelResource(modelID),
			"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "capability probe"}}}},
			"generationConfig": map[string]any{"thinkingConfig": map[string]any{
				"thinkingLevel": string(googleGenAIThinkingLevel(effort)),
			}},
		},
	}}}
}

func googleAIModelResource(modelID string) string {
	return "models/" + strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
}

func malformedProbeResponse(provider Provider) error {
	return &Error{Kind: ErrorMalformed, Provider: provider, Scope: "capability-probe", Err: errors.New("capability probe returned no response")}
}

func googleGenAIFeatureUnsupported(err error, featureTerms ...string) bool {
	var apiError googlegenai.APIError
	if !errors.As(err, &apiError) || apiError.Code != 400 {
		return false
	}
	message := strings.ToLower(apiError.Message)
	mentionsFeature := false
	for _, term := range featureTerms {
		mentionsFeature = mentionsFeature || strings.Contains(message, term)
	}
	unsupported := strings.Contains(message, "unsupported") || strings.Contains(message, "not support") || strings.Contains(message, "not available")
	return mentionsFeature && unsupported
}
