package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justinswe/std/errors"
)

const (
	defaultNVIDIABaseURL     = "https://integrate.api.nvidia.com/v1"
	defaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
	maxProviderBodyBytes     = 4 << 20
	compatibleRequestShape   = "openai-compatible-chat-v2"
)

// OpenAICompatibleConfig configures an OpenRouter or hosted NVIDIA NIM host.
type OpenAICompatibleConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

// NewOpenRouterHost creates an OpenRouter host and model prober.
func NewOpenRouterHost(config OpenAICompatibleConfig) (Host, Prober, error) {
	return newOpenAICompatibleHost(ProviderOpenRouter, config)
}

// NewNVIDIAHost creates a hosted NVIDIA NIM host and model prober.
func NewNVIDIAHost(config OpenAICompatibleConfig) (Host, Prober, error) {
	return newOpenAICompatibleHost(ProviderNVIDIANIM, config)
}

type openAICompatibleHost struct {
	provider   Provider
	baseURL    string
	apiKey     string
	httpClient *http.Client
	wireMu     sync.RWMutex
	wire       map[string]compatibleWireCapabilities
}

type compatibleWireCapabilities struct {
	reasoningEfforts map[ReasoningEffort]struct{}
}

func newOpenAICompatibleHost(provider Provider, config OpenAICompatibleConfig) (*openAICompatibleHost, *openAICompatibleHost, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, nil, errors.Errorf("%s API key is required", provider)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		if provider == ProviderOpenRouter {
			baseURL = defaultOpenRouterBaseURL
		} else {
			baseURL = defaultNVIDIABaseURL
		}
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, nil, errors.Errorf("%s base URL must be absolute", provider)
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 55 * time.Second}
	}
	host := &openAICompatibleHost{
		provider: provider, baseURL: baseURL, apiKey: strings.TrimSpace(config.APIKey), httpClient: client,
		wire: make(map[string]compatibleWireCapabilities),
	}
	return host, host, nil
}

type compatibleRequest struct {
	Model           string              `json:"model"`
	Messages        []compatibleMessage `json:"messages"`
	MaxTokens       int                 `json:"max_tokens,omitempty"`
	Tools           []compatibleTool    `json:"tools,omitempty"`
	ToolChoice      any                 `json:"tool_choice,omitempty"`
	Reasoning       *reasoningConfig    `json:"reasoning,omitempty"`
	ReasoningEffort ReasoningEffort     `json:"reasoning_effort,omitempty"`
	Provider        *compatibleProvider `json:"provider,omitempty"`
}

type compatibleProvider struct {
	RequireParameters bool `json:"require_parameters"`
}

type reasoningConfig struct {
	Effort ReasoningEffort `json:"effort"`
}

type compatibleMessage struct {
	Role             Role                 `json:"role"`
	Content          any                  `json:"content"`
	ToolCalls        []compatibleToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
	Name             string               `json:"name,omitempty"`
	Reasoning        json.RawMessage      `json:"reasoning,omitempty"`
	ReasoningContent json.RawMessage      `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage      `json:"reasoning_details,omitempty"`
}

type compatibleContentPart struct {
	Type     string                  `json:"type"`
	Text     string                  `json:"text,omitempty"`
	ImageURL *compatibleImageContent `json:"image_url,omitempty"`
}

type compatibleImageContent struct {
	URL string `json:"url"`
}

type compatibleTool struct {
	Type     string                 `json:"type"`
	Function compatibleToolFunction `json:"function"`
}

type compatibleToolFunction struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Parameters  JSONSchema `json:"parameters"`
}

type compatibleSpecificToolChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type compatibleToolCall struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
	Function     struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type compatibleResponse struct {
	ID                 string                       `json:"id"`
	Model              string                       `json:"model"`
	Provider           string                       `json:"provider"`
	Choices            []compatibleChoice           `json:"choices"`
	OpenRouterMetadata compatibleOpenRouterMetadata `json:"openrouter_metadata"`
	Usage              struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		CompletionDetail struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *compatibleError `json:"error,omitempty"`
}

type compatibleOpenRouterMetadata struct {
	Attempt   int `json:"attempt"`
	Endpoints struct {
		Available []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Selected bool   `json:"selected"`
		} `json:"available"`
	} `json:"endpoints"`
	Attempts []struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Status   int    `json:"status"`
	} `json:"attempts"`
	Pipeline []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"pipeline"`
}

type compatibleChoice struct {
	FinishReason       string                    `json:"finish_reason"`
	NativeFinishReason string                    `json:"native_finish_reason"`
	Message            compatibleResponseMessage `json:"message"`
	Error              *compatibleError          `json:"error,omitempty"`
}

type compatibleError struct {
	Code     json.RawMessage         `json:"code"`
	Message  string                  `json:"message"`
	Type     string                  `json:"type"`
	Metadata compatibleErrorMetadata `json:"metadata,omitempty"`
}

type compatibleErrorMetadata struct {
	ErrorType    string          `json:"error_type"`
	ProviderCode json.RawMessage `json:"provider_code"`
}

