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

func thoughtOnlyResponse(reason googlegenai.FinishReason) *googlegenai.GenerateContentResponse {
	return &googlegenai.GenerateContentResponse{
		Candidates: []*googlegenai.Candidate{{
			Content:      &googlegenai.Content{Role: "model", Parts: []*googlegenai.Part{{Text: "internal reasoning", Thought: true}}},
			FinishReason: reason,
		}},
		UsageMetadata: &googlegenai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 1266, CandidatesTokenCount: 18, ThoughtsTokenCount: 488, TotalTokenCount: 1772,
		},
	}
}

func testHandler(generate generateFunc) *Handler {
	return &Handler{cfg: Config{MaxOutputTokens: 256}, generate: generate}
}

func TestContentConfig(t *testing.T) {
	h := testHandler(nil)
	cfg := h.contentConfig(true, []*googlegenai.FunctionDeclaration{{Name: "tool"}}, googlegenai.FunctionCallingConfigModeAuto)
	require.Len(t, cfg.Tools, 2)
	assert.NotNil(t, cfg.Tools[0].GoogleSearch)
	assert.Equal(t, "tool", cfg.Tools[1].FunctionDeclarations[0].Name)
	assert.Equal(t, googlegenai.ThinkingLevelMedium, cfg.ThinkingConfig.ThinkingLevel)
	assert.Contains(t, cfg.SystemInstruction.Parts[0].Text, webSearchSystemPrompt)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeAuto, cfg.ToolConfig.FunctionCallingConfig.Mode)
	disabled := h.contentConfig(false, nil, googlegenai.FunctionCallingConfigModeNone)
	assert.Empty(t, disabled.Tools)
	assert.NotContains(t, disabled.SystemInstruction.Parts[0].Text, webSearchSystemPrompt)
	assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, disabled.ToolConfig.FunctionCallingConfig.Mode)
}

func TestNewRejectsInvalidConfigurationBeforeClientCreation(t *testing.T) {
	_, err := New(context.Background(), Config{ProjectID: "p", MaxOutputTokens: 8193})
	assert.ErrorContains(t, err, "between 1 and 8192")
}

func TestGenerateGroundingSources(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, model string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		assert.Equal(t, "gemini-3.1-flash-lite", model)
		require.Len(t, cfg.Tools, 1)
		return response("answer", &googlegenai.GroundingMetadata{
			WebSearchQueries: []string{"current news"},
			GroundingChunks:  []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{Title: "One", URI: "https://one.example"}}},
		}), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "current news"}}})
	require.NoError(t, err)
	assert.True(t, got.Grounded)
	assert.Len(t, got.Sources, 1)
	assert.Equal(t, 1, calls)
}

func TestAnalyzeGroundingPrioritizesSupportedValidSources(t *testing.T) {
	resp := response("answer", &googlegenai.GroundingMetadata{
		WebSearchQueries:  []string{"query"},
		SearchEntryPoint:  &googlegenai.SearchEntryPoint{RenderedContent: "<div>suggestion</div>"},
		GroundingSupports: []*googlegenai.GroundingSupport{{GroundingChunkIndices: []int32{2, 0}}},
		GroundingChunks: []*googlegenai.GroundingChunk{
			{Web: &googlegenai.GroundingChunkWeb{Title: "One", URI: "HTTPS://ONE.example/a#fragment"}},
			{Web: &googlegenai.GroundingChunkWeb{Title: "Invalid", URI: "ftp://invalid.example"}},
			{Web: &googlegenai.GroundingChunkWeb{Title: "Two", URI: "https://two.example/b"}},
			{Web: &googlegenai.GroundingChunkWeb{Title: "Duplicate", URI: "https://one.example/a"}},
		},
	})

	diagnostics := analyzeGrounding(resp, 3)
	assert.True(t, diagnostics.metadataPresent)
	assert.True(t, diagnostics.searchAttempted)
	assert.True(t, diagnostics.searchEntryPoint)
	assert.True(t, diagnostics.searchRenderedContent)
	assert.Equal(t, groundingOutcomeGrounded, diagnostics.outcome)
	assert.Equal(t, 4, diagnostics.chunkCount)
	assert.Equal(t, 4, diagnostics.webChunkCount)
	assert.Equal(t, 1, diagnostics.supportCount)
	assert.Equal(t, 2, diagnostics.validSourceCount)
	assert.Equal(t, 1, diagnostics.invalidSourceCount)
	assert.Equal(t, 1, diagnostics.duplicateSourceCount)
	assert.Equal(t, []Source{{Title: "Two", URL: "https://two.example/b"}, {Title: "One", URL: "https://one.example/a"}}, diagnostics.sources)
	assert.Equal(t, []string{"one.example", "two.example"}, diagnostics.sourceDomains)
}

