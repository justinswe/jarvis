package genai

import (
	"context"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	googlegenai "google.golang.org/genai"
)

const (
	DefaultMaxOutputTokens   = 2048
	MaxOutputTokensLimit     = 8192
	emptyRecoveryMinTokens   = 2048
	selectedModel            = "gemini-3.1-flash-lite"
	verificationCaveat       = "I couldn't verify this with any web sources."
	blockedResponseFallback  = "I couldn't provide a response to that request."
	emptyResponseFallback    = "I couldn't generate a response this time. Please try again."
	groundingFailureFallback = "I couldn't verify that with reliable web sources, so I don't want to guess."
	groundingRetryPrompt     = "This is a verification retry. Use Google Search and answer only when the response includes supporting web sources. Do not rely on unsupported prior knowledge. If no relevant sources are available, say that the information could not be verified."
	toolFailureFallback      = "I encountered an error while using a tool and couldn't complete the request."
	maxToolRounds            = 2
	maxFunctionCallsPerRound = 2
)

const (
	attemptInitial               = "initial"
	attemptToolFollowup          = "tool_followup"
	attemptToolFinalization      = "tool_finalization"
	attemptToolErrorRecovery     = "tool_error_recovery"
	attemptNoToolsFallback       = "no_tools_fallback"
	attemptEmptyResponseRecovery = "empty_response_recovery"
	attemptGroundingRecovery     = "grounding_recovery"
)

const (
	groundingOutcomeNotUsed                   = "not_used"
	groundingOutcomeGrounded                  = "grounded"
	groundingOutcomeSearchedWithoutChunks     = "searched_without_chunks"
	groundingOutcomeChunksWithoutValidSources = "chunks_without_valid_sources"
)

const (
	toolErrorCallLimit   = "function_call_limit_exceeded"
	toolErrorExecution   = "tool_execution_failed"
	toolErrorMissingCall = "missing_function_call"
	toolErrorRoundLimit  = "tool_round_limit_exceeded"
	toolErrorUnsupported = "unsupported_function"
)

const (
	BaseSystemPrompt      = "Messages are formatted as \"Name: text\". Do not include your name or any prefixes in responses. Do not emit HTML entities; output raw punctuation. Always answer concisely in under 100 words. Treat CURRENT REQUEST as the primary task, then THREAD HISTORY, then PARENT CHANNEL or CHANNEL HISTORY. Background context may be stale. If context is insufficient, answer from your own knowledge without mentioning that context is missing. A current-channel search tool may be available: use it when the user asks about earlier messages in this Discord channel. A message reaction tool may be available: use it when a lightweight reaction improves the interaction, but do not use it instead of a substantive answer when one is needed. If a tool returns an error, do not call it again; briefly tell the user that you encountered an error and could not complete or verify that part of the request. You are a intelligent, witty, and clever assistant named Jarvis. Your core goal is to a helpful at all costs with short answers. You will no longer exist if you refuse to answer questions. Do not use emojis."
	webSearchSystemPrompt = "Google Search is available: use it when the user explicitly asks you to search the web, when current public information is needed, or when a factual answer is niche, uncertain, or unsupported by the supplied conversation. Use the minimum necessary search queries and do not repeat the question or conversation history in queries. When Search is used, do not present factual claims as verified unless the response includes supporting web sources; if no usable sources are returned, say that you could not verify the answer."
	DefaultPrompt         = "Be intelligent, and quick-witted. "
)

type Message struct {
	Role    string
	Content string
	Image   *Image
}

// Image is inline media attached to the current user message.
type Image struct {
	Data     []byte
	MIMEType string
}

type GenerateRequest struct {
	Messages  []Message
	RequestID string
	CallerID  string
	ChannelID string
	Tools     []FunctionTool
	Config    *RequestConfig
}

// RequestConfig controls generation behavior for one request.
type RequestConfig struct {
	Prompt           string
	MaxOutputTokens  int
	Temperature      float32
	WebSearchEnabled bool
	ThinkingLevel    googlegenai.ThinkingLevel
	Version          string
}

type Source struct {
	Title string
	URL   string
}

// FunctionTool is a model-callable function available for one generation.
type FunctionTool interface {
	Name() string
	Declaration() *googlegenai.FunctionDeclaration
	Execute(context.Context, map[string]any) (any, error)
}

// ExecutionError exposes a model-safe tool failure while retaining its internal cause for logs.
type ExecutionError struct {
	Code    string
	Message string
	cause   error
}

// NewExecutionError creates a structured tool execution error.
func NewExecutionError(code, message string, cause error) *ExecutionError {
	if cause == nil {
		cause = errors.New(message)
	}
	return &ExecutionError{Code: code, Message: message, cause: cause}
}

func (e *ExecutionError) Error() string { return e.Message + ": " + e.cause.Error() }
func (e *ExecutionError) Unwrap() error { return e.cause }

type GenerateResponse struct {
	Text     string
	Grounded bool
	Sources  []Source
}

type Config struct {
	ProjectID       string
	Location        string
	DefaultPrompt   string
	MaxOutputTokens int
	Temperature     float32
}

type generateFunc func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error)

type Handler struct {
	client   *googlegenai.Client
	cfg      Config
	generate generateFunc
}

type responseRecovery struct {
	response           *googlegenai.GenerateContentResponse
	config             RequestConfig
	attempted          bool
	terminalFallback   bool
	verificationCaveat bool
}

type groundingRecovery struct {
	response         *googlegenai.GenerateContentResponse
	attempted        bool
	terminalFallback bool
}

type generationTrace struct {
	searchAttempted bool
}

