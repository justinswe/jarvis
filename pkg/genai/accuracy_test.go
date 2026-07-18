package genai

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegenai "google.golang.org/genai"
)

type evidenceTestTool struct {
	fakeTool
	evidence Evidence
}

func (t evidenceTestTool) Evidence(any) (Evidence, bool) { return t.evidence, true }

func runtimeTestTool(executionError error) evidenceTestTool {
	return evidenceTestTool{
		fakeTool: testTool(runtimeContextFunctionName, func(context.Context, map[string]any) (any, error) {
			if executionError != nil {
				return nil, executionError
			}
			return map[string]string{"safe": "runtime values"}, nil
		}),
		evidence: Evidence{Kind: EvidenceKindRuntimeContext, Tool: runtimeContextFunctionName, Attributes: map[string]string{
			"version":      "v0.6.0",
			"timezone":     "UTC",
			"current_time": "2026-07-16T18:30:45Z",
			"current_date": "2026-07-16",
			"weekday":      "Thursday",
		}},
	}
}

func channelSearchTestTool(executionError error) evidenceTestTool {
	return evidenceTestTool{
		fakeTool: testTool(ChannelSearchFunctionName, func(context.Context, map[string]any) (any, error) {
			if executionError != nil {
				return nil, executionError
			}
			return map[string]any{"results": []any{}}, nil
		}),
		evidence: Evidence{Kind: EvidenceKindChannelHistory, Tool: ChannelSearchFunctionName},
	}
}

func codeExecutionResponse(text string, outcome googlegenai.Outcome) *googlegenai.GenerateContentResponse {
	return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{Content: &googlegenai.Content{
		Role: "model",
		Parts: []*googlegenai.Part{
			{ExecutableCode: &googlegenai.ExecutableCode{Language: googlegenai.LanguagePython, Code: "print(42)"}},
			{CodeExecutionResult: &googlegenai.CodeExecutionResult{Outcome: outcome, Output: "42"}},
			{Text: text},
		},
	}}}}
}

func TestClassifyAccuracyPolicyUsesOnlyCurrentIntent(t *testing.T) {
	tests := []struct {
		name    string
		request string
		want    AccuracyPolicy
	}{
		{name: "runtime time", request: "What time is it in America/Los_Angeles?", want: AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName}, RuntimeContextRelevant: true}},
		{name: "local runtime", request: "What's my local time in America/Los_Angeles?", want: AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName}, RuntimeContextRelevant: true}},
		{name: "runtime version", request: "What version are you?", want: AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName}, RuntimeContextRelevant: true}},
		{name: "runtime and volatile", request: "What version are you, and who is the current CEO of Acme?", want: AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName}, GroundingRequired: true, RuntimeContextRelevant: true}},
		{name: "provenance", request: "Where did you get that 5:06 PM PDT claim from?", want: AccuracyPolicy{ProvenanceInquiry: true}},
		{name: "explicit research", request: "Research the latest stable Go release and cite sources.", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "volatile price", request: "What is AAPL's price right now?", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "contractions", request: "What's Apple's price right now?", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "implicit officeholder", request: "Who is the president of Freedonia?", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "whats happening", request: "What's happening?", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "most recent event", request: "What happened most recently?", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "anything new", request: "Anything new?", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "catch up", request: "Catch me up.", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "recent developments", request: "Tell me about recent developments.", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "just happened", request: "What just happened?", want: AccuracyPolicy{GroundingRequired: true}},
		{name: "today research", request: "What happened in markets today?", want: AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName}, GroundingRequired: true, RuntimeContextRelevant: true}},
		{name: "channel search", request: "Search this channel for deploy.", want: AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}}},
		{name: "earlier message", request: "Find Alice's earlier message about launch.", want: AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}}},
		{name: "what was said here", request: "What did Alice say here before?", want: AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}}},
		{name: "channel and web", request: "Search this channel and the web for deploy guidance.", want: AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}, GroundingRequired: true}},
		{name: "channel today", request: "Search this channel for messages from today.", want: AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName, runtimeContextFunctionName}, RuntimeContextRelevant: true}},
		{name: "calculation", request: "Calculate 17 * 29 exactly.", want: AccuracyPolicy{CodeExecutionEnabled: true}},
		{name: "conversion", request: "Convert 10 km to miles.", want: AccuracyPolicy{CodeExecutionEnabled: true}},
		{name: "statistics", request: "What is the average of 4, 8, and 15?", want: AccuracyPolicy{CodeExecutionEnabled: true}},
		{name: "time complexity", request: "Explain the time complexity of binary search."},
		{name: "quoted historical request", request: "Why did Alice ask \"what time is it?\" earlier?"},
		{name: "single quoted historical request", request: "Why did Alice ask 'what time is it?' earlier?"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, ClassifyAccuracyPolicy(test.request))
		})
	}
}