func TestAnalyzeGroundingRecordsSearchWithoutChunks(t *testing.T) {
	diagnostics := analyzeGrounding(response("answer", &googlegenai.GroundingMetadata{WebSearchQueries: []string{"query"}}), 3)
	assert.True(t, diagnostics.searchAttempted)
	assert.Zero(t, diagnostics.validSourceCount)
	assert.Equal(t, groundingOutcomeSearchedWithoutChunks, diagnostics.outcome)
}

func TestGenerateRetriesSearchWithoutSourcesAndRequiresGrounding(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return response("unverified answer", &googlegenai.GroundingMetadata{WebSearchQueries: []string{"query"}}), nil
		}
		require.Len(t, cfg.Tools, 1)
		assert.NotNil(t, cfg.Tools[0].GoogleSearch)
		require.NotNil(t, cfg.ToolConfig)
		assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
		assert.Equal(t, float32(1.0), *cfg.Temperature)
		assert.Equal(t, googlegenai.ThinkingLevelMedium, cfg.ThinkingConfig.ThinkingLevel)
		assert.GreaterOrEqual(t, cfg.MaxOutputTokens, int32(emptyRecoveryMinTokens))
		assert.Contains(t, cfg.SystemInstruction.Parts[0].Text, groundingRetryPrompt)
		return response("verified answer", &googlegenai.GroundingMetadata{
			WebSearchQueries: []string{"retry"},
			GroundingChunks:  []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{URI: "https://source.example/fact"}}},
		}), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "latest fact"}}})
	require.NoError(t, err)
	assert.Equal(t, "verified answer", got.Text)
	assert.True(t, got.Grounded)
	assert.Equal(t, []Source{{Title: "source.example", URL: "https://source.example/fact"}}, got.Sources)
	assert.Equal(t, 2, calls)
}

func TestGenerateDiscardsUnverifiedGroundingRecovery(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return response("unverified answer", &googlegenai.GroundingMetadata{WebSearchQueries: []string{"query"}}), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "latest fact"}}})
	require.NoError(t, err)
	assert.Equal(t, groundingFailureFallback, got.Text)
	assert.False(t, got.Grounded)
	assert.Empty(t, got.Sources)
	assert.Equal(t, 2, calls)
}

func TestGenerateReturnsVerificationFallbackWhenGroundingRecoveryFails(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return response("unverified answer", &googlegenai.GroundingMetadata{WebSearchQueries: []string{"query"}}), nil
		}
		return nil, errors.New("search unavailable")
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "latest fact"}}})
	require.NoError(t, err)
	assert.Equal(t, groundingFailureFallback, got.Text)
	assert.Equal(t, 2, calls)
}

func TestGenerateDoesNotRequireGroundingWithoutSearchQueries(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return response("answer from model knowledge", &googlegenai.GroundingMetadata{}), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "stable fact"}}})
	require.NoError(t, err)
	assert.Equal(t, "answer from model knowledge", got.Text)
	assert.Equal(t, 1, calls)
}

func TestGenerateUsesRequestScopedConfiguration(t *testing.T) {
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		assert.Empty(t, cfg.Tools)
		assert.Equal(t, int32(123), cfg.MaxOutputTokens)
		assert.Equal(t, float32(0.4), *cfg.Temperature)
		assert.Contains(t, cfg.SystemInstruction.Parts[0].Text, "server prompt")
		assert.NotContains(t, cfg.SystemInstruction.Parts[0].Text, webSearchSystemPrompt)
		return response("answer", nil), nil
	})
	_, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{Prompt: "server prompt", MaxOutputTokens: 123, Temperature: 0.4, WebSearchEnabled: false},
	})
	require.NoError(t, err)
}

