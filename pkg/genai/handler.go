package genai

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/justinswe/std/app"
	"go.uber.org/zap"
	googlegenai "google.golang.org/genai"
)

const (
	// DefaultModel is the default Vertex AI Gemini model.
	DefaultModel = "google/gemini-3.5-flash"
	// DefaultSystemPrompt
	DefaultSystemPrompt = "You are a helpful assistant named Jarvis. Messages are formatted as \"Name: text\". Do not include your name or any prefixes in responses. Do not emit HTML entities; output raw punctuation. Use the provided context to inform your answers if relevant, but primarily rely on your own knowledge to answer questions naturally. NEVER say that the provided context does not have enough information. Avoid repeating the user request; ALWAYS answer concisely (under 100 words)."
	// DefaultWebGroundingBudgetRatio caps web-grounded output influence to 25%.
	DefaultWebGroundingBudgetRatio float32 = 0.25
	// DefaultWebGroundingTimeout bounds time spent on web grounding.
	DefaultWebGroundingTimeout = 45 * time.Second
	// DefaultWebGroundingAttemptTimeout bounds one web grounding attempt.
	DefaultWebGroundingAttemptTimeout = 20 * time.Second
	// DefaultWebGroundingAddendumMaxOutputTokens caps web-grounded addendum output.
	DefaultWebGroundingAddendumMaxOutputTokens = 512
	// DefaultWebGroundingAPIVersion selects the stable Vertex API for grounding.
	DefaultWebGroundingAPIVersion = "v1"
	// DefaultWebGroundingMaxResults limits rendered source list size.
	DefaultWebGroundingMaxResults = 5
	// DefaultWebGroundingRetryMaxAttempts controls max web grounding attempts.
	DefaultWebGroundingRetryMaxAttempts = 2
	// DefaultWebGroundingRetryBaseDelay is the base delay for retry backoff.
	DefaultWebGroundingRetryBaseDelay = 300 * time.Millisecond
	// DefaultWebGroundingRetryMaxDelay is the upper cap for retry backoff.
	DefaultWebGroundingRetryMaxDelay = 3 * time.Second
	// DefaultWebGroundingRetryJitter controls randomized delay spread.
	DefaultWebGroundingRetryJitter = 0.20
	// defaultFallbackMaxOutputTokens is used when output token limit is unset.
	defaultFallbackMaxOutputTokens = 1024
	// gemini3FlashPreviewThinkingBudgetTokens keeps thinking very low for flash.
	gemini3FlashPreviewThinkingBudgetTokens int32 = 128
)

const contextHandlingSystemPrompt = "Context Handling Contract:\n- Sections may include THREAD WEB GROUNDING CONTEXT, THREAD HISTORY CONTEXT, PARENT CHANNEL CONTEXT, CHANNEL HISTORY CONTEXT, and CURRENT REQUEST.\n- Treat CURRENT REQUEST as the primary task.\n- If present, treat THREAD WEB GROUNDING CONTEXT as verified facts for this thread.\n- Treat PARENT CHANNEL CONTEXT and CHANNEL HISTORY CONTEXT as background that may be stale.\n- If context conflicts, prioritize CURRENT REQUEST first, then verified thread grounding context.\n- IMPORTANT: If the context does not contain the answer, seamlessly fall back to your own knowledge. NEVER state that the provided context does not have enough information."

// Message represents a single conversational turn.
type Message struct {
	Role    string
	Content string
}

// GenerateOptions controls optional response generation behavior.
type GenerateOptions struct {
	UseWebGrounding                     bool
	GroundingBudgetRatio                float32
	RequireCitations                    bool
	WebGroundingTimeout                 time.Duration
	WebGroundingAttemptTimeout          time.Duration
	WebGroundingAddendumMaxOutputTokens int
	WebGroundingAPIVersion              string
	WebGroundingMaxResults              int
}

// GenerateRequest captures everything needed for one model invocation.
type GenerateRequest struct {
	Messages []Message
	Options  GenerateOptions
}