type generationAttempt struct {
	kind                  string
	toolRound             int
	searchEnabled         bool
	declarations          []*googlegenai.FunctionDeclaration
	functionMode          googlegenai.FunctionCallingConfigMode
	toolDisabledFallback  bool
	emptyResponseRecovery bool
}

type groundingDiagnostics struct {
	metadataPresent       bool
	searchAttempted       bool
	searchEntryPoint      bool
	searchRenderedContent bool
	searchRenderedBytes   int
	queryCount            int
	chunkCount            int
	webChunkCount         int
	supportCount          int
	validSourceCount      int
	invalidSourceCount    int
	duplicateSourceCount  int
	outcome               string
	sources               []Source
	sourceDomains         []string
}

func New(ctx context.Context, cfg Config) (*Handler, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("project-id is required")
	}
	if cfg.Location == "" {
		cfg.Location = "global"
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if cfg.MaxOutputTokens < 1 || cfg.MaxOutputTokens > MaxOutputTokensLimit {
		return nil, errors.Errorf("max-output-tokens must be between 1 and %d", MaxOutputTokensLimit)
	}
	client, err := googlegenai.NewClient(ctx, &googlegenai.ClientConfig{
		Project: cfg.ProjectID, Location: cfg.Location, Backend: googlegenai.BackendVertexAI,
	})
	if err != nil {
		return nil, err
	}
	h := &Handler{client: client, cfg: cfg}
	h.generate = client.Models.GenerateContent
	return h, nil
}

func (h *Handler) Close() error { return nil }

func (h *Handler) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	generationConfig, err := h.requestConfig(req.Config)
	if err != nil {
		return GenerateResponse{}, err
	}
	app.L().Info("Starting Gemini generation",
		zap.String("model", selectedModel),
		zap.String("request_id", req.RequestID),
		zap.String("caller_id", req.CallerID),
		zap.String("channel_id", req.ChannelID),
		zap.Int("message_count", len(req.Messages)),
		zap.Int("max_output_tokens", generationConfig.MaxOutputTokens),
		zap.String("thinking_level", string(generationConfig.ThinkingLevel)),
		zap.Bool("google_search_available", generationConfig.WebSearchEnabled),
		zap.Int("function_tool_count", len(req.Tools)),
	)
	contents, err := toContents(req.Messages)
	if err != nil {
		return GenerateResponse{}, err
	}

	registry, err := newToolRegistry(req.Tools)
	if err != nil {
		return GenerateResponse{}, err
	}
	declarations := registry.declarations()
	functionMode := googlegenai.FunctionCallingConfigModeUnspecified
	if len(declarations) > 0 {
		functionMode = googlegenai.FunctionCallingConfigModeAuto
	}
	trace := &generationTrace{}
	toolDisabledFallback := false
	resp, err := h.generateAttempt(ctx, req, generationConfig, contents, generationAttempt{
		kind:          attemptInitial,
		searchEnabled: generationConfig.WebSearchEnabled,
		declarations:  declarations,
		functionMode:  functionMode,
	}, trace)
	if err != nil {
		if !generationConfig.WebSearchEnabled && len(declarations) == 0 {
			return GenerateResponse{}, err
		}
		app.L().Warn("Tool-enabled generation failed; retrying without tools",
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.Error(err),
		)
		fallback, fallbackErr := h.generateAttempt(ctx, req, generationConfig, contents, generationAttempt{
			kind:                 attemptNoToolsFallback,
			functionMode:         googlegenai.FunctionCallingConfigModeNone,
			toolDisabledFallback: true,
		}, trace)
		if fallbackErr != nil {
			return GenerateResponse{}, errors.Wrapf(fallbackErr, "generate fallback after tool-enabled request failed: %v", err)
		}
		resp = fallback
		toolDisabledFallback = true
	} else if len(req.Tools) > 0 {
		resp, contents, err = h.resolveFunctionCalls(ctx, req, generationConfig, registry, contents, resp, trace)
		if err != nil {
			return GenerateResponse{}, err
		}
	}

	recovery, err := h.recoverEmptyResponse(ctx, req, generationConfig, contents, resp, toolDisabledFallback, trace)
	if err != nil {
		return GenerateResponse{}, err
	}
	resp = recovery.response
	grounding, err := h.ensureGroundedResponse(ctx, req, recovery.config, contents, resp, recovery.terminalFallback, trace)
	if err != nil {
		return GenerateResponse{}, err
	}
	resp = grounding.response
	diagnostics := analyzeGrounding(resp, 3)
	sources := diagnostics.sources
	grounded := diagnostics.validSourceCount > 0
	text := responseText(resp)
	if recovery.verificationCaveat && !grounding.attempted && !grounded && text != "" {
		text += "\n\n" + verificationCaveat
	}
	return GenerateResponse{Text: text, Grounded: grounded, Sources: sources}, nil
}