func TestGenerateUsesRequestThinkingLevel(t *testing.T) {
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		assert.Equal(t, googlegenai.ThinkingLevelHigh, cfg.ThinkingConfig.ThinkingLevel)
		return response("answer", nil), nil
	})
	_, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config: &RequestConfig{
			Prompt: "server prompt", MaxOutputTokens: 123, WebSearchEnabled: false, ThinkingLevel: googlegenai.ThinkingLevelHigh,
		},
	})
	require.NoError(t, err)

	_, err = h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config: &RequestConfig{
			Prompt: "server prompt", MaxOutputTokens: 123, ThinkingLevel: googlegenai.ThinkingLevelLow,
		},
	})
	assert.ErrorContains(t, err, "thinking level")
}

func TestGenerateFallbackCaveatMatchesSearchAvailability(t *testing.T) {
	for _, test := range []struct {
		name, wantText string
		webSearch      bool
	}{
		{name: "search enabled", webSearch: true, wantText: "answer\n\n" + verificationCaveat},
		{name: "search disabled", wantText: "answer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
				calls++
				if calls == 1 {
					return nil, errors.New("tool-enabled call failed")
				}
				assert.Empty(t, cfg.Tools)
				assert.NotContains(t, cfg.SystemInstruction.Parts[0].Text, webSearchSystemPrompt)
				return response("answer", nil), nil
			})
			tool := testTool("tool", func(context.Context, map[string]any) (any, error) { return nil, nil })
			got, err := h.Generate(context.Background(), GenerateRequest{
				Messages: []Message{{Role: "user", Content: "hello"}},
				Tools:    []FunctionTool{tool},
				Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 123, WebSearchEnabled: test.webSearch},
			})
			require.NoError(t, err)
			assert.Equal(t, test.wantText, got.Text)
			assert.Equal(t, 2, calls)
		})
	}
}

func TestGenerateDoesNotRetryToolFreeFailure(t *testing.T) {
	wantErr := errors.New("generation failed")
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return nil, wantErr
	})
	_, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 123, WebSearchEnabled: false},
	})
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, calls)
}

func TestGenerateRecoversThoughtOnlyMaxTokensResponse(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			assert.Equal(t, int32(512), cfg.MaxOutputTokens)
			assert.Equal(t, googlegenai.ThinkingLevelMedium, cfg.ThinkingConfig.ThinkingLevel)
			return thoughtOnlyResponse(googlegenai.FinishReasonMaxTokens), nil
		}
		assert.Empty(t, cfg.Tools)
		assert.NotContains(t, cfg.SystemInstruction.Parts[0].Text, webSearchSystemPrompt)
		assert.Equal(t, int32(emptyRecoveryMinTokens), cfg.MaxOutputTokens)
		assert.Equal(t, googlegenai.ThinkingLevelLow, cfg.ThinkingConfig.ThinkingLevel)
		require.NotNil(t, cfg.ToolConfig)
		assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
		return response("recovered answer", nil), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, "recovered answer", got.Text)
	assert.Equal(t, 2, calls)
}

func TestGenerateRecoversAfterToolWithoutExecutingItTwice(t *testing.T) {
	executions := 0
	tool := testTool("react", func(context.Context, map[string]any) (any, error) {
		executions++
		return "ok", nil
	})
	calls := 0
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		switch calls {
		case 1:
			return toolResponse(&googlegenai.FunctionCall{ID: "reaction", Name: "react"}), nil
		case 2:
			require.Len(t, contents, 3)
			assert.Equal(t, "reaction", contents[2].Parts[0].FunctionResponse.ID)
			return thoughtOnlyResponse(googlegenai.FinishReasonMaxTokens), nil
		case 3:
			require.Len(t, contents, 3)
			assert.Empty(t, cfg.Tools)
			assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
			return response("done", nil), nil
		default:
			return nil, errors.New("unexpected generation call")
		}
	})

	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "react and reply"}},
		Tools:    []FunctionTool{tool},
		Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, "done", got.Text)
	assert.Equal(t, 1, executions)
	assert.Equal(t, 3, calls)
}