type compatibleResponseMessage struct {
	Content          json.RawMessage      `json:"content"`
	Refusal          json.RawMessage      `json:"refusal,omitempty"`
	ToolCalls        []compatibleToolCall `json:"tool_calls"`
	Reasoning        json.RawMessage      `json:"reasoning,omitempty"`
	ReasoningContent json.RawMessage      `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage      `json:"reasoning_details,omitempty"`
}

const compatibleContinuationFormat = "openai-compatible-chat-v1"

func (h *openAICompatibleHost) Generate(ctx context.Context, request Request) (Response, error) {
	if request.Profile.Provider != h.provider {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Err: errors.New("profile provider mismatch")}
	}
	payloadRequest, err := h.convertRequest(request)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(payloadRequest)
	if err != nil {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Err: err}
	}
	diagnostics := compatibleDiagnostics(payloadRequest, payload, request.ReasoningEffort)
	retryAfterSeconds := 0
	annotate := func(err error, metadata ResponseMetadata) error {
		return annotateCompatibleError(err, diagnostics, metadata, retryAfterSeconds)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Err: err}
	}
	httpRequest.Header.Set("Authorization", "Bearer "+h.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	if h.provider == ProviderOpenRouter {
		httpRequest.Header.Set("X-OpenRouter-Metadata", "enabled")
	}
	httpResponse, err := h.httpClient.Do(httpRequest)
	if err != nil {
		return Response{}, annotate(transportError(h.provider, err), configuredResponseMetadata(request.Profile))
	}
	defer httpResponse.Body.Close()
	retryAfterSeconds = compatibleRetryAfterSeconds(httpResponse.Header, time.Now())
	providerRequestID := compatibleRequestID(httpResponse.Header)
	metadata := configuredResponseMetadata(request.Profile)
	metadata.ProviderRequestID = providerRequestID
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxProviderBodyBytes))
	if err != nil {
		return Response{}, annotate(transportError(h.provider, err), metadata)
	}
	var decoded compatibleResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
			return Response{}, annotate(statusError(h.provider, httpResponse.StatusCode, "undecodable_error_response", "top-level-decode"), metadata)
		}
		return Response{}, annotate(&Error{Kind: ErrorMalformed, Provider: h.provider, StatusCode: httpResponse.StatusCode, Scope: "decode", Err: err}, metadata)
	}
	metadata = compatibleResponseMetadata(request.Profile, decoded, providerRequestID)
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 || decoded.Error != nil {
		providerErr := decoded.Error
		if providerErr == nil {
			providerErr = &compatibleError{Code: json.RawMessage(fmt.Sprintf("%d", httpResponse.StatusCode))}
		}
		return Response{}, annotate(classifyProviderError(h.provider, httpResponse.StatusCode, providerErr, "top-level"), metadata)
	}
	if len(decoded.Choices) == 0 {
		return Response{}, annotate(&Error{Kind: ErrorEmptyResponse, Provider: h.provider, StatusCode: httpResponse.StatusCode, Scope: "empty", Err: errors.New("provider returned no choices")}, metadata)
	}
	choice := decoded.Choices[0]
	response, err := h.decodeResponseChoice(request.Profile, decoded.Model, decoded.Usage, choice)
	response.Metadata = metadata
	if err != nil {
		return response, annotate(err, metadata)
	}
	if choice.Error != nil {
		return response, annotate(classifyProviderError(h.provider, httpResponse.StatusCode, choice.Error, "choice"), metadata)
	}
	if strings.EqualFold(choice.FinishReason, "error") {
		return response, annotate(&Error{Kind: ErrorService, Provider: h.provider, StatusCode: httpResponse.StatusCode, ErrorType: "generation_error", Scope: "choice", Err: errors.New("provider generation ended with an error")}, metadata)
	}
	if response.Finish.Blocked {
		return response, annotate(&Error{Kind: ErrorSafety, Provider: h.provider, StatusCode: httpResponse.StatusCode, ErrorType: "refusal", Scope: "choice", Err: errors.New("provider returned a refusal")}, metadata)
	}
	if strings.TrimSpace(response.Text()) == "" && len(response.Message.ToolCalls) == 0 {
		kind := ErrorEmptyResponse
		if response.Finish.Blocked {
			kind = ErrorSafety
		}
		return Response{}, annotate(&Error{Kind: kind, Provider: h.provider, StatusCode: httpResponse.StatusCode, Scope: "empty", Err: errors.New("provider returned no usable output")}, metadata)
	}
	return response, nil
}

func (h *openAICompatibleHost) decodeResponseChoice(profile Profile, modelID string, usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CompletionDetail struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}, choice compatibleChoice) (Response, error) {
	text, err := compatibleText(choice.Message.Content)
	if err != nil {
		return Response{}, &Error{Kind: ErrorMalformed, Provider: h.provider, Scope: "choice", Err: err}
	}
	refusal, err := compatibleText(choice.Message.Refusal)
	if err != nil {
		return Response{}, &Error{Kind: ErrorMalformed, Provider: h.provider, Scope: "choice", Err: errors.Wrap(err, "decode refusal")}
	}
	if text == "" {
		text = refusal
	}
	message := Message{Role: RoleAssistant}
	if text != "" {
		message.Parts = []Part{{Text: text}}
	}
	seenCallIDs := make(map[string]struct{}, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" {
			return Response{Message: message}, &Error{Kind: ErrorMalformed, Provider: h.provider, Scope: "choice", Err: errors.New("provider returned a tool call without an ID or name")}
		}
		if _, exists := seenCallIDs[call.ID]; exists {
			return Response{Message: message}, &Error{Kind: ErrorMalformed, Provider: h.provider, Scope: "choice", Err: errors.New("provider returned duplicate tool call ID")}
		}
		seenCallIDs[call.ID] = struct{}{}
		var args map[string]any
		if strings.TrimSpace(call.Function.Arguments) == "" {
			args = map[string]any{}
		} else if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return Response{Message: message}, &Error{Kind: ErrorMalformed, Provider: h.provider, Scope: "choice", Err: errors.Wrap(err, "decode tool arguments")}
		}
		message.ToolCalls = append(message.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: args})
	}
	continuation, marshalErr := json.Marshal(choice.Message)
	if marshalErr != nil {
		return Response{Message: message}, &Error{Kind: ErrorMalformed, Provider: h.provider, Scope: "choice", Err: errors.Wrap(marshalErr, "encode provider continuation")}
	}
	if compatibleMessageHasContinuation(choice.Message) {
		message.Continuation = newContinuationState(profile, compatibleContinuationFormat, continuation)
	}
	return Response{
		Message: message,
		Finish: FinishMetadata{
			Reason: choice.FinishReason, NativeReason: choice.NativeFinishReason,
			Blocked: refusal != "" || safetyFinish(choice.FinishReason),
		},
		Usage: Usage{
			InputTokens:     usage.PromptTokens,
			OutputTokens:    usage.CompletionTokens,
			ReasoningTokens: usage.CompletionDetail.ReasoningTokens,
			TotalTokens:     usage.TotalTokens,
		},
		ModelID: modelID,
	}, nil
}

func compatibleMessageHasContinuation(message compatibleResponseMessage) bool {
	if len(message.Reasoning) > 0 || len(message.ReasoningContent) > 0 || len(message.ReasoningDetails) > 0 {
		return true
	}
	for _, call := range message.ToolCalls {
		if len(call.ExtraContent) > 0 {
			return true
		}
	}
	return false
}

func (h *openAICompatibleHost) convertRequest(request Request) (compatibleRequest, error) {
	converted := compatibleRequest{Model: request.Profile.ModelID}
	if err := validateTools(request.Tools); err != nil {
		return compatibleRequest{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "invalid_tool_schema", ReasonCode: "invalid_tool_schema", Scope: "request", Err: err}
	}
	choice, err := normalizeToolChoice(request.ToolChoice, request.Tools)
	if err != nil {
		return compatibleRequest{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "invalid_tool_choice", Scope: "request", Err: err}
	}
	if len(request.Tools) > 0 && !request.Profile.Capabilities.Tools {
		return compatibleRequest{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "unsupported_tools", Scope: "capability", Err: errors.New("selected model profile does not support effective tool routing")}
	}
	if choice.Mode != "" && choice.Mode != ToolChoiceDisabled && !request.Profile.Capabilities.ToolChoice {
		return compatibleRequest{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "unsupported_tool_choice", Scope: "capability", Err: errors.New("selected model profile does not support tool choice")}
	}
	maxOutputTokens := effectiveMaxOutputTokens(request.Profile, request.MaxOutputTokens)
	if maxOutputTokens <= 0 {
		return compatibleRequest{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: "request", Err: errors.New("max output tokens must be positive")}
	}
	if strings.TrimSpace(request.System) != "" {
		converted.Messages = append(converted.Messages, compatibleMessage{Role: RoleSystem, Content: request.System})
	}
	for _, message := range request.Messages {
		item, err := compatibleRequestMessage(message, request.Profile)
		if err != nil {
			return compatibleRequest{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: "request", Err: err}
		}
		converted.Messages = append(converted.Messages, item)
	}
	if len(converted.Messages) == 0 {
		return compatibleRequest{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: "request", Err: errors.New("request contains no messages")}
	}
	wire, ok := h.wireCapabilities(request.Profile.ModelID)
	if !ok {
		return compatibleRequest{}, &Error{
			Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "unvalidated_model_capabilities",
			ReasonCode: "unvalidated_model_capabilities", Scope: "capability",
			Err: errors.New("selected model profile has not completed capability validation"),
		}
	}
	converted.MaxTokens = maxOutputTokens
	_, reasoningEffortConfirmed := wire.reasoningEfforts[request.ReasoningEffort]
	if request.Profile.Capabilities.ReasoningControls && request.ReasoningEffort != "" && reasoningEffortConfirmed {
		if h.provider == ProviderOpenRouter {
			converted.Reasoning = &reasoningConfig{Effort: request.ReasoningEffort}
		} else {
			converted.ReasoningEffort = request.ReasoningEffort
		}
	}
	for _, tool := range request.Tools {
		converted.Tools = append(converted.Tools, compatibleTool{Type: "function", Function: compatibleToolFunction{
			Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema,
		}})
	}
	switch choice.Mode {
	case ToolChoiceAutomatic:
		converted.ToolChoice = "auto"
	case ToolChoiceRequired:
		converted.ToolChoice = "required"
	case ToolChoiceDisabled:
		converted.ToolChoice = "none"
	case ToolChoiceFunction:
		specific := compatibleSpecificToolChoice{Type: "function"}
		specific.Function.Name = choice.FunctionName
		converted.ToolChoice = specific
	default:
		if len(converted.Tools) > 0 && request.Profile.Capabilities.ToolChoice {
			converted.ToolChoice = "auto"
		}
	}
	if h.provider == ProviderOpenRouter && (len(converted.Tools) > 0 || converted.Reasoning != nil) {
		converted.Provider = &compatibleProvider{RequireParameters: true}
	}
	return converted, nil
}

func (h *openAICompatibleHost) wireCapabilities(modelID string) (compatibleWireCapabilities, bool) {
	h.wireMu.RLock()
	wire, ok := h.wire[modelID]
	h.wireMu.RUnlock()
	if ok {
		return wire, true
	}
	if h.provider == ProviderNVIDIANIM {
		return compatibleWireCapabilities{}, true
	}
	return compatibleWireCapabilities{}, false
}

func (h *openAICompatibleHost) setWireCapabilities(modelID string, wire compatibleWireCapabilities) {
	h.wireMu.Lock()
	h.wire[modelID] = wire
	h.wireMu.Unlock()
}

func compatibleRequestMessage(message Message, profile Profile) (compatibleMessage, error) {
	role, ok := NormalizeRole(string(message.Role))
	if !ok {
		return compatibleMessage{}, errors.Errorf("unsupported message role %q", message.Role)
	}
	if role == RoleTool {
		if message.ToolResult == nil {
			return compatibleMessage{}, errors.New("tool message is missing its result")
		}
		if strings.TrimSpace(message.ToolResult.CallID) == "" || strings.TrimSpace(message.ToolResult.Name) == "" {
			return compatibleMessage{}, errors.New("tool result is missing its call ID or name")
		}
		output, err := message.ToolResult.MarshalOutput()
		if err != nil {
			return compatibleMessage{}, errors.Wrap(err, "encode tool result")
		}
		return compatibleMessage{Role: role, Content: string(output), ToolCallID: message.ToolResult.CallID, Name: message.ToolResult.Name}, nil
	}
	converted := compatibleMessage{Role: role}
	var continuation compatibleResponseMessage
	if role == RoleAssistant && message.Continuation != nil {
		_ = message.Continuation.decode(profile, compatibleContinuationFormat, &continuation)
		converted.Reasoning = continuation.Reasoning
		converted.ReasoningContent = continuation.ReasoningContent
		converted.ReasoningDetails = continuation.ReasoningDetails
	}
	extraContent := make(map[string]json.RawMessage, len(continuation.ToolCalls))
	for _, call := range continuation.ToolCalls {
		if call.ID != "" && len(call.ExtraContent) > 0 {
			extraContent[call.ID] = call.ExtraContent
		}
	}
	if len(message.ToolCalls) > 0 {
		converted.ToolCalls = make([]compatibleToolCall, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
				return compatibleMessage{}, errors.New("tool call is missing its ID or name")
			}
			arguments, err := json.Marshal(call.Arguments)
			if err != nil {
				return compatibleMessage{}, errors.Wrap(err, "encode tool arguments")
			}
			item := compatibleToolCall{ID: call.ID, Type: "function", ExtraContent: extraContent[call.ID]}
			item.Function.Name = call.Name
			item.Function.Arguments = string(arguments)
			converted.ToolCalls = append(converted.ToolCalls, item)
		}
	}
	parts := make([]compatibleContentPart, 0, len(message.Parts))
	hasImage := false
	for _, part := range message.Parts {
		if part.Image != nil {
			if !profile.Capabilities.Images {
				return compatibleMessage{}, errors.New("selected model profile does not support image input")
			}
			hasImage = true
			parts = append(parts, compatibleContentPart{Type: "image_url", ImageURL: &compatibleImageContent{URL: "data:" + part.Image.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(part.Image.Data)}})
		}
		if part.Text != "" {
			parts = append(parts, compatibleContentPart{Type: "text", Text: part.Text})
		}
	}
	if hasImage {
		converted.Content = parts
	} else if len(continuation.Content) > 0 {
		converted.Content = continuation.Content
	} else if message.Text() == "" && len(message.ToolCalls) > 0 {
		converted.Content = nil
	} else {
		converted.Content = message.Text()
	}
	return converted, nil
}

func compatibleText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.Wrap(err, "decode response content")
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" || part.Type == "output_text" || part.Type == "" {
			builder.WriteString(part.Text)
		}
	}
	return builder.String(), nil
}

func compatibleDiagnostics(request compatibleRequest, payload []byte, effort ReasoningEffort) RequestDiagnostics {
	diagnostics := RequestDiagnostics{
		RequestShapeVersion:      compatibleRequestShape,
		EffectiveMaxOutputTokens: request.MaxTokens,
		ReasoningRequested:       effort != "",
		ReasoningSent:            request.Reasoning != nil || request.ReasoningEffort != "",
		PayloadBytes:             len(payload),
		MessageCount:             len(request.Messages),
		ToolSchemaCount:          len(request.Tools),
		ToolSchemaFingerprint:    compatibleToolFingerprint(request.Tools),
		ToolSchemas:              compatibleToolDiagnostics(request.Tools),
		RequireParameters:        request.Provider != nil && request.Provider.RequireParameters,
	}
	diagnostics.TokenLimitField = "max_tokens"
	for _, message := range request.Messages {
		switch message.Role {
		case RoleSystem:
			diagnostics.SystemMessageCount++
		case RoleUser:
			diagnostics.UserMessageCount++
		case RoleAssistant:
			diagnostics.AssistantMessageCount++
		case RoleTool:
			diagnostics.ToolMessageCount++
		}
		if compatibleMessageHasImage(message) {
			diagnostics.InputHasImage = true
		}
	}
	return diagnostics
}

func compatibleToolFingerprint(tools []compatibleTool) string {
	if len(tools) == 0 {
		return ""
	}
	values := make([]struct {
		Name       string     `json:"name"`
		Parameters JSONSchema `json:"parameters"`
	}, 0, len(tools))
	for _, tool := range tools {
		values = append(values, struct {
			Name       string     `json:"name"`
			Parameters JSONSchema `json:"parameters"`
		}{Name: tool.Function.Name, Parameters: tool.Function.Parameters})
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:8])
}

func compatibleToolDiagnostics(tools []compatibleTool) []ToolSchemaDiagnostics {
	result := make([]ToolSchemaDiagnostics, 0, len(tools))
	for _, tool := range tools {
		encoded, err := json.Marshal(tool.Function.Parameters)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(encoded)
		result = append(result, ToolSchemaDiagnostics{Name: tool.Function.Name, Fingerprint: fmt.Sprintf("%x", sum[:8])})
	}
	return result
}

func compatibleMessageHasImage(message compatibleMessage) bool {
	parts, ok := message.Content.([]compatibleContentPart)
	if !ok {
		return false
	}
	for _, part := range parts {
		if part.ImageURL != nil {
			return true
		}
	}
	return false
}

func annotateCompatibleError(err error, diagnostics RequestDiagnostics, metadata ResponseMetadata, retryAfterSeconds int) error {
	var modelErr *Error
	if !errors.As(err, &modelErr) {
		return err
	}
	diagnostics.ProviderMessageBytes = modelErr.Request.ProviderMessageBytes
	diagnostics.ProviderMessageHash = modelErr.Request.ProviderMessageHash
	modelErr.Request = diagnostics
	modelErr.ProviderRequestID = metadata.ProviderRequestID
	modelErr.UpstreamProvider = metadata.UpstreamProvider
	modelErr.RoutingAttempt = metadata.RoutingAttempt
	modelErr.RetryAfterSeconds = retryAfterSeconds
	modelErr.PipelineStages = append([]string(nil), metadata.PipelineStages...)
	return err
}

func compatibleRetryAfterSeconds(header http.Header, now time.Time) int {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return seconds
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	seconds := int(when.Sub(now).Round(time.Second) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func configuredResponseMetadata(profile Profile) ResponseMetadata {
	return ResponseMetadata{ConfiguredProvider: profile.Provider, ConfiguredProfile: profile.Name}
}

func compatibleResponseMetadata(profile Profile, response compatibleResponse, headerRequestID string) ResponseMetadata {
	metadata := configuredResponseMetadata(profile)
	metadata.ActualModelID = safeRoutingIdentity(response.Model)
	metadata.ProviderRequestID = safeDiagnosticCode(headerRequestID)
	if metadata.ProviderRequestID == "" {
		metadata.ProviderRequestID = safeDiagnosticCode(response.ID)
	}
	metadata.UpstreamProvider = safeRoutingIdentity(response.Provider)
	metadata.RoutingAttempt = response.OpenRouterMetadata.Attempt
	for _, endpoint := range response.OpenRouterMetadata.Endpoints.Available {
		if !endpoint.Selected {
			continue
		}
		if provider := safeRoutingIdentity(endpoint.Provider); provider != "" {
			metadata.UpstreamProvider = provider
		}
		if model := safeRoutingIdentity(endpoint.Model); model != "" {
			metadata.ActualModelID = model
		}
		break
	}
	for _, attempt := range response.OpenRouterMetadata.Attempts {
		provider := safeRoutingIdentity(attempt.Provider)
		model := safeRoutingIdentity(attempt.Model)
		if provider == "" && model == "" {
			continue
		}
		status := attempt.Status
		if status < 100 || status > 599 {
			status = 0
		}
		metadata.RoutingAttempts = append(metadata.RoutingAttempts, RoutingAttemptMetadata{Provider: provider, Model: model, Status: status})
	}
	for _, stage := range response.OpenRouterMetadata.Pipeline {
		stageType := safeDiagnosticCode(stage.Type)
		stageName := safeDiagnosticCode(stage.Name)
		if stageType == "" || stageName == "" {
			continue
		}
		metadata.PipelineStages = append(metadata.PipelineStages, stageType+":"+stageName)
	}
	return metadata
}

func compatibleRequestID(header http.Header) string {
	for _, name := range []string{"X-Generation-Id", "X-Request-Id", "Request-Id"} {
		if value := safeDiagnosticCode(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func classifyCompatibleError(provider Provider, status int, message string) error {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "context length"), strings.Contains(lower, "maximum context"), strings.Contains(lower, "too many tokens"):
		return &Error{Kind: ErrorContextLimit, Provider: provider, StatusCode: status, ProviderStatusCode: status, Scope: "provider", Err: errors.New("context limit exceeded")}
	case strings.Contains(lower, "safety"), strings.Contains(lower, "content policy"):
		return &Error{Kind: ErrorSafety, Provider: provider, StatusCode: status, ProviderStatusCode: status, Scope: "provider", Err: errors.New("provider safety refusal")}
	default:
		return statusError(provider, status, "provider_error", "provider")
	}
}

func classifyProviderError(provider Provider, outerStatus int, providerErr *compatibleError, scope string) error {
	if providerErr == nil {
		providerErr = &compatibleError{}
	}
	providerStatus := compatibleStatusCode(providerErr.Code)
	effectiveStatus := providerStatus
	if effectiveStatus == 0 {
		effectiveStatus = outerStatus
	}
	errorType := safeDiagnosticCode(providerErr.Metadata.ErrorType)
	if errorType == "" {
		errorType = safeDiagnosticCode(providerErr.Type)
	}
	reasonCode := compatibleReasonCode(errorType, providerErr.Message)
	kind, known := compatibleErrorKind(errorType)
	if !known {
		kind = errorKindForStatus(effectiveStatus)
		lower := strings.ToLower(providerErr.Message)
		if strings.Contains(lower, "context length") || strings.Contains(lower, "maximum context") || strings.Contains(lower, "too many tokens") {
			kind = ErrorContextLimit
		} else if strings.Contains(lower, "safety") || strings.Contains(lower, "content policy") {
			kind = ErrorSafety
		}
	}
	modelErr := &Error{
		Kind: kind, Provider: provider, StatusCode: outerStatus, ProviderStatusCode: providerStatus,
		ErrorType: errorType, ProviderCode: compatibleProviderCode(providerErr.Metadata.ProviderCode), ReasonCode: reasonCode, Scope: scope,
		Err: errors.New("provider returned an error"),
	}
	modelErr.Request.ProviderMessageBytes, modelErr.Request.ProviderMessageHash = providerMessageDiagnostics(providerErr.Message)
	if provider == ProviderOpenRouter && scope == "top-level" && outerStatus == http.StatusBadRequest &&
		errorType == "" && kind == ErrorInvalidRequest && compatibleFallbackReason(reasonCode) {
		modelErr.FallbackHint = FallbackDifferentProvider
	}
	return modelErr
}

func compatibleReasonCode(errorType, message string) string {
	if errorType != "" {
		return errorType
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "context length"), strings.Contains(lower, "maximum context"), strings.Contains(lower, "too many tokens"):
		return "context_limit"
	case strings.Contains(lower, "safety"), strings.Contains(lower, "content policy"), strings.Contains(lower, "moderation"):
		return "safety_refusal"
	case strings.Contains(lower, "reasoning") && (strings.Contains(lower, "unsupported") || strings.Contains(lower, "invalid")):
		return "invalid_reasoning_config"
	case (strings.Contains(lower, "max_tokens") || strings.Contains(lower, "max_completion_tokens")) &&
		(strings.Contains(lower, "unsupported") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "invalid")):
		return "unsupported_token_parameter"
	case strings.Contains(lower, "tool") && (strings.Contains(lower, "schema") || strings.Contains(lower, "function")):
		return "invalid_tool_schema"
	case strings.Contains(lower, "invalid prompt"), strings.Contains(lower, "invalid message"), strings.Contains(lower, "invalid role"):
		return "invalid_prompt"
	default:
		return "request_rejected"
	}
}

func compatibleFallbackReason(reason string) bool {
	switch reason {
	case "request_rejected", "invalid_reasoning_config", "unsupported_token_parameter", "invalid_tool_schema":
		return true
	default:
		return false
	}
}

func compatibleErrorKind(value string) (ErrorKind, bool) {
	switch strings.ToLower(value) {
	case "context_length_exceeded", "max_tokens_exceeded", "token_limit_exceeded", "string_too_long", "payload_too_large":
		return ErrorContextLimit, true
	case "authentication", "authentication_error", "invalid_api_key", "permission_denied", "payment_required":
		return ErrorAuthentication, true
	case "rate_limit_exceeded", "rate_limit_error":
		return ErrorRateLimit, true
	case "provider_overloaded", "provider_unavailable", "server", "server_error", "api_error", "overloaded_error", "unmapped":
		return ErrorService, true
	case "timeout":
		return ErrorTimeout, true
	case "content_policy_violation", "refusal", "content_filter":
		return ErrorSafety, true
	case "invalid_request", "invalid_request_error", "invalid_prompt", "not_found", "precondition_failed", "unprocessable",
		"invalid_image", "image_too_large", "image_too_small", "unsupported_image_format", "image_not_found", "image_download_failed":
		return ErrorInvalidRequest, true
	default:
		return "", false
	}
}

func compatibleStatusCode(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var number int
	if json.Unmarshal(raw, &number) == nil && number >= 100 && number <= 599 {
		return number
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		number, _ = strconv.Atoi(strings.TrimSpace(value))
		if number >= 100 && number <= 599 {
			return number
		}
	}
	return 0
}

func compatibleProviderCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return safeDiagnosticCode(value)
	}
	var scalar any
	if json.Unmarshal(raw, &scalar) != nil {
		return ""
	}
	switch scalar.(type) {
	case float64, bool:
		return safeDiagnosticCode(string(raw))
	default:
		return ""
	}
}

func safeDiagnosticCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.", r) {
			continue
		}
		return ""
	}
	return value
}

func safeRoutingIdentity(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || strings.Contains(value, "://") {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./: ", r) {
			continue
		}
		return ""
	}
	return value
}

func providerMessageDiagnostics(message string) (int, string) {
	if message == "" {
		return 0, ""
	}
	sum := sha256.Sum256([]byte(message))
	return len(message), fmt.Sprintf("%x", sum[:8])
}

func safetyFinish(reason string) bool {
	switch strings.ToLower(reason) {
	case "content_filter", "safety", "blocked", "prohibited_content":
		return true
	default:
		return false
	}
}

type modelCatalog struct {
	Data  []modelCatalogEntry `json:"data"`
	Error *compatibleError    `json:"error,omitempty"`
}

type modelCatalogEntry struct {
	ID                  string   `json:"id"`
	ContextLength       int      `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	Architecture        struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Reasoning struct {
		SupportedEfforts []ReasoningEffort `json:"supported_efforts"`
	} `json:"reasoning"`
}