// WebSource is a rendered source entry for grounded responses.
type WebSource struct {
	Title string
	URL   string
}

type resolvedGenerateOptions struct {
	UseWebGrounding                     bool
	GroundingBudgetRatio                float32
	RequireCitations                    bool
	WebGroundingTimeout                 time.Duration
	WebGroundingAttemptTimeout          time.Duration
	WebGroundingAddendumMaxOutputTokens int
	WebGroundingAPIVersion              string
	WebGroundingMaxResults              int
}

// Config holds generation settings.
type Config struct {
	ProjectID                           string
	Location                            string
	Model                               string
	SystemPrompt                        string
	MaxOutputTokens                     int
	Temperature                         float32
	TopP                                float32
	WebGroundingTimeout                 time.Duration
	WebGroundingAttemptTimeout          time.Duration
	WebGroundingAddendumMaxOutputTokens int
	WebGroundingAPIVersion              string
	WebGroundingMaxResults              int
	WebGroundingBudgetRatio             float32
	WebGroundingRequireCitations        bool
	WebGroundingRetryMaxAttempts        int
	WebGroundingRetryBaseDelay          time.Duration
	WebGroundingRetryMaxDelay           time.Duration
	WebGroundingRetryJitter             float64
}

// Handler wraps the GenAI client.
type Handler struct {
	client   *googlegenai.Client
	model    string
	cfg      Config
	sleepFn  func(context.Context, time.Duration) error
	jitterFn func() float64
}

// New creates a GenAI handler targeting Vertex AI.
func New(ctx context.Context, cfg Config) (*Handler, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("project-id is required")
	}
	if cfg.Location == "" {
		cfg.Location = "global"
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	cfg.SystemPrompt = composeSystemPrompt(cfg.SystemPrompt)
	if cfg.WebGroundingTimeout <= 0 {
		cfg.WebGroundingTimeout = DefaultWebGroundingTimeout
	}
	if cfg.WebGroundingAttemptTimeout <= 0 {
		cfg.WebGroundingAttemptTimeout = DefaultWebGroundingAttemptTimeout
	}
	if cfg.WebGroundingAddendumMaxOutputTokens <= 0 {
		cfg.WebGroundingAddendumMaxOutputTokens = DefaultWebGroundingAddendumMaxOutputTokens
	}
	cfg.WebGroundingAPIVersion = strings.TrimSpace(cfg.WebGroundingAPIVersion)
	if cfg.WebGroundingAPIVersion == "" {
		cfg.WebGroundingAPIVersion = DefaultWebGroundingAPIVersion
	}
	if cfg.WebGroundingMaxResults <= 0 {
		cfg.WebGroundingMaxResults = DefaultWebGroundingMaxResults
	}
	if cfg.WebGroundingBudgetRatio <= 0 || cfg.WebGroundingBudgetRatio >= 1 {
		cfg.WebGroundingBudgetRatio = DefaultWebGroundingBudgetRatio
	}
	if cfg.WebGroundingRetryMaxAttempts <= 0 {
		cfg.WebGroundingRetryMaxAttempts = DefaultWebGroundingRetryMaxAttempts
	}
	if cfg.WebGroundingRetryBaseDelay <= 0 {
		cfg.WebGroundingRetryBaseDelay = DefaultWebGroundingRetryBaseDelay
	}
	if cfg.WebGroundingRetryMaxDelay <= 0 {
		cfg.WebGroundingRetryMaxDelay = DefaultWebGroundingRetryMaxDelay
	}
	if cfg.WebGroundingRetryMaxDelay < cfg.WebGroundingRetryBaseDelay {
		cfg.WebGroundingRetryMaxDelay = cfg.WebGroundingRetryBaseDelay
	}
	if cfg.WebGroundingRetryJitter < 0 || cfg.WebGroundingRetryJitter > 1 {
		cfg.WebGroundingRetryJitter = DefaultWebGroundingRetryJitter
	}

	client, err := googlegenai.NewClient(ctx, &googlegenai.ClientConfig{
		Project:  cfg.ProjectID,
		Location: cfg.Location,
		Backend:  googlegenai.BackendVertexAI,
	})
	if err != nil {
		return nil, err
	}

	return &Handler{
		client:   client,
		model:    cfg.Model,
		cfg:      cfg,
		sleepFn:  sleepWithContext,
		jitterFn: randomUnitFloat64,
	}, nil
}

