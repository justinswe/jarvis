package llm

import (
	"context"

	"github.com/justinswe/std/errors"
	googlegenai "google.golang.org/genai"
)

// EmbeddingDimension is the fixed vector width. It matches the store's vector(768)
// column: changing it means a schema migration and a full re-embed, so it is a
// constant, not a knob.
const EmbeddingDimension = 768

// Embedder produces fixed-dimension embedding vectors on a named model. Implemented by
// the Google hosts only; other providers can be added when a deployment without Google
// credentials needs memory retrieval.
type Embedder interface {
	Embed(ctx context.Context, modelID string, texts []string) ([][]float32, error)
}

// BoundEmbedder pairs an embedding-capable host with one model ID.
type BoundEmbedder struct {
	Host    Embedder
	ModelID string
}

// Embed embeds texts on the bound model.
func (b *BoundEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return b.Host.Embed(ctx, b.ModelID, texts)
}

// VertexEmbedFunc is the Google SDK embedding boundary used by the adapter.
type VertexEmbedFunc func(context.Context, string, []*googlegenai.Content, *googlegenai.EmbedContentConfig) (*googlegenai.EmbedContentResponse, error)

// Embed implements Embedder over the Gemini embedding API for both Google backends.
func (h *googleGenAIHost) Embed(ctx context.Context, modelID string, texts []string) ([][]float32, error) {
	if h.embed == nil {
		return nil, &Error{Kind: ErrorInvalidRequest, Provider: h.provider, Err: errors.New("host has no embedding support")}
	}
	if len(texts) == 0 {
		return nil, nil
	}
	contents := make([]*googlegenai.Content, len(texts))
	for i, text := range texts {
		contents[i] = googlegenai.NewContentFromText(text, googlegenai.RoleUser)
	}
	dimension := int32(EmbeddingDimension)
	response, err := h.embed(ctx, modelID, contents, &googlegenai.EmbedContentConfig{
		OutputDimensionality: &dimension,
	})
	if err != nil {
		return nil, classifyGoogleGenAIError(h.provider, err)
	}
	if len(response.Embeddings) != len(texts) {
		return nil, &Error{Kind: ErrorMalformed, Provider: h.provider,
			Err: errors.Errorf("embedding count %d does not match input count %d", len(response.Embeddings), len(texts))}
	}
	vectors := make([][]float32, len(texts))
	for i, embedding := range response.Embeddings {
		if embedding == nil || len(embedding.Values) == 0 {
			return nil, &Error{Kind: ErrorMalformed, Provider: h.provider, Err: errors.New("embedding response has an empty vector")}
		}
		vectors[i] = embedding.Values
	}
	return vectors, nil
}