type modelEndpointCatalog struct {
	Data struct {
		Endpoints []struct {
			ContextLength       int      `json:"context_length"`
			MaxCompletionTokens int      `json:"max_completion_tokens"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"endpoints"`
	} `json:"data"`
}

func (h *openAICompatibleHost) Probe(ctx context.Context, profile Profile) (Capabilities, error) {
	var catalog modelCatalog
	catalogPath := "/models"
	if h.provider == ProviderOpenRouter {
		catalogPath = "/models/user"
	}
	if err := h.getCompatibleJSON(ctx, catalogPath, "probe-models", &catalog); err != nil {
		return Capabilities{}, err
	}
	for _, model := range catalog.Data {
		if model.ID != profile.ModelID {
			continue
		}
		if len(model.Architecture.OutputModalities) > 0 && !containsFold(model.Architecture.OutputModalities, "text") {
			return Capabilities{}, &Error{
				Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "unsupported_output_modality", Scope: "probe",
				Err: errors.New("selected model does not provide text output"),
			}
		}
		capabilities := catalogCapabilities(model.SupportedParameters, model.Architecture.InputModalities)
		if len(model.Reasoning.SupportedEfforts) > 0 {
			capabilities.Reasoning = true
			capabilities.ReasoningControls = true
		}
		capabilities.ContextTokens = model.ContextLength
		capabilities.MaxOutputTokens = model.TopProvider.MaxCompletionTokens
		confirmedParameters := model.SupportedParameters
		if h.provider == ProviderOpenRouter {
			contextLimit, outputLimit, endpointParameters, endpointErr := h.probeOpenRouterEndpoint(ctx, model.ID)
			if endpointErr != nil {
				return Capabilities{}, endpointErr
			}
			capabilities.ContextTokens = lowerPositiveLimit(capabilities.ContextTokens, contextLimit)
			capabilities.MaxOutputTokens = lowerPositiveLimit(capabilities.MaxOutputTokens, outputLimit)
			capabilities = confirmEndpointCapabilities(capabilities, endpointParameters)
			confirmedParameters = endpointParameters
		}
		wire, err := h.catalogWireCapabilities(confirmedParameters, model.Reasoning.SupportedEfforts)
		if err != nil {
			return Capabilities{}, err
		}
		h.setWireCapabilities(model.ID, wire)
		return capabilities, nil
	}
	return Capabilities{}, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, StatusCode: http.StatusNotFound, Err: errors.Errorf("model %q is not available", profile.ModelID)}
}