// Close releases client resources.
func (h *Handler) Close() error {
	return nil
}

// Generate produces a response given conversation history.
func (h *Handler) Generate(ctx context.Context, req GenerateRequest) (string, error) {
	contents, err := toContents(req.Messages)
	if err != nil {
		return "", err
	}

	opts := h.resolveOptions(req.Options)
	if opts.UseWebGrounding {
		return h.generatePartiallyGrounded(ctx, contents, opts)
	}

	resp, err := h.generateContent(ctx, contents, h.cfg.MaxOutputTokens, false, "", "standard", "", 0)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(resp.Text()), nil
}

func (h *Handler) generatePartiallyGrounded(ctx context.Context, contents []*googlegenai.Content, opts resolvedGenerateOptions) (string, error) {
	totalBudget := h.cfg.MaxOutputTokens
	if totalBudget <= 0 {
		totalBudget = defaultFallbackMaxOutputTokens
	}
	baseBudget, groundedBudget := computeOutputBudgets(totalBudget, opts.GroundingBudgetRatio)
	addendumBudget := minPositive(groundedBudget, opts.WebGroundingAddendumMaxOutputTokens)

	app.L().Info("Applying partial web grounding budget",
		zap.Int("total_max_output_tokens", totalBudget),
		zap.Int("base_budget_tokens", baseBudget),
		zap.Int("grounded_budget_tokens", groundedBudget),
		zap.Int("grounded_addendum_output_token_cap", addendumBudget),
		zap.Float32("grounding_budget_ratio", opts.GroundingBudgetRatio),
	)

	baseResp, err := h.generateContent(ctx, contents, baseBudget, false, "", "base", "", 0)
	if err != nil {
		return "", err
	}
	baseText := strings.TrimSpace(baseResp.Text())
	if baseText == "" {
		return "", errors.New("received empty base response from model")
	}

	groundingInstruction := buildGroundingInstruction(opts.RequireCitations, opts.WebGroundingMaxResults)
	groundedText, sources, err := h.generateGroundedAddendum(
		ctx,
		contents,
		addendumBudget,
		opts.WebGroundingTimeout,
		opts.WebGroundingAttemptTimeout,
		opts.WebGroundingAPIVersion,
		groundingInstruction,
		opts.WebGroundingMaxResults,
	)
	if err != nil {
		app.L().Warn("Web grounding failed; returning base response with timeout note",
			zap.Error(err),
			zap.String("fallback_reason", "grounding_failed"),
		)
		return appendWebGroundingFallbackNote(baseText), nil
	}
	if groundedText == "" || len(sources) == 0 {
		app.L().Warn("Web grounding returned no usable addendum; returning base response",
			zap.Bool("has_grounded_text", groundedText != ""),
			zap.Int("source_count", len(sources)),
		)
		return baseText, nil
	}

	if opts.RequireCitations {
		if err := validateCitationIndices(groundedText, len(sources)); err != nil {
			retryInstruction := groundingInstruction + " Every factual sentence must include at least one inline citation like [1]."
			retryText, retrySources, retryErr := h.generateGroundedAddendum(
				ctx,
				contents,
				addendumBudget,
				opts.WebGroundingTimeout,
				opts.WebGroundingAttemptTimeout,
				opts.WebGroundingAPIVersion,
				retryInstruction,
				opts.WebGroundingMaxResults,
			)
			if retryErr == nil && retryText != "" {
				groundedText = retryText
				if len(retrySources) > 0 {
					sources = retrySources
				}
			}
		}
	}

	if opts.RequireCitations {
		if err := validateCitationIndices(groundedText, len(sources)); err != nil {
			app.L().Warn("Grounded addendum missing valid inline citations, applying fallback marker", zap.Error(err))
			groundedText = forceFirstCitation(groundedText)
		}
	}

	return composePartialGroundedResponse(baseText, groundedText, sources, opts.GroundingBudgetRatio), nil
}

