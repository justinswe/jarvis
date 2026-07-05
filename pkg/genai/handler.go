package genai

import (
	"context"
	"net/url"
	"strings"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	googlegenai "google.golang.org/genai"
)

const (
	DefaultModel           = "google/gemini-3.5-flash"
	DefaultMaxOutputTokens = 256
	MaxOutputTokensLimit   = 512
	verificationCaveat     = "I couldn't verify this with usable web sources, so please confirm time-sensitive details."
)

const (
	BaseSystemPrompt = "Messages are formatted as \"Name: text\". Do not include your name or any prefixes in responses. Do not emit HTML entities; output raw punctuation. Always answer concisely in under 100 words. Treat CURRENT REQUEST as the primary task, then THREAD HISTORY, then PARENT CHANNEL or CHANNEL HISTORY. Background context may be stale. If context is insufficient, answer from your own knowledge without mentioning that context is missing. Google Search is available: use it only when the user explicitly asks you to search, when current information is needed, or when you cannot answer a factual question confidently. Use the minimum necessary search queries and do not repeat the question or conversation history in queries."
	DefaultPrompt    = "You are a helpful assistant named Jarvis."
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
}

type Source struct {
	Title string
	URL   string
}

type GenerateResponse struct {
	Text     string
	Grounded bool
	Sources  []Source
}

type Config struct {
	ProjectID       string
	Location        string
	Model           string
	DefaultPrompt   string
	MaxOutputTokens int
	Temperature     float32
}

type generateFunc func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error)

type Handler struct {
	client       *googlegenai.Client
	model        string
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
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if !strings.Contains(strings.ToLower(cfg.Model), "gemini-3.5-flash") {
		return nil, errors.Errorf("model %q is incompatible with required MINIMAL thinking; use %s", cfg.Model, DefaultModel)
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
	h := &Handler{client: client, model: cfg.Model, cfg: cfg, systemPrompt: systemPrompt}
	h.generate = client.Models.GenerateContent
	return h, nil
}

func (h *Handler) Close() error { return nil }

func (h *Handler) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	app.L().Info("Starting Gemini generation",
		zap.String("model", h.model),
		zap.String("request_id", req.RequestID),
		zap.String("caller_id", req.CallerID),
		zap.String("channel_id", req.ChannelID),
		zap.Int("message_count", len(req.Messages)),
		zap.Int("max_output_tokens", h.cfg.MaxOutputTokens),
		zap.Bool("google_search_available", true),
	)
	contents, err := toContents(req.Messages)
	if err != nil {
		return GenerateResponse{}, err
	}

	resp, err := h.generate(ctx, h.model, contents, h.contentConfig(true))
	if err != nil {
		app.L().Warn("Search-enabled generation failed; retrying without tools", zap.Error(err))
		fallback, fallbackErr := h.generate(ctx, h.model, contents, h.contentConfig(false))
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

	sources := extractSources(resp, 3)
	h.logTokenUsage(resp, req, false, len(sources))
	grounded := len(sources) > 0
	text := strings.TrimSpace(resp.Text())
	if searchWasUsed(resp) && !grounded && text != "" {
		text += "\n\n" + verificationCaveat
	}
	return GenerateResponse{Text: text, Grounded: grounded, Sources: sources}, nil
}

func (h *Handler) contentConfig(search bool) *googlegenai.GenerateContentConfig {
	cfg := &googlegenai.GenerateContentConfig{
		SystemInstruction: &googlegenai.Content{Parts: []*googlegenai.Part{{Text: h.systemPrompt}}},
		MaxOutputTokens:   int32(h.cfg.MaxOutputTokens),
		Temperature:       &h.cfg.Temperature,
		ThinkingConfig:    &googlegenai.ThinkingConfig{ThinkingLevel: googlegenai.ThinkingLevelMinimal},
	}
	if search {
		cfg.Tools = []*googlegenai.Tool{{GoogleSearch: &googlegenai.GoogleSearch{}}}
	}
	return cfg
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
		zap.String("model", h.model),
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
