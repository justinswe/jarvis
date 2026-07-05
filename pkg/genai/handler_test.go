package genai

import (
	"context"
	"testing"

	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegenai "google.golang.org/genai"
)

func response(text string, metadata *googlegenai.GroundingMetadata) *googlegenai.GenerateContentResponse {
	return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{Content: &googlegenai.Content{Parts: []*googlegenai.Part{{Text: text}}}, GroundingMetadata: metadata}}}
}

func TestContentConfig(t *testing.T) {
	h := &Handler{cfg: Config{MaxOutputTokens: 256, Temperature: .5}, systemPrompt: composeSystemPrompt("system")}
	cfg := h.contentConfig(true)
	require.Len(t, cfg.Tools, 1)
	assert.NotNil(t, cfg.Tools[0].GoogleSearch)
	assert.Equal(t, googlegenai.ThinkingLevelMinimal, cfg.ThinkingConfig.ThinkingLevel)
	assert.Equal(t, int32(256), cfg.MaxOutputTokens)
	assert.Empty(t, h.contentConfig(false).Tools)
}

func TestNewRejectsInvalidConfigurationBeforeClientCreation(t *testing.T) {
	_, err := New(context.Background(), Config{ProjectID: "p", Model: "gemini-2.5-flash"})
	assert.ErrorContains(t, err, "incompatible")
	_, err = New(context.Background(), Config{ProjectID: "p", MaxOutputTokens: 513})
	assert.ErrorContains(t, err, "between 1 and 512")
}

func TestGenerateGroundingSources(t *testing.T) {
	h := &Handler{model: DefaultModel, cfg: Config{MaxOutputTokens: 256}, systemPrompt: composeSystemPrompt("s")}
	h.generate = func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		require.Len(t, cfg.Tools, 1)
		return response("answer", &googlegenai.GroundingMetadata{GroundingChunks: []*googlegenai.GroundingChunk{
			{Web: &googlegenai.GroundingChunkWeb{Title: "One", URI: "https://one.example"}},
			{Web: &googlegenai.GroundingChunkWeb{Title: "duplicate", URI: "https://one.example"}},
			{Web: &googlegenai.GroundingChunkWeb{Title: "Two", URI: "https://two.example"}},
			{Web: &googlegenai.GroundingChunkWeb{Title: "Three", URI: "https://three.example"}},
			{Web: &googlegenai.GroundingChunkWeb{Title: "Four", URI: "https://four.example"}},
		}}), nil
	}
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "current news"}}})
	require.NoError(t, err)
	assert.True(t, got.Grounded)
	assert.Len(t, got.Sources, 3)
}

func TestGenerateStableResponseHasNoSources(t *testing.T) {
	h := &Handler{model: DefaultModel, cfg: Config{MaxOutputTokens: 256}, systemPrompt: composeSystemPrompt("s"), generate: func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		return response("stable", nil), nil
	}}
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "hello"}}})
	require.NoError(t, err)
	assert.Equal(t, "stable", got.Text)
	assert.False(t, got.Grounded)
	assert.Empty(t, got.Sources)
}

func TestGenerateMissingSourcesAddsCaveat(t *testing.T) {
	h := &Handler{model: DefaultModel, cfg: Config{MaxOutputTokens: 256}, systemPrompt: composeSystemPrompt("s"), generate: func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		return response("answer", &googlegenai.GroundingMetadata{}), nil
	}}
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "search"}}})
	require.NoError(t, err)
	assert.Contains(t, got.Text, verificationCaveat)
}

func TestGenerateSearchFailureFallsBackOnce(t *testing.T) {
	calls := 0
	h := &Handler{model: DefaultModel, cfg: Config{MaxOutputTokens: 256}, systemPrompt: composeSystemPrompt("s")}
	h.generate = func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if len(cfg.Tools) > 0 {
			return nil, errors.New("search unavailable")
		}
		return response("fallback", nil), nil
	}
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "question"}}})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Contains(t, got.Text, verificationCaveat)
}

func TestSanitizeText(t *testing.T) { assert.Equal(t, "hi\nthere", sanitizeText(" hi\x07\nthere ")) }

func TestComposeSystemPrompt(t *testing.T) {
	t.Run("uses configurable prompt with base behavior", func(t *testing.T) {
		got := composeSystemPrompt("You are an assistant named Friday.")
		assert.Contains(t, got, BaseSystemPrompt)
		assert.Contains(t, got, "named Friday")
		assert.NotContains(t, BaseSystemPrompt, "Jarvis")
	})

	t.Run("uses default identity", func(t *testing.T) {
		got := composeSystemPrompt("")
		assert.Equal(t, BaseSystemPrompt+"\n\n"+DefaultPrompt, got)
	})
}

func TestGroundingUsage(t *testing.T) {
	resp := response("answer", &googlegenai.GroundingMetadata{
		WebSearchQueries: []string{"first query", "second query"},
	})
	used, queryCount := groundingUsage(resp)
	assert.True(t, used)
	assert.Equal(t, 2, queryCount)
}