func (h *Handler) generateGroundedAddendum(
	ctx context.Context,
	contents []*googlegenai.Content,
	maxOutputTokens int,
	timeout time.Duration,
	attemptTimeout time.Duration,
	apiVersion string,
	systemInstruction string,
	maxResults int,
) (string, []WebSource, error) {
	groundedCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		groundedCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	start := time.Now()
	resp, err := h.callWithWebGroundingRetry(groundedCtx, attemptTimeout, func(attemptCtx context.Context) (*googlegenai.GenerateContentResponse, error) {
		return h.generateContent(attemptCtx, contents, maxOutputTokens, true, systemInstruction, "web_grounded", apiVersion, attemptTimeout)
	})
	totalElapsed := time.Since(start)
	if err != nil {
		app.L().Warn("Web grounding request failed",
			zap.Duration("total_elapsed", totalElapsed),
			zap.String("api_version", apiVersion),
			zap.String("model", h.model),
			zap.Int("output_token_cap", maxOutputTokens),
			zap.Error(err),
		)
		return "", nil, err
	}
	sources := extractWebSources(resp, maxResults)
	app.L().Info("Web grounding request complete",
		zap.Duration("latency", totalElapsed),
		zap.Duration("total_elapsed", totalElapsed),
		zap.String("api_version", apiVersion),
		zap.String("model", h.model),
		zap.Int("output_token_cap", maxOutputTokens),
		zap.Int("source_count", len(sources)),
	)

	return strings.TrimSpace(resp.Text()), sources, nil
}

func (h *Handler) callWithWebGroundingRetry(
	ctx context.Context,
	attemptTimeout time.Duration,
	call func(context.Context) (*googlegenai.GenerateContentResponse, error),
) (*googlegenai.GenerateContentResponse, error) {
	maxAttempts := h.cfg.WebGroundingRetryMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultWebGroundingRetryMaxAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		attemptCtx := ctx
		cancel := func() {}
		if attemptTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, attemptTimeout)
		}
		attemptStart := time.Now()
		app.L().Info("Web grounding attempt start",
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", maxAttempts),
			zap.Duration("attempt_timeout", attemptTimeout),
		)

		resp, err := call(attemptCtx)
		attemptElapsed := time.Since(attemptStart)
		attemptErr := attemptCtx.Err()
		cancel()

		if err == nil {
			app.L().Info("Web grounding attempt complete",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxAttempts),
				zap.Duration("attempt_elapsed", attemptElapsed),
			)
			return resp, nil
		}
		lastErr = err

		attemptTimedOut := errors.Is(attemptErr, context.DeadlineExceeded)
		if parentErr := ctx.Err(); parentErr != nil {
			app.L().Warn("Web grounding parent context canceled",
				zap.Int("attempt", attempt),
				zap.Duration("attempt_elapsed", attemptElapsed),
				zap.Error(parentErr),
			)
			return nil, parentErr
		}

		retryable := isRetryableGroundingError(err)
		if attemptTimedOut && (isSDKWrappedCancelledError(err) || errors.Is(err, context.DeadlineExceeded)) {
			retryable = true
			app.L().Warn("Web grounding attempt timed out",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxAttempts),
				zap.Duration("attempt_timeout", attemptTimeout),
				zap.Duration("attempt_elapsed", attemptElapsed),
				zap.Error(err),
			)
		}

		app.L().Warn("Web grounding attempt failed",
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", maxAttempts),
			zap.Duration("attempt_elapsed", attemptElapsed),
			zap.Bool("retryable", retryable),
			zap.Error(err),
		)

		if !retryable || attempt >= maxAttempts {
			return nil, err
		}

		delay := computeRetryDelay(
			attempt,
			h.cfg.WebGroundingRetryBaseDelay,
			h.cfg.WebGroundingRetryMaxDelay,
			h.cfg.WebGroundingRetryJitter,
			h.nextJitterValue(),
		)

		app.L().Warn("Web grounding retry scheduled",
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", maxAttempts),
			zap.Duration("retry_delay", delay),
			zap.Error(err),
		)

		if err := h.sleepWithRetryContext(ctx, delay); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