func TestBroadRecencyRequestNeedsScopeOnlyOnce(t *testing.T) {
	firstTurn := []Message{{Role: "user", Content: "What's happening?"}}
	assert.True(t, broadRecencyNeedsClarification(firstTurn))

	repeated := []Message{
		{Role: "user", Content: "What's happening?"},
		{Role: "model", Content: "What topic or region should I focus on?"},
		{Role: "user", Content: "Anything new?"},
	}
	assert.False(t, broadRecencyNeedsClarification(repeated))
	compiledHistory := []Message{{Role: "user", Content: "THREAD HISTORY:\n[timestamp unavailable] user: What's happening?\n[timestamp unavailable] Jarvis [bot]: What topic or region?\n\nCURRENT REQUEST:\nAnything new?"}}
	assert.False(t, broadRecencyNeedsClarification(compiledHistory))

	scoped := []Message{{Role: "user", Content: "What's happening in Argentina?"}}
	assert.False(t, broadRecencyNeedsClarification(scoped))
}

func TestSearchTriggerUsesStrongestDeterministicSignal(t *testing.T) {
	tests := []struct {
		request string
		enabled bool
		want    string
	}{
		{request: "Why is the sky blue?", enabled: true, want: searchTriggerModelOptional},
		{request: "Why is the sky blue?", enabled: false, want: searchTriggerNone},
		{request: "Research the latest Go release.", enabled: true, want: searchTriggerExplicit},
		{request: "What is the latest Go release?", enabled: true, want: searchTriggerVolatile},
		{request: "Who is the president of Freedonia?", enabled: true, want: searchTriggerImplicitVolatile},
	}
	for _, test := range tests {
		policy := ClassifyAccuracyPolicy(test.request)
		assert.Equal(t, test.want, classifySearchTrigger(test.request, policy, test.enabled), test.request)
	}
}

func TestChannelHistoryValidationRequiresRecordedEvidence(t *testing.T) {
	policy := AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}}
	assert.Equal(t, "missing_channel_history_evidence", accuracyValidationFailure(
		"Alice said deploy at noon.", "What did Alice say here before?", "", policy, nil,
	))
	assert.Empty(t, accuracyValidationFailure(
		"Alice said deploy at noon.", "What did Alice say here before?", "", policy,
		[]Evidence{{Kind: EvidenceKindChannelHistory, Tool: ChannelSearchFunctionName}},
	))
	assert.Equal(t, channelHistoryFailureFallback, accuracyFallback(policy, "missing_channel_history_evidence"))
}

func TestMergeAccuracyPoliciesUnionsRequiredFunctions(t *testing.T) {
	got := mergeAccuracyPolicies(
		AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}},
		AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName, ChannelSearchFunctionName}},
	)
	assert.Equal(t, []string{ChannelSearchFunctionName, runtimeContextFunctionName}, got.RequiredFunctionNames)
}

func TestCurrentRequestExcludesHistoricalTranscript(t *testing.T) {
	messages := []Message{{Role: "user", Content: "THREAD HISTORY:\n[timestamp unavailable] alice: What time is it?\n\nCURRENT REQUEST:\nExplain time complexity."}}
	request := currentRequest(messages)
	assert.Equal(t, "Explain time complexity.", request)
	assert.Equal(t, AccuracyPolicy{}, ClassifyAccuracyPolicy(request))
}