func (h *openAICompatibleHost) getCompatibleJSON(ctx context.Context, path, scope string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+path, nil)
	if err != nil {
		return &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: scope, Err: err}
	}
	request.Header.Set("Authorization", "Bearer "+h.apiKey)
	response, err := h.httpClient.Do(request)
	if err != nil {
		return transportError(h.provider, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderBodyBytes))
	if err != nil {
		return transportError(h.provider, err)
	}
	var envelope struct {
		Error *compatibleError `json:"error,omitempty"`
	}
	decodeErr := json.Unmarshal(body, &envelope)
	if response.StatusCode < 200 || response.StatusCode >= 300 || envelope.Error != nil {
		if decodeErr != nil {
			return statusError(h.provider, response.StatusCode, "undecodable_error_response", scope)
		}
		providerErr := envelope.Error
		if providerErr == nil {
			providerErr = &compatibleError{Code: json.RawMessage(fmt.Sprintf("%d", response.StatusCode))}
		}
		return classifyProviderError(h.provider, response.StatusCode, providerErr, scope)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return &Error{Kind: ErrorMalformed, Provider: h.provider, StatusCode: response.StatusCode, Scope: scope, Err: err}
	}
	return nil
}

func (h *openAICompatibleHost) probeOpenRouterEndpoint(ctx context.Context, modelID string) (int, int, []string, error) {
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return 0, 0, nil, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Scope: "probe-endpoints", Err: errors.New("OpenRouter model ID must include an author and model slug")}
	}
	path := "/models/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/endpoints"
	var catalog modelEndpointCatalog
	if err := h.getCompatibleJSON(ctx, path, "probe-endpoints", &catalog); err != nil {
		return 0, 0, nil, err
	}
	if len(catalog.Data.Endpoints) == 0 {
		return 0, 0, nil, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, ErrorType: "no_available_endpoint", Scope: "probe-endpoints", Err: errors.New("selected model has no available OpenRouter endpoint")}
	}
	selected := catalog.Data.Endpoints[0]
	selectedScore := endpointCapabilityScore(selected.SupportedParameters)
	for _, endpoint := range catalog.Data.Endpoints[1:] {
		score := endpointCapabilityScore(endpoint.SupportedParameters)
		if score > selectedScore {
			selected = endpoint
			selectedScore = score
		}
	}
	return selected.ContextLength, selected.MaxCompletionTokens, append([]string(nil), selected.SupportedParameters...), nil
}