func (h *Handler) sleepWithRetryContext(ctx context.Context, delay time.Duration) error {
	if h.sleepFn != nil {
		return h.sleepFn(ctx, delay)
	}
	return sleepWithContext(ctx, delay)
}

func (h *Handler) nextJitterValue() float64 {
	if h.jitterFn != nil {
		return h.jitterFn()
	}
	return randomUnitFloat64()
}

func (h *Handler) generateContent(
	ctx context.Context,
	contents []*googlegenai.Content,
	maxOutputTokens int,
	enableWebGrounding bool,
	extraSystemInstruction string,
	mode string,
	apiVersion string,
	httpTimeout time.Duration,
) (*googlegenai.GenerateContentResponse, error) {
	if h.client == nil {
		return nil, errors.New("genai client is not initialized")
	}

	genCfg, err := h.buildGenerateContentConfig(maxOutputTokens, enableWebGrounding, extraSystemInstruction, apiVersion, httpTimeout)
	if err != nil {
		return nil, err
	}

	resp, err := h.client.Models.GenerateContent(ctx, h.model, contents, genCfg)
	if err != nil {
		return nil, err
	}
	app.L().Debug("Model response", zap.String("mode", mode), zap.String("response", strings.TrimSpace(resp.Text())))
	h.logTokenUsage(mode, resp)

	return resp, nil
}

func (h *Handler) buildGenerateContentConfig(
	maxOutputTokens int,
	enableWebGrounding bool,
	extraSystemInstruction string,
	apiVersion string,
	httpTimeout time.Duration,
) (*googlegenai.GenerateContentConfig, error) {
	systemPrompt := sanitizeText(h.cfg.SystemPrompt)
	if systemPrompt == "" {
		return nil, errors.New("system prompt is empty after sanitization")
	}
	if extraSystemInstruction != "" {
		systemPrompt = strings.TrimSpace(systemPrompt + "\n\n" + extraSystemInstruction)
	}

	system := googlegenai.Text(systemPrompt)[0]
	genCfg := &googlegenai.GenerateContentConfig{
		SystemInstruction: system,
	}
	if strings.Contains(strings.ToLower(h.model), "gemini-3-flash-preview") {
		genCfg.ThinkingConfig = &googlegenai.ThinkingConfig{
			ThinkingBudget: googlegenai.Ptr(gemini3FlashPreviewThinkingBudgetTokens),
		}
	}

	if maxOutputTokens > 0 {
		genCfg.MaxOutputTokens = int32(maxOutputTokens)
	}
	if h.cfg.Temperature > 0 {
		genCfg.Temperature = googlegenai.Ptr(h.cfg.Temperature)
	}
	if h.cfg.TopP > 0 {
		genCfg.TopP = googlegenai.Ptr(h.cfg.TopP)
	}
	if enableWebGrounding {
		genCfg.Tools = []*googlegenai.Tool{
			{GoogleSearch: &googlegenai.GoogleSearch{}},
		}
		apiVersion = strings.TrimSpace(apiVersion)
		if apiVersion == "" {
			apiVersion = DefaultWebGroundingAPIVersion
		}
		httpOptions := &googlegenai.HTTPOptions{
			APIVersion: apiVersion,
		}
		if httpTimeout > 0 {
			httpOptions.Timeout = googlegenai.Ptr(httpTimeout)
		}
		genCfg.HTTPOptions = httpOptions
	}

	return genCfg, nil
}