func TestRuntimeClaimValidationComparesEveryReturnedField(t *testing.T) {
	evidence := []Evidence{{Kind: EvidenceKindRuntimeContext, Tool: runtimeContextFunctionName, Attributes: map[string]string{
		"version":      "v0.6.0",
		"timezone":     "America/Los_Angeles",
		"current_time": "2026-07-16T18:30:45Z",
		"current_date": "2026-07-16",
		"weekday":      "Thursday",
	}}}
	policy := AccuracyPolicy{RuntimeContextRelevant: true}
	assert.Empty(t, accuracyValidationFailure(
		"It is 11:30:45 AM PDT on July 16, 2026 (Thursday) in America/Los_Angeles; version v0.6.0.",
		"What time is it?", "", policy, evidence,
	))
	for _, test := range []struct {
		name, request, text, want string
	}{
		{name: "time", request: "What time is it?", text: "It is 11:31 AM PDT.", want: "runtime_time_mismatch"},
		{name: "timezone", request: "What time is it?", text: "It is 11:30 AM EDT.", want: "runtime_timezone_mismatch"},
		{name: "date", request: "What date is it?", text: "The date is 7/17/2026.", want: "runtime_date_mismatch"},
		{name: "year", request: "What year is it?", text: "The current year is 2025.", want: "runtime_year_mismatch"},
		{name: "weekday", request: "What day is it?", text: "It is Friday.", want: "runtime_weekday_mismatch"},
		{name: "version", request: "What version are you?", text: "Jarvis version v0.7.0.", want: "runtime_version_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, accuracyValidationFailure(test.text, test.request, "", policy, evidence))
		})
	}
	assert.Equal(t, "missing_runtime_time_claim", accuracyValidationFailure(
		"I checked runtime context.", "What time is it?", "", policy, evidence,
	))
	assert.Equal(t, "missing_runtime_timezone_claim", accuracyValidationFailure(
		"It is 11:30.", "What time is it?", "", policy, evidence,
	))
	assert.Equal(t, "missing_runtime_version_claim", accuracyValidationFailure(
		"I'm Jarvis.", "What version are you?", "", policy, evidence,
	))
}

func TestRuntimeValidatorAllowsRequestedHistoricalOrConversionTimes(t *testing.T) {
	assert.Empty(t, accuracyValidationFailure(
		"5:06 PM PDT converts to 00:06 UTC the next day.",
		"Convert 5:06 PM PDT to UTC.", "", AccuracyPolicy{}, nil,
	))
	assert.Empty(t, accuracyValidationFailure(
		"Apollo 11 landed on July 20, 1969.",
		"When did Apollo 11 land?", "", AccuracyPolicy{}, nil,
	))
}

func TestProvenanceValidationAllowsOnlyRecordedRuntimeClaim(t *testing.T) {
	policy := AccuracyPolicy{ProvenanceInquiry: true}
	history := "[2026-07-16T00:00:00Z] Jarvis [bot]: It was 5:06 PM PDT.\n-# Evidence used: runtime context"
	assert.Empty(t, accuracyValidationFailure("The recorded claim was 5:06 PM PDT.", "Where did that come from?", history, policy, nil))
	assert.Equal(t, "unsupported_provenance_runtime_claim", accuracyValidationFailure(
		"The recorded claim was 5:06 PM PDT, but it is 6:10 PM PDT now.",
		"Where did that come from?", history, policy, nil,
	))
	assert.Equal(t, "unsupported_provenance_runtime_claim", accuracyValidationFailure(
		"The unsupported claim was 5:06 PM PDT.",
		"Where did that come from?", "[timestamp] Jarvis [bot]: It was 5:06 PM PDT.", policy, nil,
	))
}