func TestGroundingRecoveryDoesNotExecuteFunctionToolTwice(t *testing.T) {
	executions := 0
	tool := testTool("lookup", func(context.Context, map[string]any) (any, error) {
		executions++
		return "tool result", nil
	})
	calls := 0
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		switch calls {
		case 1:
			resp := toolResponse(&googlegenai.FunctionCall{ID: "one", Name: "lookup"})
			resp.Candidates[0].GroundingMetadata = &googlegenai.GroundingMetadata{WebSearchQueries: []string{"query"}}
			return resp, nil
		case 2:
			require.Len(t, contents, 3)
			return response("unverified tool answer", nil), nil
		case 3:
			require.Len(t, cfg.Tools, 1)
			assert.NotNil(t, cfg.Tools[0].GoogleSearch)
			assert.Empty(t, cfg.Tools[0].FunctionDeclarations)
			return response("verified tool answer", &googlegenai.GroundingMetadata{
				WebSearchQueries: []string{"retry"},
				GroundingChunks:  []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{URI: "https://source.example"}}},
			}), nil
		default:
			return nil, errors.New("unexpected generation call")
		}
	})

	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "look it up"}},
		Tools:    []FunctionTool{tool},
	})
	require.NoError(t, err)
	assert.Equal(t, "verified tool answer", got.Text)
	assert.Equal(t, 1, executions)
	assert.Equal(t, 3, calls)
}

func TestGenerateDoesNotRetryBlockedEmptyResponse(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{FinishReason: googlegenai.FinishReasonSafety}}}, nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, blockedResponseFallback, got.Text)
	assert.Equal(t, 1, calls)
}

func TestGenerateDoesNotRetryBlockedPrompt(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return &googlegenai.GenerateContentResponse{PromptFeedback: &googlegenai.GenerateContentResponsePromptFeedback{
			BlockReason: googlegenai.BlockedReasonSafety,
		}}, nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, blockedResponseFallback, got.Text)
	assert.Equal(t, 1, calls)
}

func TestGenerateRecoversMissingAndWhitespaceOnlyCandidates(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *googlegenai.GenerateContentResponse
	}{
		{name: "missing response"},
		{name: "missing candidate", response: &googlegenai.GenerateContentResponse{}},
		{name: "nil candidate", response: &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{nil}}},
		{name: "whitespace only", response: response(" \n\t", nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
				calls++
				if calls == 1 {
					return test.response, nil
				}
				return response("recovered", nil), nil
			})

			got, err := h.Generate(context.Background(), GenerateRequest{
				Messages: []Message{{Role: "user", Content: "hello"}},
				Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 512, WebSearchEnabled: false},
			})
			require.NoError(t, err)
			assert.Equal(t, "recovered", got.Text)
			assert.Equal(t, 2, calls)
		})
	}
}

func TestGenerateUsesFallbackWhenEmptyRecoveryHasNoText(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return thoughtOnlyResponse(googlegenai.FinishReasonMaxTokens), nil
		}
		return thoughtOnlyResponse(googlegenai.FinishReasonStop), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, emptyResponseFallback, got.Text)
	assert.Equal(t, 2, calls)
}

func TestGenerateDoesNotRecoverAfterContextCancellation(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return thoughtOnlyResponse(googlegenai.FinishReasonMaxTokens), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.Generate(ctx, GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{Prompt: "prompt", MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}

func TestGenerateRejectsInvalidRequestConfiguration(t *testing.T) {
	h := testHandler(nil)
	_, err := h.Generate(context.Background(), GenerateRequest{Config: &RequestConfig{MaxOutputTokens: 8193}})
	assert.ErrorContains(t, err, "between 1 and 8192")
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

func TestGenerateReturnsStructuredExecutionErrorToModel(t *testing.T) {
	tool := testTool("known", func(context.Context, map[string]any) (any, error) {
		return nil, NewExecutionError("database_unavailable", "Configuration is temporarily unavailable.", errors.New("secret backend failure"))
	})
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		if len(contents) == 1 {
			return toolResponse(&googlegenai.FunctionCall{ID: "failed", Name: "known"}), nil
		}
		functionResponse := contents[2].Parts[0].FunctionResponse.Response
		assert.Equal(t, "database_unavailable", toolErrorCode(t, functionResponse))
		errorValue := functionResponse["error"].(map[string]any)
		assert.Equal(t, "Configuration is temporarily unavailable.", errorValue["message"])
		assert.NotContains(t, errorValue["message"], "secret")
		return response("The configuration is temporarily unavailable.", nil), nil
	})

	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "x"}}, Tools: []FunctionTool{tool}})
	require.NoError(t, err)
	assert.Equal(t, "The configuration is temporarily unavailable.", got.Text)
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