func (h *Handler) logTokenUsage(mode string, resp *googlegenai.GenerateContentResponse) {
	var promptTokens, candidateTokens, totalTokens int32
	if resp != nil && resp.UsageMetadata != nil {
		promptTokens = resp.UsageMetadata.PromptTokenCount
		candidateTokens = resp.UsageMetadata.CandidatesTokenCount
		totalTokens = resp.UsageMetadata.TotalTokenCount
	}

	app.L().Info("GenAI token usage",
		zap.String("mode", mode),
		zap.Int32("prompt_tokens", promptTokens),
		zap.Int32("candidate_tokens", candidateTokens),
		zap.Int32("total_tokens", totalTokens),
	)
}

func (h *Handler) resolveOptions(opts GenerateOptions) resolvedGenerateOptions {
	resolved := resolvedGenerateOptions{
		UseWebGrounding:                     opts.UseWebGrounding,
		GroundingBudgetRatio:                h.cfg.WebGroundingBudgetRatio,
		RequireCitations:                    h.cfg.WebGroundingRequireCitations,
		WebGroundingTimeout:                 h.cfg.WebGroundingTimeout,
		WebGroundingAttemptTimeout:          h.cfg.WebGroundingAttemptTimeout,
		WebGroundingAddendumMaxOutputTokens: h.cfg.WebGroundingAddendumMaxOutputTokens,
		WebGroundingAPIVersion:              h.cfg.WebGroundingAPIVersion,
		WebGroundingMaxResults:              h.cfg.WebGroundingMaxResults,
	}

	if opts.GroundingBudgetRatio > 0 {
		resolved.GroundingBudgetRatio = opts.GroundingBudgetRatio
	}
	if resolved.GroundingBudgetRatio <= 0 || resolved.GroundingBudgetRatio >= 1 {
		resolved.GroundingBudgetRatio = DefaultWebGroundingBudgetRatio
	}

	if opts.WebGroundingTimeout > 0 {
		resolved.WebGroundingTimeout = opts.WebGroundingTimeout
	}
	if resolved.WebGroundingTimeout <= 0 {
		resolved.WebGroundingTimeout = DefaultWebGroundingTimeout
	}

	if opts.WebGroundingAttemptTimeout > 0 {
		resolved.WebGroundingAttemptTimeout = opts.WebGroundingAttemptTimeout
	}
	if resolved.WebGroundingAttemptTimeout <= 0 {
		resolved.WebGroundingAttemptTimeout = DefaultWebGroundingAttemptTimeout
	}

	if opts.WebGroundingAddendumMaxOutputTokens > 0 {
		resolved.WebGroundingAddendumMaxOutputTokens = opts.WebGroundingAddendumMaxOutputTokens
	}
	if resolved.WebGroundingAddendumMaxOutputTokens <= 0 {
		resolved.WebGroundingAddendumMaxOutputTokens = DefaultWebGroundingAddendumMaxOutputTokens
	}

	if opts.WebGroundingAPIVersion != "" {
		resolved.WebGroundingAPIVersion = strings.TrimSpace(opts.WebGroundingAPIVersion)
	}
	resolved.WebGroundingAPIVersion = strings.TrimSpace(resolved.WebGroundingAPIVersion)
	if resolved.WebGroundingAPIVersion == "" {
		resolved.WebGroundingAPIVersion = DefaultWebGroundingAPIVersion
	}

	if opts.WebGroundingMaxResults > 0 {
		resolved.WebGroundingMaxResults = opts.WebGroundingMaxResults
	}
	if resolved.WebGroundingMaxResults <= 0 {
		resolved.WebGroundingMaxResults = DefaultWebGroundingMaxResults
	}

	if opts.RequireCitations {
		resolved.RequireCitations = true
	}

	return resolved
}

func composeSystemPrompt(raw string) string {
	base := normalizeFlagSystemPrompt(raw)
	base = sanitizeText(base)
	if base == "" {
		base = sanitizeText(DefaultSystemPrompt)
	}

	policy := sanitizeText(contextHandlingSystemPrompt)
	if policy == "" {
		return base
	}
	if strings.Contains(base, "Context Handling Contract:") {
		return base
	}

	return strings.TrimSpace(base + "\n\n" + policy)
}