func TestGenerateForcesOnlyRequiredRuntimeFunctionThenReturnsToAuto(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		switch calls {
		case 1:
			require.Len(t, cfg.Tools, 1)
			require.Len(t, cfg.Tools[0].FunctionDeclarations, 1)
			assert.Equal(t, runtimeContextFunctionName, cfg.Tools[0].FunctionDeclarations[0].Name)
			require.NotNil(t, cfg.ToolConfig)
			assert.Equal(t, googlegenai.FunctionCallingConfigModeAny, cfg.ToolConfig.FunctionCallingConfig.Mode)
			assert.Equal(t, []string{runtimeContextFunctionName}, cfg.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
			return toolResponse(&googlegenai.FunctionCall{ID: "runtime", Name: runtimeContextFunctionName}), nil
		case 2:
			require.Len(t, contents, 3)
			require.Len(t, cfg.Tools, 1)
			assert.Len(t, cfg.Tools[0].FunctionDeclarations, 2)
			assert.Equal(t, googlegenai.FunctionCallingConfigModeAuto, cfg.ToolConfig.FunctionCallingConfig.Mode)
			return response("It is 18:30:45 UTC on 2026-07-16, a Thursday.", nil), nil
		default:
			return nil, errors.New("unexpected generation call")
		}
	})
	tools := []FunctionTool{
		runtimeTestTool(nil),
		testTool("unrelated", func(context.Context, map[string]any) (any, error) { return "unused", nil }),
	}
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What time is it?"}},
		Tools:    tools,
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, "It is 18:30:45 UTC on 2026-07-16, a Thursday.", got.Text)
	assert.Equal(t, 2, calls)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, EvidenceKindRuntimeContext, got.Evidence[0].Kind)
}

func TestGenerateForcesChannelSearchWithoutGoogleSearch(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, contents []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		switch calls {
		case 1:
			require.Len(t, cfg.Tools, 1)
			require.Len(t, cfg.Tools[0].FunctionDeclarations, 1)
			assert.Equal(t, ChannelSearchFunctionName, cfg.Tools[0].FunctionDeclarations[0].Name)
			assert.Nil(t, cfg.Tools[0].GoogleSearch)
			assert.Equal(t, googlegenai.FunctionCallingConfigModeAny, cfg.ToolConfig.FunctionCallingConfig.Mode)
			return toolResponse(&googlegenai.FunctionCall{ID: "history", Name: ChannelSearchFunctionName, Args: map[string]any{"query": "deploy"}}), nil
		case 2:
			require.Len(t, contents, 3)
			require.Len(t, cfg.Tools, 1)
			assert.Nil(t, cfg.Tools[0].GoogleSearch)
			return response("Alice mentioned the deploy window.", nil), nil
		default:
			return nil, errors.New("unexpected generation call")
		}
	})
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Search this channel for deploy."}},
		Tools:    []FunctionTool{channelSearchTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "Alice mentioned the deploy window.", got.Text)
	assert.Equal(t, 2, calls)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, EvidenceKindChannelHistory, got.Evidence[0].Kind)
}

func TestGenerateRequiresChannelSearchAndWebGrounding(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		switch calls {
		case 1:
			require.Len(t, cfg.Tools, 1)
			assert.Equal(t, ChannelSearchFunctionName, cfg.Tools[0].FunctionDeclarations[0].Name)
			return toolResponse(&googlegenai.FunctionCall{ID: "history", Name: ChannelSearchFunctionName, Args: map[string]any{"query": "deploy"}}), nil
		case 2:
			require.Len(t, cfg.Tools, 2)
			assert.NotNil(t, cfg.Tools[0].GoogleSearch)
			return response("The stored note agrees with the external guidance.", &googlegenai.GroundingMetadata{
				WebSearchQueries: []string{"deploy guidance"},
				GroundingChunks: []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{
					URI: "https://example.com/deploy-guidance",
				}}},
			}), nil
		default:
			return nil, errors.New("unexpected generation call")
		}
	})
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Search this channel and the web for deploy guidance."}},
		Tools:    []FunctionTool{channelSearchTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, "The stored note agrees with the external guidance.", got.Text)
	assert.True(t, got.Grounded)
	require.Len(t, got.Evidence, 2)
	assert.Equal(t, EvidenceKindChannelHistory, got.Evidence[0].Kind)
	assert.Equal(t, EvidenceKindWeb, got.Evidence[1].Kind)
	require.Len(t, got.Sources, 1)
}

func TestGenerateReturnsChannelHistoryFallbackWhenRequiredToolIsUnavailable(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		assert.Empty(t, cfg.Tools)
		return response("Alice said deploy at noon.", nil), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Search this channel for deploy."}},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: true},
	})
	require.NoError(t, err)
	assert.Equal(t, channelHistoryFailureFallback, got.Text)
	assert.Equal(t, 1, calls)
}

