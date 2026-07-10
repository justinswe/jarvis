package genai

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegenai "google.golang.org/genai"
)

type fakeTool struct {
	name string
	decl *googlegenai.FunctionDeclaration
	exec func(context.Context, map[string]any) (any, error)
}

func (t fakeTool) Name() string                                  { return t.name }
func (t fakeTool) Declaration() *googlegenai.FunctionDeclaration { return t.decl }
func (t fakeTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	return t.exec(ctx, args)
}

func testTool(name string, exec func(context.Context, map[string]any) (any, error)) fakeTool {
	return fakeTool{name: name, decl: &googlegenai.FunctionDeclaration{Name: name}, exec: exec}
}

func response(text string, metadata *googlegenai.GroundingMetadata) *googlegenai.GenerateContentResponse {
	return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{Content: &googlegenai.Content{Parts: []*googlegenai.Part{{Text: text}}}, GroundingMetadata: metadata}}}
}

func toolResponse(calls ...*googlegenai.FunctionCall) *googlegenai.GenerateContentResponse {
	parts := make([]*googlegenai.Part, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, googlegenai.NewPartFromFunctionCall(call.Name, call.Args))
		parts[len(parts)-1].FunctionCall.ID = call.ID
	}
	return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{Content: &googlegenai.Content{Role: "model", Parts: parts}}}}
}

func testHandler(generate generateFunc) *Handler {
	return &Handler{cfg: Config{MaxOutputTokens: 256}, systemPrompt: composeSystemPrompt("s"), generate: generate}
}

func TestContentConfig(t *testing.T) {
	h := testHandler(nil)
	cfg := h.contentConfig(true, []*googlegenai.FunctionDeclaration{{Name: "tool"}}, googlegenai.FunctionCallingConfigModeAuto)
	require.Len(t, cfg.Tools, 2)
	assert.NotNil(t, cfg.Tools[0].GoogleSearch)
	assert.Equal(t, "tool", cfg.Tools[1].FunctionDeclarations[0].Name)
	assert.Equal(t, googlegenai.ThinkingLevelMinimal, cfg.ThinkingConfig.ThinkingLevel)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAuto, cfg.ToolConfig.FunctionCallingConfig.Mode)
	disabled := h.contentConfig(false, nil, googlegenai.FunctionCallingConfigModeNone)
	assert.Empty(t, disabled.Tools)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, disabled.ToolConfig.FunctionCallingConfig.Mode)
}

func TestNewRejectsInvalidConfigurationBeforeClientCreation(t *testing.T) {
	_, err := New(context.Background(), Config{ProjectID: "p", MaxOutputTokens: 513})
	assert.ErrorContains(t, err, "between 1 and 512")
}

func TestGenerateGroundingSources(t *testing.T) {
	h := testHandler(func(_ context.Context, model string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		assert.Equal(t, selectedModel, model)
		require.Len(t, cfg.Tools, 1)
		return response("answer", &googlegenai.GroundingMetadata{GroundingChunks: []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{Title: "One", URI: "https://one.example"}}}}), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "current news"}}})
	require.NoError(t, err)
	assert.True(t, got.Grounded)
	assert.Len(t, got.Sources, 1)
}

func TestToolRegistryRejectsInvalidTools(t *testing.T) {
	valid := testTool("a", func(context.Context, map[string]any) (any, error) { return nil, nil })
	for _, tools := range [][]FunctionTool{{nil}, {fakeTool{name: ""}}, {fakeTool{name: "a", decl: &googlegenai.FunctionDeclaration{Name: "b"}}}, {valid, valid}} {
		_, err := newToolRegistry(tools)
		assert.Error(t, err)
	}
}

func TestGenerateRespondsToMalformedUnknownAndOverBudgetCalls(t *testing.T) {
	tool := testTool("known", func(context.Context, map[string]any) (any, error) { return "ok", nil })
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		if len(contents) == 1 {
			return toolResponse(
				&googlegenai.FunctionCall{ID: "u", Name: "unknown"},
				&googlegenai.FunctionCall{ID: "k", Name: "known"},
				&googlegenai.FunctionCall{ID: "extra", Name: "known"},
			), nil
		}
		responses := contents[2].Parts
		require.Len(t, responses, 3)
		assert.Equal(t, toolErrorUnsupported, toolErrorCode(t, responses[0].FunctionResponse.Response))
		assert.Equal(t, "ok", responses[1].FunctionResponse.Response["output"])
		assert.Equal(t, toolErrorCallLimit, toolErrorCode(t, responses[2].FunctionResponse.Response))
		return response("done", nil), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "x"}}, Tools: []FunctionTool{tool}})
	require.NoError(t, err)
	assert.Equal(t, "done", got.Text)
}