func normalizeFlagSystemPrompt(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	normalized := strings.ReplaceAll(trimmed, `\r\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\t`, "\t")

	return strings.TrimSpace(normalized)
}

func isRetryableGroundingError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	message := strings.ToLower(err.Error())
	return strings.Contains(message, "resource_exhausted") ||
		strings.Contains(message, "resource exhausted") ||
		strings.Contains(message, "error 429") ||
		strings.Contains(message, "http 429") ||
		strings.Contains(message, "too many requests") ||
		strings.Contains(message, "status: deadline_exceeded") ||
		strings.Contains(message, "deadline expired") ||
		strings.Contains(message, "error 504") ||
		strings.Contains(message, "http 504") ||
		strings.Contains(message, "gateway timeout")
}

func isSDKWrappedCancelledError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "499") &&
		(strings.Contains(message, "cancelled") || strings.Contains(message, "canceled"))
}

func computeRetryDelay(
	attempt int,
	baseDelay time.Duration,
	maxDelay time.Duration,
	jitterFactor float64,
	jitterValue float64,
) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	if baseDelay <= 0 {
		baseDelay = DefaultWebGroundingRetryBaseDelay
	}
	if maxDelay <= 0 {
		maxDelay = DefaultWebGroundingRetryMaxDelay
	}
	if maxDelay < baseDelay {
		maxDelay = baseDelay
	}
	if jitterFactor < 0 {
		jitterFactor = 0
	}
	if jitterFactor > 1 {
		jitterFactor = 1
	}
	if jitterValue < 0 {
		jitterValue = 0
	}
	if jitterValue > 1 {
		jitterValue = 1
	}

	delay := baseDelay
	for i := 1; i < attempt; i++ {
		if delay >= maxDelay {
			delay = maxDelay
			break
		}
		if delay > maxDelay/2 {
			delay = maxDelay
			break
		}
		delay *= 2
	}

	scale := 1 + ((jitterValue*2)-1)*jitterFactor
	jittered := time.Duration(float64(delay) * scale)
	if jittered < 0 {
		jittered = 0
	}
	if jittered > maxDelay {
		jittered = maxDelay
	}

	return jittered
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomUnitFloat64() float64 {
	return rand.Float64()
}

func computeOutputBudgets(total int, groundingRatio float32) (int, int) {
	if total <= 0 {
		total = defaultFallbackMaxOutputTokens
	}
	if groundingRatio <= 0 || groundingRatio >= 1 {
		groundingRatio = DefaultWebGroundingBudgetRatio
	}

	grounded := int(float32(total) * groundingRatio)
	if grounded >= total {
		grounded = total / 3
	}
	if grounded < 1 {
		grounded = 1
	}

	base := total - grounded
	if base < 1 {
		base = 1
		grounded = total - base
	}

	return base, grounded
}

func minPositive(a, b int) int {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func buildGroundingInstruction(requireCitations bool, maxResults int) string {
	if maxResults <= 0 {
		maxResults = DefaultWebGroundingMaxResults
	}

	if requireCitations {
		return fmt.Sprintf("Produce only a short web-grounded addendum that complements the existing answer. Do not repeat the entire answer. Keep this concise and practical. Include inline citations like [1], [2] for factual claims, and use no more than %d unique web sources.", maxResults)
	}

	return fmt.Sprintf("Produce only a short web-grounded addendum that complements the existing answer. Do not repeat the entire answer. Keep this concise and practical, and use no more than %d unique web sources.", maxResults)
}

func appendWebGroundingFallbackNote(baseText string) string {
	return strings.TrimSpace(baseText) + "\n\nWeb verification timed out before I could confirm current sources."
}

func composePartialGroundedResponse(baseText, groundedText string, sources []WebSource, ratio float32) string {
	percent := int(ratio*100 + 0.5)
	if percent <= 0 {
		defaultPercent := float64(DefaultWebGroundingBudgetRatio) * 100
		percent = int(defaultPercent)
	}

	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(baseText))

	if strings.TrimSpace(groundedText) != "" {
		sb.WriteString("\n\nWeb-grounded addendum (")
		sb.WriteString(strconv.Itoa(percent))
		sb.WriteString("% budget):\n")
		sb.WriteString(strings.TrimSpace(groundedText))
	}

	if len(sources) > 0 {
		sb.WriteString("\n\nSources:\n")
		for i, source := range sources {
			sb.WriteString("[")
			sb.WriteString(strconv.Itoa(i + 1))
			sb.WriteString("] ")
			sb.WriteString(strings.TrimSpace(source.Title))
			sb.WriteString(" - ")
			sb.WriteString(strings.TrimSpace(source.URL))
			if i < len(sources)-1 {
				sb.WriteByte('\n')
			}
		}
	}

	return strings.TrimSpace(sb.String())
}