func TestGenerateRequiresGroundingWhenSearchWasNeverAttempted(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return response("unsupported current answer", nil), nil
		}
		require.Len(t, cfg.Tools, 1)
		assert.NotNil(t, cfg.Tools[0].GoogleSearch)
		return response("verified current answer", &googlegenai.GroundingMetadata{
			WebSearchQueries: []string{"latest release"},
			GroundingChunks:  []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{URI: "https://go.dev/doc/devel/release"}}},
		}), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "What is the latest Go release?"}}})
	require.NoError(t, err)
	assert.Equal(t, "verified current answer", got.Text)
	assert.True(t, got.Grounded)
	assert.Equal(t, 2, calls)
}

func TestGenerateQualifiesRequiredCurrentClaimWhenSearchIsDisabled(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return response("The current CEO is Alice.", nil), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "Who is the current CEO of Acme?"}},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, groundingDisabledFallback, got.Text)
	assert.Contains(t, got.Text, "stable background")
	assert.False(t, got.Grounded)
	assert.Equal(t, 1, calls)
}

func TestGenerateEnablesCodeExecutionOnlyForComputation(t *testing.T) {
	t.Run("calculation", func(t *testing.T) {
		h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			require.Len(t, cfg.Tools, 1)
			assert.NotNil(t, cfg.Tools[0].CodeExecution)
			return codeExecutionResponse("17 × 29 = 493.", googlegenai.OutcomeOK), nil
		})
		got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "Calculate 17 * 29 exactly."}}})
		require.NoError(t, err)
		assert.Equal(t, "17 × 29 = 493.", got.Text)
		require.Len(t, got.Evidence, 1)
		assert.Equal(t, EvidenceKindCodeExecution, got.Evidence[0].Kind)
	})

	t.Run("stable question", func(t *testing.T) {
		h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
			require.Len(t, cfg.Tools, 1)
			assert.Nil(t, cfg.Tools[0].CodeExecution)
			return response("Rayleigh scattering.", nil), nil
		})
		_, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "Why is the sky blue?"}}})
		require.NoError(t, err)
	})
}

func TestGenerateRetriesMissingCodeExecutionOnce(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return response("493", nil), nil
		}
		assert.Contains(t, cfg.SystemInstruction.Parts[0].Text, codeExecutionRetryPrompt)
		return codeExecutionResponse("17 × 29 = 493.", googlegenai.OutcomeOK), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "Calculate 17 * 29 exactly."}}})
	require.NoError(t, err)
	assert.Equal(t, "17 × 29 = 493.", got.Text)
	assert.Equal(t, 2, calls)
}

func TestCodeExecutionRecoveryAlsoPreservesRequiredGrounding(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		metadata := &googlegenai.GroundingMetadata{
			WebSearchQueries: []string{"latest stock price"},
			GroundingChunks:  []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{URI: "https://example.com/price"}}},
		}
		if calls == 1 {
			return response("grounded but not computed", metadata), nil
		}
		require.Len(t, cfg.Tools, 2)
		assert.NotNil(t, cfg.Tools[0].GoogleSearch)
		assert.NotNil(t, cfg.Tools[1].CodeExecution)
		recovery := codeExecutionResponse("The computed result is 493.", googlegenai.OutcomeOK)
		recovery.Candidates[0].GroundingMetadata = metadata
		return recovery, nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "Look up the latest stock price and calculate 17 * 29."}}})
	require.NoError(t, err)
	assert.Equal(t, "The computed result is 493.", got.Text)
	assert.True(t, got.Grounded)
	assert.Equal(t, 2, calls)
}

func TestGenerateRejectsUnsupportedRuntimeClaimAndRetriesOnce(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return response("My internal clock says 5:06 PM PDT.", nil), nil
		}
		assert.Contains(t, cfg.SystemInstruction.Parts[0].Text, accuracyRetryPrompt)
		return response("Here is a migration limerick.", nil), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "Write a limerick about a database migration."}}})
	require.NoError(t, err)
	assert.Equal(t, "Here is a migration limerick.", got.Text)
	assert.Equal(t, 2, calls)
}