func endpointCapabilityScore(parameters []string) int {
	score := 0
	for _, parameter := range parameters {
		switch strings.ToLower(parameter) {
		case "tools", "tool_choice", "reasoning", "reasoning_effort":
			score++
		}
	}
	return score
}

func confirmEndpointCapabilities(capabilities Capabilities, parameters []string) Capabilities {
	confirmed := catalogCapabilities(parameters, nil)
	capabilities.Tools = capabilities.Tools && confirmed.Tools
	capabilities.ToolChoice = capabilities.ToolChoice && confirmed.ToolChoice
	capabilities.Reasoning = capabilities.Reasoning && confirmed.Reasoning
	capabilities.ReasoningControls = capabilities.ReasoningControls && confirmed.Reasoning
	return capabilities
}

func lowerPositiveLimit(current, candidate int) int {
	if current <= 0 {
		return candidate
	}
	if candidate <= 0 || current <= candidate {
		return current
	}
	return candidate
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func (h *openAICompatibleHost) catalogWireCapabilities(parameters []string, efforts []ReasoningEffort) (compatibleWireCapabilities, error) {
	if h.provider == ProviderNVIDIANIM {
		return compatibleWireCapabilities{}, nil
	}
	reasoningEfforts := make(map[ReasoningEffort]struct{}, len(efforts))
	reasoningParameter := containsFold(parameters, "reasoning") || containsFold(parameters, "reasoning_effort")
	for _, effort := range efforts {
		if !reasoningParameter {
			break
		}
		switch effort {
		case ReasoningLow, ReasoningMedium, ReasoningHigh:
			reasoningEfforts[effort] = struct{}{}
		}
	}
	if h.provider == ProviderOpenRouter {
		return compatibleWireCapabilities{reasoningEfforts: reasoningEfforts}, nil
	}
	return compatibleWireCapabilities{reasoningEfforts: reasoningEfforts}, nil
}

func catalogCapabilities(parameters, modalities []string) Capabilities {
	var result Capabilities
	for _, parameter := range parameters {
		switch strings.ToLower(parameter) {
		case "tools":
			result.Tools = true
		case "tool_choice":
			result.ToolChoice = true
		case "reasoning", "reasoning_effort", "include_reasoning":
			result.Reasoning = true
		}
	}
	for _, modality := range modalities {
		if strings.EqualFold(modality, "image") {
			result.Images = true
		}
	}
	return result
}
