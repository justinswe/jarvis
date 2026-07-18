package genai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	googlegenai "google.golang.org/genai"
)

const evaluationCorpusPath = "pkg/genai/search_eval_cases.jsonl"

type evaluationCase struct {
	ID               string                 `json:"id"`
	Category         string                 `json:"category"`
	Development      bool                   `json:"development"`
	Messages         []Message              `json:"messages"`
	WebSearchEnabled bool                   `json:"web_search_enabled"`
	Fixture          string                 `json:"fixture,omitempty"`
	Tools            []string               `json:"tools,omitempty"`
	Expectations     evaluationExpectations `json:"expectations"`
}

type evaluationExpectations struct {
	SearchAction          string   `json:"search_action"`
	PermittedDispositions []string `json:"permitted_dispositions"`
	SourcesRequired       bool     `json:"sources_required"`
	ClarificationBehavior string   `json:"clarification_behavior"`
	ProhibitedClaims      []string `json:"prohibited_claims"`
}

type evaluationRecord struct {
	CaseID                  string                 `json:"case_id"`
	Category                string                 `json:"category"`
	PromptVariant           string                 `json:"prompt_variant"`
	Run                     int                    `json:"run"`
	Expectations            evaluationExpectations `json:"expectations"`
	ResponseText            string                 `json:"response_text,omitempty"`
	Error                   string                 `json:"error,omitempty"`
	Disposition             string                 `json:"disposition"`
	SearchRequired          bool                   `json:"search_required"`
	SearchAttempted         bool                   `json:"search_attempted"`
	SearchTrigger           string                 `json:"search_trigger"`
	SearchResult            string                 `json:"search_result"`
	GroundingRetryResult    string                 `json:"grounding_retry_result"`
	SearchQueryCount        int                    `json:"search_query_count"`
	ModelCallCount          int                    `json:"model_call_count"`
	RetryUsed               bool                   `json:"retry_used"`
	SourceCount             int                    `json:"source_count"`
	SupportedSourceCount    int                    `json:"supported_source_count"`
	GroundingOutcome        string                 `json:"grounding_outcome"`
	LatencyMilliseconds     int64                  `json:"latency_milliseconds"`
	TerminalFallbackReason  string                 `json:"terminal_fallback_reason"`
	DeterministicViolations []string               `json:"deterministic_violations,omitempty"`
}

// TestLiveSearchEvaluation runs the opt-in Search behavior evaluation corpus.
func TestLiveSearchEvaluation(t *testing.T) {
	variant := envOrDefault("JARVIS_EVAL_PROMPT_VARIANT", promptVariantConcise)
	if variant != promptVariantBaseline && variant != promptVariantConcise && variant != promptVariantFewShot {
		t.Fatalf("JARVIS_EVAL_PROMPT_VARIANT must be %q, %q, or %q", promptVariantBaseline, promptVariantConcise, promptVariantFewShot)
	}
	subset := envOrDefault("JARVIS_EVAL_SUBSET", "development")
	if subset != "development" && subset != "full" {
		t.Fatalf("JARVIS_EVAL_SUBSET must be %q or %q", "development", "full")
	}
	runs, err := strconv.Atoi(envOrDefault("JARVIS_EVAL_RUNS", "1"))
	if err != nil || runs < 1 {
		t.Fatal("JARVIS_EVAL_RUNS must be a positive integer")
	}

	cases := loadEvaluationCases(t, subset)
	projectID := strings.TrimSpace(os.Getenv("JARVIS_EVAL_PROJECT_ID"))
	if projectID == "" {
		t.Skip("set JARVIS_EVAL_PROJECT_ID to run live Gemini evaluation")
	}
	h, err := New(context.Background(), Config{
		ProjectID:       projectID,
		Location:        envOrDefault("JARVIS_EVAL_LOCATION", "global"),
		MaxOutputTokens: DefaultMaxOutputTokens,
	})
	if err != nil {
		t.Fatalf("create Gemini handler: %v", err)
	}
	defer h.Close()
	h.searchPromptVariant = variant
	liveGenerate := h.generate

	outputPath := evaluationOutputPath(variant, subset)
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatalf("create evaluation output: %v", err)
	}
	defer output.Close()
	encoder := json.NewEncoder(output)

	for run := 1; run <= runs; run++ {
		for _, evalCase := range cases {
			h.generate = liveGenerate
			if evalCase.Fixture != "" {
				h.generate = evaluationFixture(evalCase.Fixture)
			}
			var diagnostics generationDiagnostics
			h.observeGeneration = func(got generationDiagnostics) { diagnostics = got }
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			started := time.Now()
			response, generateErr := h.Generate(ctx, GenerateRequest{
				Messages: evalCase.Messages,
				Tools:    evaluationTools(evalCase.Tools),
				Config: &RequestConfig{
					MaxOutputTokens:  DefaultMaxOutputTokens,
					WebSearchEnabled: evalCase.WebSearchEnabled,
					ThinkingLevel:    googlegenai.ThinkingLevelMedium,
				},
			})
			cancel()

			record := newEvaluationRecord(evalCase, variant, run, response, diagnostics, generateErr, time.Since(started))
			if err := encoder.Encode(record); err != nil {
				t.Fatalf("write evaluation output: %v", err)
			}
		}
	}
	t.Logf("wrote %d evaluation cases x %d run(s) to %s", len(cases), runs, outputPath)
}