func TestGenerateRejectsMismatchedRuntimeValueWithOneCorrection(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		switch calls {
		case 1:
			return toolResponse(&googlegenai.FunctionCall{ID: "runtime", Name: runtimeContextFunctionName}), nil
		case 2:
			return response("It is 5:06 PM PDT.", nil), nil
		case 3:
			assert.Contains(t, cfg.SystemInstruction.Parts[0].Text, accuracyRetryPrompt)
			return response("It is 18:30 UTC.", nil), nil
		default:
			return nil, errors.New("unexpected generation call")
		}
	})
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What time is it?"}},
		Tools:    []FunctionTool{runtimeTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, "It is 18:30 UTC.", got.Text)
	assert.Equal(t, 3, calls)
}

func TestGenerateRefreshesSourcedButStalePriorTime(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return toolResponse(&googlegenai.FunctionCall{ID: "runtime", Name: runtimeContextFunctionName}), nil
		}
		return response("It is now 18:30 UTC.", nil), nil
	})
	message := "THREAD HISTORY:\n[2026-07-16T17:00:00Z] Jarvis [bot]: It is 17:00 UTC.\n-# Evidence used: runtime context\n\nCURRENT REQUEST:\nWhat time is it now?"
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: message}},
		Tools:    []FunctionTool{runtimeTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, "It is now 18:30 UTC.", got.Text)
	assert.NotContains(t, got.Text, "17:00")
	assert.Equal(t, 2, calls)
}

func TestGenerateUsesTerminalFallbackAfterSingleAccuracyRetry(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return response("My internal clock says 5:06 PM PDT.", nil), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "Tell me a joke."}}})
	require.NoError(t, err)
	assert.Equal(t, accuracyFailureFallback, got.Text)
	assert.Equal(t, 2, calls)
}

func TestGenerateProvenanceInquiryNeverInventsClockOrReplacementTime(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return response("I got it from my internal clock; the actual time is 6:10 PM PDT.", nil), nil
		}
		assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
		return response("No source was preserved for that earlier claim, so I can't verify where it came from.", nil), nil
	})
	message := "THREAD HISTORY:\n[2026-07-16T00:00:00Z] Jarvis [bot]: It is 5:06 PM PDT.\n\nCURRENT REQUEST:\nWhere did you get that time from?"
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: message}},
		Tools:    []FunctionTool{runtimeTestTool(nil)},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, provenanceFailureFallback, got.Text)
	assert.NotContains(t, got.Text, "6:10")
	assert.Empty(t, got.Evidence)
	assert.Equal(t, 2, calls)
}

func TestGenerateRuntimeToolFailureReturnsUnverifiedFallback(t *testing.T) {
	calls := 0
	h := testHandler(func(_ context.Context, _ string, _ []*googlegenai.Content, cfg *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return toolResponse(&googlegenai.FunctionCall{ID: "runtime", Name: runtimeContextFunctionName}), nil
		}
		assert.Empty(t, cfg.Tools)
		assert.Equal(t, googlegenai.FunctionCallingConfigModeNone, cfg.ToolConfig.FunctionCallingConfig.Mode)
		return response("It is 5:06 PM PDT.", nil), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "What time is it?"}},
		Tools:    []FunctionTool{runtimeTestTool(errors.New("clock unavailable"))},
		Config:   &RequestConfig{MaxOutputTokens: 512, WebSearchEnabled: false},
	})
	require.NoError(t, err)
	assert.Equal(t, runtimeVerificationFallback, got.Text)
	assert.Empty(t, got.Evidence)
	assert.Equal(t, 2, calls)
}

func TestGenerateAsksForIANATimezoneWithoutCallingModel(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		return response("should not be called", nil), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "What time is it for me?"}}})
	require.NoError(t, err)
	assert.Equal(t, timezoneClarificationFallback, got.Text)
	assert.Zero(t, calls)
}

func TestGenerateSharesOneRetryAcrossGroundingAndCodeExecution(t *testing.T) {
	calls := 0
	h := testHandler(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return response("unsupported calculation", nil), nil
		}
		return response("grounded but not computed", &googlegenai.GroundingMetadata{
			WebSearchQueries: []string{"latest stock price"},
			GroundingChunks:  []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{URI: "https://example.com/price"}}},
		}), nil
	})
	got, err := h.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: "user", Content: "Look up the latest stock price and calculate 17 * 29."}}})
	require.NoError(t, err)
	assert.Equal(t, codeExecutionFailureFallback, got.Text)
	assert.Equal(t, 2, calls)
}