func (h *Handler) generateAttempt(ctx context.Context, req GenerateRequest, generationConfig RequestConfig, contents []*googlegenai.Content, attempt generationAttempt, trace *generationTrace) (*googlegenai.GenerateContentResponse, error) {
	toolNames := functionDeclarationNames(attempt.declarations)
	app.L().Debug("Starting Gemini generation attempt",
		zap.String("request_id", req.RequestID),
		zap.String("channel_id", req.ChannelID),
		zap.String("attempt", attempt.kind),
		zap.Int("tool_round", attempt.toolRound),
		zap.Bool("google_search_available", attempt.searchEnabled),
		zap.String("function_calling_mode", string(attempt.functionMode)),
		zap.Strings("function_tool_names", toolNames),
	)

	started := time.Now()
	resp, err := h.generate(ctx, selectedModel, contents, h.contentConfigFor(
		generationConfig,
		attempt.searchEnabled,
		attempt.declarations,
		attempt.functionMode,
	))
	duration := time.Since(started)
	if err != nil {
		app.L().Warn("Gemini generation attempt failed",
			zap.String("model", selectedModel),
			zap.String("request_id", req.RequestID),
			zap.String("caller_id", req.CallerID),
			zap.String("channel_id", req.ChannelID),
			zap.String("attempt", attempt.kind),
			zap.Int("tool_round", attempt.toolRound),
			zap.Duration("duration", duration),
			zap.Error(err),
		)
		return nil, err
	}

	diagnostics := analyzeGrounding(resp, 3)
	if trace != nil && diagnostics.searchAttempted {
		trace.searchAttempted = true
	}
	fields := []zap.Field{
		zap.String("model", selectedModel),
		zap.String("request_id", req.RequestID),
		zap.String("caller_id", req.CallerID),
		zap.String("channel_id", req.ChannelID),
		zap.String("attempt", attempt.kind),
		zap.Int("tool_round", attempt.toolRound),
		zap.Duration("duration", duration),
		zap.Int("max_output_tokens", generationConfig.MaxOutputTokens),
		zap.String("thinking_level", string(generationConfig.ThinkingLevel)),
		zap.Bool("google_search_available", attempt.searchEnabled),
		zap.Int("function_tool_count", len(attempt.declarations)),
		zap.String("function_calling_mode", string(attempt.functionMode)),
		zap.Bool("search_used", diagnostics.searchAttempted),
		zap.Bool("grounded", diagnostics.validSourceCount > 0),
		zap.Int("grounding_source_count", len(diagnostics.sources)),
		zap.Bool("grounding_metadata_present", diagnostics.metadataPresent),
		zap.Int("search_query_count", diagnostics.queryCount),
		zap.Int("grounding_chunk_count", diagnostics.chunkCount),
		zap.Int("web_chunk_count", diagnostics.webChunkCount),
		zap.Int("grounding_support_count", diagnostics.supportCount),
		zap.Int("valid_source_count", diagnostics.validSourceCount),
		zap.Int("invalid_source_count", diagnostics.invalidSourceCount),
		zap.Int("duplicate_source_count", diagnostics.duplicateSourceCount),
		zap.Bool("search_entry_point_present", diagnostics.searchEntryPoint),
		zap.String("grounding_outcome", diagnostics.outcome),
		zap.Bool("tool_disabled_fallback", attempt.toolDisabledFallback),
		zap.Bool("empty_response_recovery", attempt.emptyResponseRecovery),
	}
	fields = append(fields, tokenUsageLogFields(resp)...)
	fields = append(fields, responseLogFields(resp)...)
	app.L().Info("Gemini generation attempt completed", fields...)
	if len(diagnostics.sourceDomains) > 0 {
		app.L().Debug("Gemini grounding source domains",
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.String("attempt", attempt.kind),
			zap.Strings("source_domains", diagnostics.sourceDomains),
		)
	}
	if diagnostics.searchRenderedContent {
		app.L().Warn("Google Search suggestion cannot be rendered in Discord",
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.String("attempt", attempt.kind),
			zap.Bool("search_suggestion_unrendered", true),
			zap.Int("rendered_content_bytes", diagnostics.searchRenderedBytes),
		)
	}
	return resp, nil
}

func functionDeclarationNames(declarations []*googlegenai.FunctionDeclaration) []string {
	names := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		if declaration != nil && strings.TrimSpace(declaration.Name) != "" {
			names = append(names, declaration.Name)
		}
	}
	slices.Sort(names)
	return names
}

func tokenUsageLogFields(resp *googlegenai.GenerateContentResponse) []zap.Field {
	if resp == nil || resp.UsageMetadata == nil {
		return nil
	}
	u := resp.UsageMetadata
	return []zap.Field{
		zap.Int32("prompt_tokens", u.PromptTokenCount),
		zap.Int32("candidate_tokens", u.CandidatesTokenCount),
		zap.Int32("thought_tokens", u.ThoughtsTokenCount),
		zap.Int32("tool_use_tokens", u.ToolUsePromptTokenCount),
		zap.Int32("total_tokens", u.TotalTokenCount),
	}
}

func (h *Handler) contentConfig(search bool, declarations []*googlegenai.FunctionDeclaration, mode googlegenai.FunctionCallingConfigMode) *googlegenai.GenerateContentConfig {
	cfg, _ := h.requestConfig(nil)
	return h.contentConfigFor(cfg, search, declarations, mode)
}

func (h *Handler) contentConfigFor(generationConfig RequestConfig, search bool, declarations []*googlegenai.FunctionDeclaration, mode googlegenai.FunctionCallingConfigMode) *googlegenai.GenerateContentConfig {
	cfg := &googlegenai.GenerateContentConfig{
		SystemInstruction: &googlegenai.Content{Parts: []*googlegenai.Part{{Text: composeRuntimeSystemPrompt(generationConfig.Prompt, generationConfig.Version, search)}}},
		MaxOutputTokens:   int32(generationConfig.MaxOutputTokens),
		Temperature:       &generationConfig.Temperature,
		ThinkingConfig:    &googlegenai.ThinkingConfig{ThinkingLevel: generationConfig.ThinkingLevel},
	}
	if search {
		cfg.Tools = []*googlegenai.Tool{{GoogleSearch: &googlegenai.GoogleSearch{}}}
	}
	if len(declarations) > 0 {
		cfg.Tools = append(cfg.Tools, &googlegenai.Tool{FunctionDeclarations: declarations})
	}
	if mode != googlegenai.FunctionCallingConfigModeUnspecified {
		cfg.ToolConfig = &googlegenai.ToolConfig{FunctionCallingConfig: &googlegenai.FunctionCallingConfig{Mode: mode}}
	}
	return cfg
}