func TestGenerateReturnsToolExecutionErrorToModel(t *testing.T) {
	tool := testTool("known", func(context.Context, map[string]any) (any, error) {
		return nil, errors.New("discord unavailable")
	})
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		if len(contents) == 1 {
			return toolResponse(&googlegenai.FunctionCall{ID: "failed", Name: "known"}), nil
		}
		functionResponse := contents[2].Parts[0].FunctionResponse.Response
		assert.Equal(t, toolErrorExecution, toolErrorCode(t, functionResponse))
		errorValue, ok := functionResponse["error"].(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, errorValue["message"], "discord unavailable")
		return response("I encountered an error while searching the channel.", nil), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "x"}}, Tools: []FunctionTool{tool}})
	require.NoError(t, err)
	assert.Equal(t, "I encountered an error while searching the channel.", got.Text)
}

func TestGeneratePreservesResponsesAndFinalizesAfterSecondRound(t *testing.T) {
	tool := testTool("known", func(_ context.Context, args map[string]any) (any, error) { return args["query"], nil })
	call := 0
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		call++
		switch call {
		case 1:
			return toolResponse(&googlegenai.FunctionCall{ID: "one", Name: "known", Args: map[string]any{"query": "first"}}), nil
		case 2:
			require.Len(t, contents, 3)
			assert.Equal(t, "one", contents[2].Parts[0].FunctionResponse.ID)
			return toolResponse(&googlegenai.FunctionCall{ID: "two", Name: "known", Args: map[string]any{"query": "second"}}), nil
		case 3:
			require.Len(t, contents, 5)
			assert.Empty(t, cfg.Tools[1:])
			assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
			assert.Equal(t, "two", contents[4].Parts[0].FunctionResponse.ID)
			return response("done", nil), nil
		}
		return nil, errors.New("unexpected call")
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "x"}}, Tools: []FunctionTool{tool}})
	require.NoError(t, err)
	assert.Equal(t, "done", got.Text)
	assert.Equal(t, 3, call)
}

func TestGenerateExplainsUnexpectedCallAfterToolRoundLimit(t *testing.T) {
	executions := 0
	tool := testTool("known", func(context.Context, map[string]any) (any, error) {
		executions++
		return "result", nil
	})
	call := 0
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		call++
		switch call {
		case 1:
			return toolResponse(&googlegenai.FunctionCall{ID: "one", Name: "known"}), nil
		case 2:
			return toolResponse(&googlegenai.FunctionCall{ID: "two", Name: "known"}), nil
		case 3:
			assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
			return toolResponse(&googlegenai.FunctionCall{ID: "three", Name: "known"}), nil
		case 4:
			require.Len(t, contents, 7)
			errorResponse := contents[6].Parts[0].FunctionResponse
			assert.Equal(t, "three", errorResponse.ID)
			assert.Equal(t, toolErrorRoundLimit, toolErrorCode(t, errorResponse.Response))
			assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
			return response("I encountered an error while searching the channel.", nil), nil
		}
		return nil, errors.New("unexpected call")
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "x"}}, Tools: []FunctionTool{tool}})
	require.NoError(t, err)
	assert.Equal(t, "I encountered an error while searching the channel.", got.Text)
	assert.Equal(t, 2, executions)
	assert.Equal(t, 4, call)
}

func TestGenerateUsesFallbackWhenToolErrorRecoveryIsNonText(t *testing.T) {
	tool := testTool("known", func(context.Context, map[string]any) (any, error) { return "result", nil })
	call := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		call++
		if call < 4 {
			return toolResponse(&googlegenai.FunctionCall{ID: "call", Name: "known"}), nil
		}
		return toolResponse(&googlegenai.FunctionCall{ID: "four", Name: "known"}), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "x"}}, Tools: []FunctionTool{tool}})
	require.NoError(t, err)
	assert.Equal(t, toolFailureFallback, got.Text)
	assert.Equal(t, 4, call)
}

func TestGenerateUsesFallbackWhenToolFinalizationIsEmpty(t *testing.T) {
	tool := testTool("known", func(context.Context, map[string]any) (any, error) { return "result", nil })
	call := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		call++
		if call < 3 {
			return toolResponse(&googlegenai.FunctionCall{ID: "call", Name: "known"}), nil
		}
		return &googlegenai.GenerateContentResponse{}, nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "x"}}, Tools: []FunctionTool{tool}})
	require.NoError(t, err)
	assert.Equal(t, toolFailureFallback, got.Text)
	assert.Equal(t, 3, call)
}

func toolErrorCode(t *testing.T, response map[string]any) string {
	t.Helper()
	errorValue, ok := response["error"].(map[string]any)
	require.True(t, ok)
	code, ok := errorValue["code"].(string)
	require.True(t, ok)
	return code
}

func TestSanitizeText(t *testing.T) { assert.Equal(t, "hi\nthere", sanitizeText(" hi\x07\nthere ")) }
