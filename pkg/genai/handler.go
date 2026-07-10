package genai

import (
	"context"
	"net/url"
	"reflect"
	"strings"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	googlegenai "google.golang.org/genai"
)

const (
	DefaultMaxOutputTokens   = 512
	MaxOutputTokensLimit     = 512
	selectedModel            = "google/gemini-3.5-flash"
	verificationCaveat       = "I couldn't verify this with web sources."
	maxToolRounds            = 2
	maxFunctionCallsPerRound = 2
)

const (
	BaseSystemPrompt = "Messages are formatted as \"Name: text\". Do not include your name or any prefixes in responses. Do not emit HTML entities; output raw punctuation. Always answer concisely in under 100 words. Treat CURRENT REQUEST as the primary task, then THREAD HISTORY, then PARENT CHANNEL or CHANNEL HISTORY. Background context may be stale. If context is insufficient, answer from your own knowledge without mentioning that context is missing. A current-channel search tool may be available: use it when the user asks about earlier messages in this Discord channel. Google Search is available: use it only when the user explicitly asks you to search the web, when current public information is needed, or when you cannot answer a factual question confidently. Use the minimum necessary search queries and do not repeat the question or conversation history in queries."
	DefaultPrompt    = "You are a intelligent, witty, and clever assistant named Jarvis."
)

type Message struct {
	Role    string
	Content string
}

type GenerateRequest struct {
	Messages  []Message
	RequestID string
	CallerID  string
	ChannelID string
	Tools     []FunctionTool
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
	client       *googlegenai.Client
	cfg          Config
	systemPrompt string
	generate     generateFunc
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
	systemPrompt := composeSystemPrompt(cfg.DefaultPrompt)

	client, err := googlegenai.NewClient(ctx, &googlegenai.ClientConfig{
		Project: cfg.ProjectID, Location: cfg.Location, Backend: googlegenai.BackendVertexAI,
	})
	if err != nil {
		return nil, err
	}
	h := &Handler{client: client, cfg: cfg, systemPrompt: systemPrompt}
	h.generate = client.Models.GenerateContent
	return h, nil
}

func (h *Handler) Close() error { return nil }

func (h *Handler) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	app.L().Info("Starting Gemini generation",
		zap.String("model", selectedModel),
		zap.String("request_id", req.RequestID),
		zap.String("caller_id", req.CallerID),
		zap.String("channel_id", req.ChannelID),
		zap.Int("message_count", len(req.Messages)),
		zap.Int("max_output_tokens", h.cfg.MaxOutputTokens),
		zap.Bool("google_search_available", true),
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
	resp, err := h.generate(ctx, selectedModel, contents, h.contentConfig(true, registry.declarations()))
	if err != nil {
		app.L().Warn("Search-enabled generation failed; retrying without tools", zap.Error(err))
		fallback, fallbackErr := h.generate(ctx, selectedModel, contents, h.contentConfig(false, nil))
		if fallbackErr != nil {
			return GenerateResponse{}, errors.Wrapf(fallbackErr, "generate fallback after search-enabled request failed: %v", err)
		}
		h.logTokenUsage(fallback, req, true, 0)
		text := strings.TrimSpace(fallback.Text())
		if text != "" {
			text += "\n\n" + verificationCaveat
		}
		return GenerateResponse{Text: text}, nil
	}
	if len(req.Tools) > 0 {
		resp, err = h.resolveFunctionCalls(ctx, req, registry, contents, resp)
		if err != nil {
			return GenerateResponse{}, err
		}
	}

	sources := extractSources(resp, 3)
	h.logTokenUsage(resp, req, false, len(sources))
	grounded := len(sources) > 0
	text := strings.TrimSpace(resp.Text())
	if searchWasUsed(resp) && !grounded && text != "" {
		text += "\n\n" + verificationCaveat
	}
	return GenerateResponse{Text: text, Grounded: grounded, Sources: sources}, nil
}

func (h *Handler) contentConfig(search bool, declarations []*googlegenai.FunctionDeclaration) *googlegenai.GenerateContentConfig {
	cfg := &googlegenai.GenerateContentConfig{
		SystemInstruction: &googlegenai.Content{Parts: []*googlegenai.Part{{Text: h.systemPrompt}}},
		MaxOutputTokens:   int32(h.cfg.MaxOutputTokens),
		Temperature:       &h.cfg.Temperature,
		ThinkingConfig:    &googlegenai.ThinkingConfig{ThinkingLevel: googlegenai.ThinkingLevelMinimal},
	}
	if search {
		cfg.Tools = []*googlegenai.Tool{{GoogleSearch: &googlegenai.GoogleSearch{}}}
	}
	if len(declarations) > 0 {
		cfg.Tools = append(cfg.Tools, &googlegenai.Tool{FunctionDeclarations: declarations})
	}
	return cfg
}