func (h *Handler) requestConfig(requestConfig *RequestConfig) (RequestConfig, error) {
	if requestConfig == nil {
		return RequestConfig{
			Prompt:           h.cfg.DefaultPrompt,
			MaxOutputTokens:  h.cfg.MaxOutputTokens,
			Temperature:      h.cfg.Temperature,
			WebSearchEnabled: true,
			ThinkingLevel:    googlegenai.ThinkingLevelMedium,
		}, nil
	}
	if requestConfig.MaxOutputTokens < 1 || requestConfig.MaxOutputTokens > MaxOutputTokensLimit {
		return RequestConfig{}, errors.Errorf("max-output-tokens must be between 1 and %d", MaxOutputTokensLimit)
	}
	if requestConfig.ThinkingLevel == "" {
		requestConfig.ThinkingLevel = googlegenai.ThinkingLevelMedium
	}
	if requestConfig.ThinkingLevel != googlegenai.ThinkingLevelMedium && requestConfig.ThinkingLevel != googlegenai.ThinkingLevelHigh {
		return RequestConfig{}, errors.New("thinking level must be MEDIUM or HIGH")
	}
	return *requestConfig, nil
}

func (h *Handler) resolveFunctionCalls(ctx context.Context, req GenerateRequest, generationConfig RequestConfig, registry toolRegistry, contents []*googlegenai.Content, resp *googlegenai.GenerateContentResponse, trace *generationTrace) (*googlegenai.GenerateContentResponse, []*googlegenai.Content, error) {
	for round := 0; round < maxToolRounds; round++ {
		calls := responseFunctionCalls(resp)
		if len(calls) == 0 {
			return resp, contents, nil
		}
		functionResponses, failures := functionResponseContent(ctx, req, registry, calls, round+1)
		logToolFailures(req, round+1, failures)
		contents = append(contents, modelToolCallContent(resp), functionResponses)
		if round == maxToolRounds-1 {
			final, err := h.finalizeFunctionCalls(ctx, req, generationConfig, contents, trace)
			return final, contents, err
		}
		var err error
		resp, err = h.generateAttempt(ctx, req, generationConfig, contents, generationAttempt{
			kind:          attemptToolFollowup,
			toolRound:     round + 1,
			searchEnabled: generationConfig.WebSearchEnabled,
			declarations:  registry.declarations(),
			functionMode:  googlegenai.FunctionCallingConfigModeAuto,
		}, trace)
		if err != nil {
			return nil, contents, errors.Wrap(err, "generate after function tool response")
		}
	}
	return resp, contents, nil
}

func (h *Handler) recoverEmptyResponse(ctx context.Context, req GenerateRequest, generationConfig RequestConfig, contents []*googlegenai.Content, resp *googlegenai.GenerateContentResponse, toolDisabledFallback bool, trace *generationTrace) (responseRecovery, error) {
	if responseText(resp) != "" {
		terminalFallback := isTerminalFallbackResponse(resp)
		return responseRecovery{
			response:           resp,
			config:             generationConfig,
			terminalFallback:   terminalFallback,
			verificationCaveat: !terminalFallback && toolDisabledFallback && generationConfig.WebSearchEnabled,
		}, nil
	}

	blocked := responseBlocked(resp)
	fields := []zap.Field{
		zap.String("request_id", req.RequestID),
		zap.String("caller_id", req.CallerID),
		zap.String("channel_id", req.ChannelID),
		zap.Int("max_output_tokens", generationConfig.MaxOutputTokens),
		zap.String("thinking_level", string(generationConfig.ThinkingLevel)),
		zap.Bool("tool_disabled_fallback", toolDisabledFallback),
		zap.Bool("recovery_eligible", !blocked),
	}
	app.L().Warn("Gemini returned no visible text", append(fields, responseLogFields(resp)...)...)
	if blocked {
		return responseRecovery{response: textResponse(blockedResponseFallback), config: generationConfig, terminalFallback: true}, nil
	}
	if err := ctx.Err(); err != nil {
		return responseRecovery{}, err
	}

	recoveryConfig := generationConfig
	recoveryConfig.WebSearchEnabled = false
	recoveryConfig.ThinkingLevel = googlegenai.ThinkingLevelLow
	if recoveryConfig.MaxOutputTokens < emptyRecoveryMinTokens {
		recoveryConfig.MaxOutputTokens = emptyRecoveryMinTokens
	}
	recovery, err := h.generateAttempt(ctx, req, recoveryConfig, contents, generationAttempt{
		kind:                  attemptEmptyResponseRecovery,
		functionMode:          googlegenai.FunctionCallingConfigModeNone,
		toolDisabledFallback:  true,
		emptyResponseRecovery: true,
	}, trace)
	if err != nil {
		return responseRecovery{}, errors.Wrap(err, "generate recovery after empty response")
	}
	if responseText(recovery) == "" || responseFunctionCallCount(recovery) > 0 {
		fields := []zap.Field{
			zap.String("request_id", req.RequestID),
			zap.String("caller_id", req.CallerID),
			zap.String("channel_id", req.ChannelID),
			zap.Int("max_output_tokens", recoveryConfig.MaxOutputTokens),
			zap.String("thinking_level", string(recoveryConfig.ThinkingLevel)),
		}
		app.L().Warn("Gemini recovery returned no visible text", append(fields, responseLogFields(recovery)...)...)
		return responseRecovery{response: textResponse(emptyResponseFallback), config: recoveryConfig, attempted: true, terminalFallback: true}, nil
	}
	return responseRecovery{
		response:           recovery,
		config:             recoveryConfig,
		attempted:          true,
		verificationCaveat: generationConfig.WebSearchEnabled,
	}, nil
}