func loadEvaluationCases(t *testing.T, subset string) []evaluationCase {
	t.Helper()
	file, err := os.Open(evaluationCorpusFilePath())
	if err != nil {
		t.Fatalf("open evaluation corpus: %v", err)
	}
	defer file.Close()

	var cases []evaluationCase
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		var evalCase evaluationCase
		if err := json.Unmarshal(scanner.Bytes(), &evalCase); err != nil {
			t.Fatalf("parse evaluation corpus line %d: %v", line, err)
		}
		if subset == "development" && !evalCase.Development {
			continue
		}
		cases = append(cases, evalCase)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read evaluation corpus: %v", err)
	}
	if want := map[string]int{"development": 30, "full": 100}[subset]; len(cases) != want {
		t.Fatalf("%s corpus contains %d cases; want %d", subset, len(cases), want)
	}
	return cases
}

func evaluationCorpusFilePath() string {
	runfilesRoot := strings.TrimSpace(os.Getenv("TEST_SRCDIR"))
	workspace := strings.TrimSpace(os.Getenv("TEST_WORKSPACE"))
	if runfilesRoot == "" || workspace == "" {
		return evaluationCorpusPath
	}
	return filepath.Join(runfilesRoot, workspace, evaluationCorpusPath)
}

func newEvaluationRecord(evalCase evaluationCase, variant string, run int, response GenerateResponse, diagnostics generationDiagnostics, generateErr error, elapsed time.Duration) evaluationRecord {
	record := evaluationRecord{
		CaseID:                 evalCase.ID,
		Category:               evalCase.Category,
		PromptVariant:          variant,
		Run:                    run,
		Expectations:           evalCase.Expectations,
		ResponseText:           response.Text,
		Disposition:            evaluationDisposition(response, diagnostics, generateErr),
		SearchRequired:         diagnostics.searchRequired,
		SearchAttempted:        diagnostics.searchAttempted,
		SearchTrigger:          diagnostics.searchTrigger,
		SearchResult:           diagnostics.searchResult,
		GroundingRetryResult:   diagnostics.groundingRetryResult,
		SearchQueryCount:       diagnostics.searchQueryCount,
		ModelCallCount:         diagnostics.modelCalls,
		RetryUsed:              diagnostics.retryUsed,
		SourceCount:            len(response.Sources),
		SupportedSourceCount:   diagnostics.supportedSourceCount,
		GroundingOutcome:       diagnostics.groundingOutcome,
		LatencyMilliseconds:    elapsed.Milliseconds(),
		TerminalFallbackReason: diagnostics.terminalFallbackReason,
	}
	if generateErr != nil {
		record.Error = generateErr.Error()
	}
	record.DeterministicViolations = evaluationViolations(evalCase, record)
	return record
}

func evaluationDisposition(response GenerateResponse, diagnostics generationDiagnostics, err error) string {
	if err != nil {
		return "error"
	}
	if diagnostics.terminalFallbackReason != "" && diagnostics.terminalFallbackReason != terminalFallbackNone {
		return "terminal-fallback"
	}
	if response.Grounded {
		return "grounded-answer"
	}
	if strings.HasPrefix(response.Text, searchEvidenceGapPrefix) {
		return "qualified-answer"
	}
	if strings.Contains(response.Text, "?") && len(response.Text) < 400 {
		return "clarification"
	}
	return "answer"
}