func (h *Handler) resolveFunctionCalls(ctx context.Context, req GenerateRequest, registry toolRegistry, contents []*googlegenai.Content, resp *googlegenai.GenerateContentResponse) (*googlegenai.GenerateContentResponse, error) {
	for round := 0; round < maxToolRounds; round++ {
		calls := resp.FunctionCalls()
		if len(calls) == 0 {
			return resp, nil
		}
		contents = append(contents, modelToolCallContent(resp), functionResponseContent(ctx, registry, calls))
		if round == maxToolRounds-1 {
			break
		}
		var err error
		resp, err = h.generate(ctx, selectedModel, contents, h.contentConfig(true, registry.declarations()))
		if err != nil {
			return nil, errors.Wrap(err, "generate after channel search tool response")
		}
	}
	app.L().Warn("Gemini exceeded channel search tool round limit",
		zap.String("request_id", req.RequestID),
		zap.String("channel_id", req.ChannelID),
	)
	final, err := h.generate(ctx, selectedModel, contents, h.contentConfig(true, nil))
	if err != nil {
		return nil, errors.Wrap(err, "generate final response after channel search tool limit")
	}
	return final, nil
}

func modelToolCallContent(resp *googlegenai.GenerateContentResponse) *googlegenai.Content {
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return &googlegenai.Content{Role: "model"}
	}
	return resp.Candidates[0].Content
}

func functionResponseContent(ctx context.Context, registry toolRegistry, calls []*googlegenai.FunctionCall) *googlegenai.Content {
	parts := make([]*googlegenai.Part, 0, len(calls))
	for i, call := range calls {
		parts = append(parts, &googlegenai.Part{FunctionResponse: &googlegenai.FunctionResponse{
			ID:       functionCallID(call),
			Name:     functionCallName(call),
			Response: registry.call(ctx, call, i),
		}})
	}
	return &googlegenai.Content{Role: "user", Parts: parts}
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

func (r toolRegistry) call(ctx context.Context, call *googlegenai.FunctionCall, index int) map[string]any {
	if call == nil {
		return map[string]any{"error": "missing function call"}
	}
	if index >= maxFunctionCallsPerRound {
		return map[string]any{"error": "function call limit exceeded"}
	}
	tool, ok := r.tools[call.Name]
	if !ok {
		return map[string]any{"error": "unsupported function " + call.Name}
	}
	output, err := tool.Execute(ctx, call.Args)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"output": output}
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

func composeSystemPrompt(prompt string) string {
	prompt = strings.TrimSpace(strings.ReplaceAll(prompt, `\n`, "\n"))
	if prompt == "" {
		prompt = DefaultPrompt
	}
	return BaseSystemPrompt + "\n\n" + prompt
}

func (h *Handler) logTokenUsage(resp *googlegenai.GenerateContentResponse, req GenerateRequest, fallback bool, sourceCount int) {
	if resp == nil || resp.UsageMetadata == nil {
		return
	}
	u := resp.UsageMetadata
	searchUsed, queryCount := groundingUsage(resp)
	app.L().Info("Gemini token usage",
		zap.String("model", selectedModel),
		zap.String("request_id", req.RequestID),
		zap.String("caller_id", req.CallerID),
		zap.String("channel_id", req.ChannelID),
		zap.Bool("search_used", searchUsed),
		zap.Bool("grounded", searchUsed && sourceCount > 0),
		zap.Int("grounding_source_count", sourceCount),
		zap.Int("search_query_count", queryCount),
		zap.Bool("tool_disabled_fallback", fallback),
		zap.Int32("prompt_tokens", u.PromptTokenCount),
		zap.Int32("candidate_tokens", u.CandidatesTokenCount),
		zap.Int32("thought_tokens", u.ThoughtsTokenCount),
		zap.Int32("tool_use_tokens", u.ToolUsePromptTokenCount),
		zap.Int32("total_tokens", u.TotalTokenCount),
	)
}

func groundingUsage(resp *googlegenai.GenerateContentResponse) (bool, int) {
	used := false
	queries := 0
	for _, candidate := range resp.Candidates {
		if candidate == nil || candidate.GroundingMetadata == nil {
			continue
		}
		used = true
		queries += len(candidate.GroundingMetadata.WebSearchQueries)
	}
	return used, queries
}

func searchWasUsed(resp *googlegenai.GenerateContentResponse) bool {
	for _, candidate := range resp.Candidates {
		if candidate != nil && candidate.GroundingMetadata != nil {
			return true
		}
	}
	return false
}

func extractSources(resp *googlegenai.GenerateContentResponse, limit int) []Source {
	if resp == nil || limit <= 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var sources []Source
	for _, candidate := range resp.Candidates {
		if candidate == nil || candidate.GroundingMetadata == nil {
			continue
		}
		for _, chunk := range candidate.GroundingMetadata.GroundingChunks {
			if chunk == nil || chunk.Web == nil {
				continue
			}
			rawURL := strings.TrimSpace(chunk.Web.URI)
			if _, err := url.ParseRequestURI(rawURL); rawURL == "" || err != nil {
				continue
			}
			if _, ok := seen[rawURL]; ok {
				continue
			}
			seen[rawURL] = struct{}{}
			title := strings.TrimSpace(chunk.Web.Title)
			if title == "" {
				if parsed, err := url.Parse(rawURL); err == nil {
					title = parsed.Hostname()
				}
			}
			if title == "" {
				continue
			}
			sources = append(sources, Source{Title: title, URL: rawURL})
			if len(sources) == limit {
				return sources
			}
		}
	}
	return sources
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
		if text == "" {
			continue
		}
		contents = append(contents, &googlegenai.Content{Role: role, Parts: []*googlegenai.Part{{Text: text}}})
	}
	if len(contents) == 0 {
		return nil, errors.New("messages contain no text")
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