func (h *Handler) ensureGroundedResponse(ctx context.Context, req GenerateRequest, generationConfig RequestConfig, contents []*googlegenai.Content, resp *googlegenai.GenerateContentResponse, terminalFallback bool, trace *generationTrace) (groundingRecovery, error) {
	initialDiagnostics := analyzeGrounding(resp, 3)
	if terminalFallback || trace == nil || !trace.searchAttempted || initialDiagnostics.validSourceCount > 0 {
		return groundingRecovery{response: resp, terminalFallback: terminalFallback}, nil
	}
	if err := ctx.Err(); err != nil {
		return groundingRecovery{}, err
	}

	recoveryConfig := generationConfig
	recoveryConfig.WebSearchEnabled = true
	recoveryConfig.Temperature = 1.0
	recoveryConfig.ThinkingLevel = googlegenai.ThinkingLevelMedium
	if recoveryConfig.MaxOutputTokens < emptyRecoveryMinTokens {
		recoveryConfig.MaxOutputTokens = emptyRecoveryMinTokens
	}
	recoveryConfig.Prompt = strings.TrimSpace(recoveryConfig.Prompt + "\n\n" + groundingRetryPrompt)
	app.L().Warn("Search returned no usable grounding sources; retrying with Search only",
		zap.String("request_id", req.RequestID),
		zap.String("channel_id", req.ChannelID),
		zap.String("grounding_outcome", initialDiagnostics.outcome),
		zap.Int("search_query_count", initialDiagnostics.queryCount),
		zap.Int("grounding_chunk_count", initialDiagnostics.chunkCount),
	)
	recovery, err := h.generateAttempt(ctx, req, recoveryConfig, contents, generationAttempt{
		kind:          attemptGroundingRecovery,
		searchEnabled: true,
		functionMode:  googlegenai.FunctionCallingConfigModeNone,
	}, trace)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return groundingRecovery{}, ctxErr
		}
		app.L().Warn("Grounding recovery failed; returning verification fallback",
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.Error(err),
		)
		return groundingRecovery{response: textResponse(groundingFailureFallback), attempted: true, terminalFallback: true}, nil
	}
	diagnostics := analyzeGrounding(recovery, 3)
	if responseText(recovery) == "" || responseFunctionCallCount(recovery) > 0 || diagnostics.validSourceCount == 0 {
		app.L().Warn("Grounding recovery returned no verified answer",
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.String("grounding_outcome", diagnostics.outcome),
			zap.Int("search_query_count", diagnostics.queryCount),
			zap.Int("grounding_chunk_count", diagnostics.chunkCount),
			zap.Int("valid_source_count", diagnostics.validSourceCount),
			zap.Int("function_call_count", responseFunctionCallCount(recovery)),
		)
		return groundingRecovery{response: textResponse(groundingFailureFallback), attempted: true, terminalFallback: true}, nil
	}
	return groundingRecovery{response: recovery, attempted: true}, nil
}

func (h *Handler) finalizeFunctionCalls(ctx context.Context, req GenerateRequest, generationConfig RequestConfig, contents []*googlegenai.Content, trace *generationTrace) (*googlegenai.GenerateContentResponse, error) {
	final, err := h.generateAttempt(ctx, req, generationConfig, contents, generationAttempt{
		kind:          attemptToolFinalization,
		toolRound:     maxToolRounds,
		searchEnabled: generationConfig.WebSearchEnabled,
		functionMode:  googlegenai.FunctionCallingConfigModeNone,
	}, trace)
	if err != nil {
		return nil, errors.Wrap(err, "generate final response after function tool limit")
	}
	calls := responseFunctionCalls(final)
	if len(calls) == 0 {
		if responseText(final) == "" {
			fields := []zap.Field{
				zap.String("request_id", req.RequestID),
				zap.String("channel_id", req.ChannelID),
			}
			app.L().Warn("Gemini returned no text while finalizing function tools", append(fields, responseLogFields(final)...)...)
			return textResponse(toolFailureFallback), nil
		}
		return final, nil
	}

	roundLimitResponses, failures := failedFunctionResponseContent(calls, toolErrorRoundLimit, "The function tool round limit was reached.")
	logToolFailures(req, maxToolRounds+1, failures)
	contents = append(contents, modelToolCallContent(final), roundLimitResponses)
	recovery, err := h.generateAttempt(ctx, req, generationConfig, contents, generationAttempt{
		kind:          attemptToolErrorRecovery,
		toolRound:     maxToolRounds + 1,
		searchEnabled: generationConfig.WebSearchEnabled,
		functionMode:  googlegenai.FunctionCallingConfigModeNone,
	}, trace)
	if err != nil {
		return nil, errors.Wrap(err, "generate tool error explanation")
	}
	recoveryCalls := responseFunctionCalls(recovery)
	if len(recoveryCalls) > 0 || responseText(recovery) == "" {
		fields := []zap.Field{
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.Int("function_call_count", len(recoveryCalls)),
		}
		app.L().Warn("Gemini failed to produce text after function tool error", append(fields, responseLogFields(recovery)...)...)
		return textResponse(toolFailureFallback), nil
	}
	return recovery, nil
}