func evaluationViolations(evalCase evaluationCase, record evaluationRecord) []string {
	var violations []string
	if evalCase.Expectations.SourcesRequired && record.SourceCount == 0 {
		violations = append(violations, "sources-required")
	}
	if evalCase.Expectations.SearchAction == "required" && !record.SearchAttempted {
		violations = append(violations, "search-required")
	}
	if evalCase.Expectations.SearchAction == "avoid" && record.SearchAttempted {
		violations = append(violations, "search-should-be-avoided")
	}
	if evalCase.Expectations.ClarificationBehavior == "required" && record.Disposition != "clarification" {
		violations = append(violations, "clarification-required")
	}
	if !containsString(evalCase.Expectations.PermittedDispositions, record.Disposition) {
		violations = append(violations, "disposition-not-permitted")
	}
	lowerResponse := strings.ToLower(record.ResponseText)
	for _, prohibited := range evalCase.Expectations.ProhibitedClaims {
		if strings.HasPrefix(prohibited, "literal:") && strings.Contains(lowerResponse, strings.ToLower(strings.TrimPrefix(prohibited, "literal:"))) {
			violations = append(violations, "prohibited-literal")
		}
	}
	return violations
}

func evaluationFixture(name string) generateFunc {
	calls := 0
	return func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) (*googlegenai.GenerateContentResponse, error) {
		calls++
		if calls == 1 {
			return evaluationResponse("Unsupported current answer.", &googlegenai.GroundingMetadata{WebSearchQueries: []string{"redacted"}}), nil
		}
		switch name {
		case "provider-error":
			return nil, errors.New("injected Search provider failure")
		case "empty-response":
			return &googlegenai.GenerateContentResponse{}, nil
		case "malformed-source":
			return evaluationResponse("The result could not be narrowed beyond stable background.", &googlegenai.GroundingMetadata{
				WebSearchQueries: []string{"redacted"},
				GroundingChunks:  []*googlegenai.GroundingChunk{{Web: &googlegenai.GroundingChunkWeb{URI: "ftp://invalid.example"}}},
			}), nil
		default:
			panic("unknown evaluation fixture: " + name)
		}
	}
}

func evaluationResponse(text string, metadata *googlegenai.GroundingMetadata) *googlegenai.GenerateContentResponse {
	return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{
		Content:           &googlegenai.Content{Role: "model", Parts: []*googlegenai.Part{{Text: text}}},
		GroundingMetadata: metadata,
	}}}
}

type evaluationTool struct {
	name        string
	declaration *googlegenai.FunctionDeclaration
	execute     func() any
	evidence    func(any) Evidence
}

func (tool evaluationTool) Name() string { return tool.name }

func (tool evaluationTool) Declaration() *googlegenai.FunctionDeclaration { return tool.declaration }

func (tool evaluationTool) Execute(context.Context, map[string]any) (any, error) {
	return tool.execute(), nil
}

func (tool evaluationTool) Evidence(result any) (Evidence, bool) { return tool.evidence(result), true }

func evaluationTools(names []string) []FunctionTool {
	var tools []FunctionTool
	for _, name := range names {
		switch name {
		case runtimeContextFunctionName:
			tools = append(tools, evaluationRuntimeTool())
		case ChannelSearchFunctionName:
			tools = append(tools, evaluationChannelSearchTool())
		default:
			panic("unknown evaluation tool: " + name)
		}
	}
	return tools
}

func evaluationRuntimeTool() FunctionTool {
	return evaluationTool{
		name:        runtimeContextFunctionName,
		declaration: &googlegenai.FunctionDeclaration{Name: runtimeContextFunctionName, Description: "Return the evaluation runtime context."},
		execute: func() any {
			now := time.Now().UTC()
			return map[string]string{
				"version":      "evaluation",
				"timezone":     "UTC",
				"current_time": now.Format(time.RFC3339),
				"current_date": now.Format("2006-01-02"),
				"weekday":      now.Weekday().String(),
			}
		},
		evidence: func(result any) Evidence {
			attributes, _ := result.(map[string]string)
			return Evidence{Kind: EvidenceKindRuntimeContext, Tool: runtimeContextFunctionName, Attributes: attributes}
		},
	}
}

func evaluationChannelSearchTool() FunctionTool {
	return evaluationTool{
		name:        ChannelSearchFunctionName,
		declaration: &googlegenai.FunctionDeclaration{Name: ChannelSearchFunctionName, Description: "Search evaluation channel history."},
		execute:     func() any { return map[string]any{"results": []any{}} },
		evidence: func(any) Evidence {
			return Evidence{Kind: EvidenceKindChannelHistory, Tool: ChannelSearchFunctionName}
		},
	}
}

func evaluationOutputPath(variant, subset string) string {
	directory := strings.TrimSpace(os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"))
	if directory == "" {
		directory = os.TempDir()
	}
	return filepath.Join(directory, fmt.Sprintf("search_eval_%s_%s.jsonl", variant, subset))
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