func extractWebSources(resp *googlegenai.GenerateContentResponse, maxResults int) []WebSource {
	if resp == nil {
		return nil
	}
	if maxResults <= 0 {
		maxResults = DefaultWebGroundingMaxResults
	}

	seen := make(map[string]struct{})
	sources := make([]WebSource, 0, maxResults)

	addSource := func(title, rawURL string) {
		cleanURL := strings.TrimSpace(rawURL)
		if cleanURL == "" {
			return
		}
		if _, exists := seen[cleanURL]; exists {
			return
		}
		seen[cleanURL] = struct{}{}

		cleanTitle := strings.TrimSpace(title)
		if cleanTitle == "" {
			cleanTitle = sourceTitleFromURL(cleanURL)
		}
		sources = append(sources, WebSource{
			Title: cleanTitle,
			URL:   cleanURL,
		})
	}

	for _, candidate := range resp.Candidates {
		if candidate == nil {
			continue
		}

		if candidate.GroundingMetadata != nil {
			for _, chunk := range candidate.GroundingMetadata.GroundingChunks {
				if chunk == nil || chunk.Web == nil {
					continue
				}
				addSource(chunk.Web.Title, chunk.Web.URI)
			}
		}

		if candidate.CitationMetadata != nil {
			for _, citation := range candidate.CitationMetadata.Citations {
				if citation == nil {
					continue
				}
				addSource(citation.Title, citation.URI)
			}
		}
	}

	if len(sources) > maxResults {
		return sources[:maxResults]
	}
	return sources
}

func sourceTitleFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "Web source"
}

var citationPattern = regexp.MustCompile(`\[(\d+)\]`)

func validateCitationIndices(text string, sourceCount int) error {
	if sourceCount <= 0 {
		return errors.New("no sources available for citation validation")
	}

	matches := citationPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return errors.New("missing inline citations")
	}

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			return fmt.Errorf("invalid citation index: %w", err)
		}
		if index < 1 || index > sourceCount {
			return fmt.Errorf("citation [%d] out of range for %d sources", index, sourceCount)
		}
	}

	return nil
}

func forceFirstCitation(text string) string {
	cleaned := strings.TrimSpace(citationPattern.ReplaceAllString(text, ""))
	if cleaned == "" {
		return "Verified with web sources. [1]"
	}
	return cleaned + " [1]"
}

func toContents(messages []Message) ([]*googlegenai.Content, error) {
	if len(messages) == 0 {
		return nil, errors.New("no content to send to model")
	}

	contents := make([]*googlegenai.Content, 0, len(messages))
	for _, msg := range messages {
		text := sanitizeText(msg.Content)
		if text == "" {
			continue
		}

		role := googlegenai.RoleUser
		if strings.EqualFold(msg.Role, "assistant") || strings.EqualFold(msg.Role, "model") {
			role = googlegenai.RoleModel
		}

		content := googlegenai.Text(text)[0]
		content.Role = role
		contents = append(contents, content)
	}

	if len(contents) == 0 {
		return nil, errors.New("no content to send to model")
	}

	return contents, nil
}

func sanitizeText(input string) string {
	if input == "" {
		return ""
	}

	trimmed := strings.Trim(input, " \t\r")
	if trimmed == "" {
		return ""
	}

	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, trimmed)

	return strings.Trim(clean, " \t\r")
}