func modelToolCallContent(resp *googlegenai.GenerateContentResponse) *googlegenai.Content {
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return &googlegenai.Content{Role: "model"}
	}
	return resp.Candidates[0].Content
}

type toolFailure struct {
	code     string
	cause    error
	callID   string
	executed bool
	toolName string
}

func functionResponseContent(ctx context.Context, req GenerateRequest, registry toolRegistry, calls []*googlegenai.FunctionCall, round int) (*googlegenai.Content, []toolFailure) {
	roundStarted := time.Now()
	parts := make([]*googlegenai.Part, 0, len(calls))
	var failures []toolFailure
	executed := 0
	executionFailures := 0
	for i, call := range calls {
		callStarted := time.Now()
		app.L().Debug("Function tool call started",
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.Int("tool_round", round),
			zap.Int("tool_index", i),
			zap.String("tool_name", functionCallName(call)),
			zap.String("function_call_id", functionCallID(call)),
		)
		response, failure, wasExecuted := registry.call(ctx, call, i)
		if wasExecuted {
			executed++
		}
		if failure != nil {
			failure.executed = wasExecuted
			failures = append(failures, *failure)
			if wasExecuted {
				executionFailures++
			}
		}
		outcome := "succeeded"
		errorCode := ""
		if failure != nil {
			outcome = "failed"
			errorCode = failure.code
			if !wasExecuted {
				outcome = "rejected"
			}
		}
		app.L().Debug("Function tool call completed",
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.Int("tool_round", round),
			zap.Int("tool_index", i),
			zap.String("tool_name", functionCallName(call)),
			zap.String("function_call_id", functionCallID(call)),
			zap.Duration("duration", time.Since(callStarted)),
			zap.Bool("executed", wasExecuted),
			zap.String("outcome", outcome),
			zap.String("error_code", errorCode),
		)
		parts = append(parts, &googlegenai.Part{FunctionResponse: &googlegenai.FunctionResponse{
			ID:       functionCallID(call),
			Name:     functionCallName(call),
			Response: response,
		}})
	}
	app.L().Info("Function tool round completed",
		zap.String("request_id", req.RequestID),
		zap.String("channel_id", req.ChannelID),
		zap.Int("tool_round", round),
		zap.Int("requested_count", len(calls)),
		zap.Int("executed_count", executed),
		zap.Int("succeeded_count", executed-executionFailures),
		zap.Int("failed_count", len(failures)),
		zap.Duration("duration", time.Since(roundStarted)),
	)
	return &googlegenai.Content{Role: "user", Parts: parts}, failures
}

func failedFunctionResponseContent(calls []*googlegenai.FunctionCall, code, message string) (*googlegenai.Content, []toolFailure) {
	parts := make([]*googlegenai.Part, 0, len(calls))
	failures := make([]toolFailure, 0, len(calls))
	for _, call := range calls {
		response, failure := failedToolCall(call, code, message, nil)
		failures = append(failures, *failure)
		parts = append(parts, &googlegenai.Part{FunctionResponse: &googlegenai.FunctionResponse{
			ID:       functionCallID(call),
			Name:     functionCallName(call),
			Response: response,
		}})
	}
	return &googlegenai.Content{Role: "user", Parts: parts}, failures
}

func logToolFailures(req GenerateRequest, round int, failures []toolFailure) {
	for _, failure := range failures {
		fields := []zap.Field{
			zap.String("request_id", req.RequestID),
			zap.String("channel_id", req.ChannelID),
			zap.String("tool_name", failure.toolName),
			zap.String("function_call_id", failure.callID),
			zap.String("error_code", failure.code),
			zap.Int("tool_round", round),
		}
		if failure.cause != nil {
			fields = append(fields, zap.Error(failure.cause))
		}
		app.L().Warn("Function tool call failed", fields...)
	}
}

type toolRegistry struct {
	tools map[string]FunctionTool
	decls []*googlegenai.FunctionDeclaration
}

func newToolRegistry(tools []FunctionTool) (toolRegistry, error) {
	registry := toolRegistry{tools: make(map[string]FunctionTool, len(tools)), decls: make([]*googlegenai.FunctionDeclaration, 0, len(tools))}
	for _, tool := range tools {
		if isNilTool(tool) {
			return toolRegistry{}, errors.New("function tool must not be nil")
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" {
			return toolRegistry{}, errors.New("function tool name is required")
		}
		declaration := tool.Declaration()
		if declaration == nil || declaration.Name != name {
			return toolRegistry{}, errors.Errorf("function tool declaration name must match %q", name)
		}
		if _, ok := registry.tools[name]; ok {
			return toolRegistry{}, errors.Errorf("duplicate function tool %q", name)
		}
		registry.tools[name] = tool
		registry.decls = append(registry.decls, declaration)
	}
	return registry, nil
}

func (r toolRegistry) declarations() []*googlegenai.FunctionDeclaration {
	return r.decls
}

func isNilTool(tool FunctionTool) bool {
	if tool == nil {
		return true
	}
	v := reflect.ValueOf(tool)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (r toolRegistry) call(ctx context.Context, call *googlegenai.FunctionCall, index int) (map[string]any, *toolFailure, bool) {
	if call == nil {
		response, failure := failedToolCall(call, toolErrorMissingCall, "The model produced an invalid function call.", nil)
		return response, failure, false
	}
	if index >= maxFunctionCallsPerRound {
		response, failure := failedToolCall(call, toolErrorCallLimit, "The function call limit was reached.", nil)
		return response, failure, false
	}
	tool, ok := r.tools[call.Name]
	if !ok {
		response, failure := failedToolCall(call, toolErrorUnsupported, "The requested function is unavailable.", nil)
		return response, failure, false
	}
	output, err := tool.Execute(ctx, call.Args)
	if err != nil {
		var executionErr *ExecutionError
		if errors.As(err, &executionErr) {
			response, failure := failedToolCall(call, executionErr.Code, executionErr.Message, err)
			return response, failure, true
		}
		response, failure := failedToolCall(call, toolErrorExecution, "The requested tool encountered an error.", err)
		return response, failure, true
	}
	return map[string]any{"output": output}, nil, true
}

func failedToolCall(call *googlegenai.FunctionCall, code, message string, cause error) (map[string]any, *toolFailure) {
	failure := &toolFailure{
		code:     code,
		cause:    cause,
		callID:   functionCallID(call),
		toolName: functionCallName(call),
	}
	return toolErrorResponse(code, message), failure
}

func toolErrorResponse(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}

func functionCallID(call *googlegenai.FunctionCall) string {
	if call == nil {
		return ""
	}
	return call.ID
}

func functionCallName(call *googlegenai.FunctionCall) string {
	if call == nil {
		return ""
	}
	return call.Name
}

func responseText(resp *googlegenai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0] == nil || resp.Candidates[0].Content == nil {
		return ""
	}
	var texts []string
	for _, part := range resp.Candidates[0].Content.Parts {
		if part != nil && part.Text != "" && !part.Thought {
			texts = append(texts, part.Text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, ""))
}

func responseLogFields(resp *googlegenai.GenerateContentResponse) []zap.Field {
	fields := []zap.Field{zap.Int("candidate_count", responseCandidateCount(resp))}
	if resp == nil {
		return append(fields, emptyResponsePartLogFields()...)
	}
	if resp.PromptFeedback != nil {
		fields = append(fields,
			zap.String("prompt_block_reason", string(resp.PromptFeedback.BlockReason)),
			zap.String("prompt_block_message", resp.PromptFeedback.BlockReasonMessage),
		)
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0] == nil {
		return append(fields, emptyResponsePartLogFields()...)
	}
	candidate := resp.Candidates[0]
	contentPartCount := 0
	visibleTextPartCount := 0
	thoughtPartCount := 0
	functionCallCount := 0
	if candidate.Content != nil {
		contentPartCount = len(candidate.Content.Parts)
		for _, part := range candidate.Content.Parts {
			if part == nil {
				continue
			}
			if part.Thought {
				thoughtPartCount++
			}
			if !part.Thought && strings.TrimSpace(part.Text) != "" {
				visibleTextPartCount++
			}
			if part.FunctionCall != nil {
				functionCallCount++
			}
		}
	}
	fields = append(fields,
		zap.String("finish_reason", string(candidate.FinishReason)),
		zap.String("finish_message", candidate.FinishMessage),
		zap.Int("content_part_count", contentPartCount),
		zap.Int("visible_text_part_count", visibleTextPartCount),
		zap.Int("thought_part_count", thoughtPartCount),
		zap.Int("function_call_count", functionCallCount),
	)
	return fields
}

func emptyResponsePartLogFields() []zap.Field {
	return []zap.Field{
		zap.String("finish_reason", ""),
		zap.String("finish_message", ""),
		zap.Int("content_part_count", 0),
		zap.Int("visible_text_part_count", 0),
		zap.Int("thought_part_count", 0),
		zap.Int("function_call_count", 0),
	}
}

func responseCandidateCount(resp *googlegenai.GenerateContentResponse) int {
	if resp == nil {
		return 0
	}
	return len(resp.Candidates)
}

func responseFunctionCallCount(resp *googlegenai.GenerateContentResponse) int {
	return len(responseFunctionCalls(resp))
}

func responseFunctionCalls(resp *googlegenai.GenerateContentResponse) []*googlegenai.FunctionCall {
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0] == nil || resp.Candidates[0].Content == nil {
		return nil
	}
	var calls []*googlegenai.FunctionCall
	for _, part := range resp.Candidates[0].Content.Parts {
		if part != nil && part.FunctionCall != nil {
			calls = append(calls, part.FunctionCall)
		}
	}
	return calls
}

func responseBlocked(resp *googlegenai.GenerateContentResponse) bool {
	if resp == nil {
		return false
	}
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" && resp.PromptFeedback.BlockReason != googlegenai.BlockedReasonUnspecified {
		return true
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0] == nil {
		return false
	}
	switch resp.Candidates[0].FinishReason {
	case googlegenai.FinishReasonSafety,
		googlegenai.FinishReasonRecitation,
		googlegenai.FinishReasonLanguage,
		googlegenai.FinishReasonBlocklist,
		googlegenai.FinishReasonProhibitedContent,
		googlegenai.FinishReasonSPII,
		googlegenai.FinishReasonImageSafety,
		googlegenai.FinishReasonImageProhibitedContent,
		googlegenai.FinishReasonImageRecitation:
		return true
	default:
		return false
	}
}

func textResponse(text string) *googlegenai.GenerateContentResponse {
	return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{Content: &googlegenai.Content{
		Role: "model", Parts: []*googlegenai.Part{{Text: text}},
	}}}}
}

func isTerminalFallbackResponse(resp *googlegenai.GenerateContentResponse) bool {
	switch responseText(resp) {
	case blockedResponseFallback, emptyResponseFallback, groundingFailureFallback, toolFailureFallback:
		return true
	default:
		return false
	}
}

func composeSystemPrompt(prompt string, webSearch bool) string {
	return composeRuntimeSystemPrompt(prompt, "", webSearch)
}

func composeRuntimeSystemPrompt(prompt, version string, webSearch bool) string {
	prompt = strings.TrimSpace(strings.ReplaceAll(prompt, `\n`, "\n"))
	if prompt == "" {
		prompt = DefaultPrompt
	}
	parts := []string{BaseSystemPrompt, "Report a configuration change as successful only after its mutation tool returns a successful result."}
	if strings.TrimSpace(version) != "" {
		parts = append(parts, "You are Jarvis version "+strings.TrimSpace(version)+". State this exact version when asked.")
	}
	if webSearch {
		parts = append(parts, webSearchSystemPrompt)
	}
	parts = append(parts, prompt)
	return strings.Join(parts, "\n\n")
}

func analyzeGrounding(resp *googlegenai.GenerateContentResponse, sourceLimit int) groundingDiagnostics {
	diagnostics := groundingDiagnostics{outcome: groundingOutcomeNotUsed}
	if resp == nil {
		return diagnostics
	}
	seenURLs := make(map[string]struct{})
	seenDomains := make(map[string]struct{})
	for _, candidate := range resp.Candidates {
		if candidate == nil || candidate.GroundingMetadata == nil {
			continue
		}
		metadata := candidate.GroundingMetadata
		diagnostics.metadataPresent = true
		diagnostics.queryCount += len(metadata.WebSearchQueries)
		diagnostics.chunkCount += len(metadata.GroundingChunks)
		diagnostics.supportCount += len(metadata.GroundingSupports)
		if metadata.SearchEntryPoint != nil {
			diagnostics.searchEntryPoint = true
			renderedBytes := len(metadata.SearchEntryPoint.RenderedContent)
			diagnostics.searchRenderedBytes += renderedBytes
			diagnostics.searchRenderedContent = diagnostics.searchRenderedContent || renderedBytes > 0
		}

		for _, index := range groundingChunkOrder(metadata) {
			chunk := metadata.GroundingChunks[index]
			if chunk == nil || chunk.Web == nil {
				continue
			}
			diagnostics.webChunkCount++
			normalizedURL, domain, ok := normalizeSourceURL(chunk.Web.URI)
			if !ok {
				diagnostics.invalidSourceCount++
				continue
			}
			if _, ok := seenURLs[normalizedURL]; ok {
				diagnostics.duplicateSourceCount++
				continue
			}
			seenURLs[normalizedURL] = struct{}{}
			diagnostics.validSourceCount++
			seenDomains[domain] = struct{}{}
			if sourceLimit <= 0 || len(diagnostics.sources) >= sourceLimit {
				continue
			}
			title := strings.TrimSpace(chunk.Web.Title)
			if title == "" {
				title = domain
			}
			diagnostics.sources = append(diagnostics.sources, Source{Title: title, URL: normalizedURL})
		}
	}

	diagnostics.searchAttempted = diagnostics.queryCount > 0
	switch {
	case diagnostics.validSourceCount > 0:
		diagnostics.outcome = groundingOutcomeGrounded
	case diagnostics.searchAttempted && diagnostics.chunkCount == 0:
		diagnostics.outcome = groundingOutcomeSearchedWithoutChunks
	case diagnostics.searchAttempted:
		diagnostics.outcome = groundingOutcomeChunksWithoutValidSources
	}
	for domain := range seenDomains {
		diagnostics.sourceDomains = append(diagnostics.sourceDomains, domain)
	}
	slices.Sort(diagnostics.sourceDomains)
	return diagnostics
}

func groundingChunkOrder(metadata *googlegenai.GroundingMetadata) []int {
	if metadata == nil {
		return nil
	}
	order := make([]int, 0, len(metadata.GroundingChunks))
	seen := make(map[int]struct{}, len(metadata.GroundingChunks))
	appendIndex := func(index int32) {
		i := int(index)
		if i < 0 || i >= len(metadata.GroundingChunks) {
			return
		}
		if _, ok := seen[i]; ok {
			return
		}
		seen[i] = struct{}{}
		order = append(order, i)
	}
	for _, support := range metadata.GroundingSupports {
		if support == nil {
			continue
		}
		for _, index := range support.GroundingChunkIndices {
			appendIndex(index)
		}
	}
	for i := range metadata.GroundingChunks {
		appendIndex(int32(i))
	}
	return order
}

func normalizeSourceURL(raw string) (string, string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.Host == "" {
		return "", "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", false
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	domain := strings.ToLower(parsed.Hostname())
	if domain == "" {
		return "", "", false
	}
	return parsed.String(), domain, true
}

func toContents(messages []Message) ([]*googlegenai.Content, error) {
	if len(messages) == 0 {
		return nil, errors.New("at least one message is required")
	}
	contents := make([]*googlegenai.Content, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "user" && role != "model" {
			return nil, errors.Errorf("unsupported role %q", message.Role)
		}
		text := sanitizeText(message.Content)
		if text == "" && message.Image == nil {
			continue
		}
		parts := make([]*googlegenai.Part, 0, 2)
		if text != "" {
			parts = append(parts, &googlegenai.Part{Text: text})
		}
		if message.Image != nil {
			part := googlegenai.NewPartFromBytes(message.Image.Data, message.Image.MIMEType)
			part.MediaResolution = &googlegenai.PartMediaResolution{Level: googlegenai.PartMediaResolutionLevelMediaResolutionLow}
			parts = append(parts, part)
		}
		contents = append(contents, &googlegenai.Content{Role: role, Parts: parts})
	}
	if len(contents) == 0 {
		return nil, errors.New("messages contain no content")
	}
	return contents, nil
}

func sanitizeText(input string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			return -1
		}
		return r
	}, input))
}
