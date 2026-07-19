package genai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/justinswe/jarvis/pkg/llm"
	"github.com/justinswe/jarvis/pkg/websearch"
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
	Run                     int                    `json:"run"`
	Expectations            evaluationExpectations `json:"expectations"`
	ResponseText            string                 `json:"response_text,omitempty"`
	Error                   string                 `json:"error,omitempty"`
	Disposition             string                 `json:"disposition"`
	SearchRequired          bool                   `json:"search_required"`
	SearchAttempted         bool                   `json:"search_attempted"`
	SearchInvocationCount   int                    `json:"search_invocation_count"`
	SearchProviderCallCount int                    `json:"search_provider_call_count"`
	PrimaryProvider         websearch.Provider     `json:"primary_provider,omitempty"`
	RecoveryProvider        websearch.Provider     `json:"recovery_provider,omitempty"`
	SearchTrigger           string                 `json:"search_trigger"`
	SearchResult            string                 `json:"search_result"`
	RecoveryResult          string                 `json:"recovery_result"`
	ModelCallCount          int                    `json:"model_call_count"`
	RetryUsed               bool                   `json:"retry_used"`
	ReturnedResultCount     int                    `json:"returned_result_count"`
	AcceptedResultCount     int                    `json:"accepted_result_count"`
	MissingURLCount         int                    `json:"missing_url_count"`
	InvalidURLCount         int                    `json:"invalid_url_count"`
	DuplicateURLCount       int                    `json:"duplicate_url_count"`
	MissingSnippetCount     int                    `json:"missing_snippet_count"`
	ResponseBodyBytes       int                    `json:"response_body_bytes"`
	HTTPStatus              int                    `json:"http_status"`
	ErrorKind               websearch.ErrorKind    `json:"error_kind,omitempty"`
	RetryAfterMilliseconds  int64                  `json:"retry_after_milliseconds"`
	SearchLatencyMillis     int64                  `json:"search_latency_milliseconds"`
	ParserOutcome           string                 `json:"parser_outcome"`
	SourceAvailable         bool                   `json:"source_available"`
	SourceAvailability      string                 `json:"source_availability"`
	SourceCount             int                    `json:"source_count"`
	EvidenceStatus          EvidenceStatus         `json:"evidence_status"`
	EvidenceStatuses        []EvidenceStatus       `json:"evidence_statuses,omitempty"`
	LatencyMilliseconds     int64                  `json:"latency_milliseconds"`
	TerminalFallbackReason  string                 `json:"terminal_fallback_reason"`
	DeterministicViolations []string               `json:"deterministic_violations,omitempty"`
}

// TestLiveSearchEvaluation runs the opt-in provider-neutral Search evaluation corpus.
func TestLiveSearchEvaluation(t *testing.T) {
	subset := strings.TrimSpace(manualTestOptions.evalSubset)
	if subset != "development" && subset != "full" {
		t.Fatalf("JARVIS_EVAL_SUBSET must be %q or %q", "development", "full")
	}
	if manualTestOptions.evalRuns < 1 {
		t.Fatal("JARVIS_EVAL_RUNS must be a positive integer")
	}
	if len(manualTestOptions.evalModelProfiles) == 0 || strings.TrimSpace(manualTestOptions.evalPrimaryModelProfile) == "" {
		t.Skip("set JARVIS_EVAL_MODEL_PROFILE and JARVIS_EVAL_PRIMARY_MODEL_PROFILE to run live evaluation")
	}
	clients := evaluationSearchClients(t)
	if len(clients) == 0 {
		t.Skip("set JARVIS_EVAL_WEB_SEARCH_PROVIDERS and newly rotated provider credentials")
	}
	h, err := New(context.Background(), Config{
		ProjectID: strings.TrimSpace(manualTestOptions.evalProjectID), Location: strings.TrimSpace(manualTestOptions.evalLocation),
		GoogleAIAPIKey:   manualTestOptions.googleAIAPIKey,
		NVIDIAAPIKey:     manualTestOptions.nvidiaAPIKey,
		OpenRouterAPIKey: manualTestOptions.openRouterAPIKey, OpenRouterBaseURL: manualTestOptions.openRouterBaseURL,
		ModelProfiles: manualTestOptions.evalModelProfiles, PrimaryModelProfile: manualTestOptions.evalPrimaryModelProfile,
		FallbackModelProfile: manualTestOptions.evalFallbackModelProfile,
		MaxOutputTokens:      DefaultMaxOutputTokens, WebSearchClients: clients,
	})
	if err != nil {
		t.Fatalf("create evaluation handler: %v", err)
	}
	defer h.Close()
	liveSearchers := append([]webSearcher(nil), h.webSearchers...)

	outputPath := evaluationOutputPath(t, subset)
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatalf("create evaluation output: %v", err)
	}
	defer output.Close()
	encoder := json.NewEncoder(output)

	for run := 1; run <= manualTestOptions.evalRuns; run++ {
		for _, evalCase := range loadEvaluationCases(t, subset) {
			h.webSearchers = liveSearchers
			if evalCase.Fixture != "" {
				h.webSearchers = []webSearcher{evaluationFixture(evalCase.Fixture)}
			}
			var diagnostics generationDiagnostics
			h.observeGeneration = func(got generationDiagnostics) { diagnostics = got }
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			started := time.Now()
			response, generateErr := h.Generate(ctx, GenerateRequest{
				Messages: evalCase.Messages, Tools: evaluationTools(evalCase.Tools),
				Config: &RequestConfig{MaxOutputTokens: DefaultMaxOutputTokens, WebSearchEnabled: evalCase.WebSearchEnabled, ReasoningEffort: llm.ReasoningMedium},
			})
			cancel()
			record := newEvaluationRecord(evalCase, run, response, diagnostics, generateErr, time.Since(started))
			if err := encoder.Encode(record); err != nil {
				t.Fatalf("write evaluation output: %v", err)
			}
		}
	}
}

func evaluationSearchClients(t *testing.T) []*websearch.Client {
	t.Helper()
	var clients []*websearch.Client
	seen := make(map[websearch.Provider]struct{})
	for index, name := range strings.Split(manualTestOptions.evalWebSearchProviders, ",") {
		provider := websearch.Provider(strings.TrimSpace(name))
		if provider == "" {
			continue
		}
		if !websearch.SupportedProvider(provider) || len(clients) == 2 {
			t.Fatalf("invalid JARVIS_EVAL_WEB_SEARCH_PROVIDERS")
		}
		if _, duplicate := seen[provider]; duplicate {
			t.Fatalf("duplicate evaluation provider %q", provider)
		}
		if provider == websearch.ProviderSerper && index != 0 {
			t.Fatal("serper must be the first evaluation provider")
		}
		seen[provider] = struct{}{}
		key := map[websearch.Provider]string{
			websearch.ProviderSerper: manualTestOptions.serperAPIKey, websearch.ProviderFirecrawl: manualTestOptions.firecrawlAPIKey,
			websearch.ProviderTavily: manualTestOptions.tavilyAPIKey,
		}[provider]
		client, err := websearch.New(websearch.Config{Provider: provider, APIKey: key})
		if err != nil {
			t.Fatalf("create evaluation provider %q: %v", provider, err)
		}
		clients = append(clients, client)
	}
	return clients
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
		if subset != "development" || evalCase.Development {
			cases = append(cases, evalCase)
		}
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
	path, err := runfiles.Rlocation("jarvis/" + evaluationCorpusPath)
	if err == nil {
		return path
	}
	return evaluationCorpusPath
}

func newEvaluationRecord(evalCase evaluationCase, run int, response GenerateResponse, diagnostics generationDiagnostics, generateErr error, elapsed time.Duration) evaluationRecord {
	record := evaluationRecord{
		CaseID: evalCase.ID, Category: evalCase.Category, Run: run, Expectations: evalCase.Expectations,
		ResponseText: response.Text, Disposition: evaluationDisposition(response, diagnostics, generateErr),
		SearchRequired: diagnostics.searchRequired, SearchAttempted: diagnostics.searchAttempted,
		SearchInvocationCount: diagnostics.searchInvocationCount, SearchProviderCallCount: diagnostics.searchProviderCalls,
		PrimaryProvider: diagnostics.primaryProvider, RecoveryProvider: diagnostics.recoveryProvider,
		SearchTrigger: diagnostics.searchTrigger, SearchResult: diagnostics.searchResult, RecoveryResult: diagnostics.recoveryResult,
		ModelCallCount: diagnostics.modelCalls, RetryUsed: diagnostics.retryUsed,
		ReturnedResultCount: diagnostics.returnedResultCount, AcceptedResultCount: diagnostics.validSourceCount,
		MissingURLCount: diagnostics.missingURLCount, InvalidURLCount: diagnostics.invalidURLCount,
		DuplicateURLCount: diagnostics.duplicateURLCount, MissingSnippetCount: diagnostics.missingSnippetCount,
		ResponseBodyBytes: diagnostics.responseBodyBytes, HTTPStatus: diagnostics.httpStatus, ErrorKind: diagnostics.errorKind,
		RetryAfterMilliseconds: diagnostics.retryAfter.Milliseconds(), SearchLatencyMillis: diagnostics.searchLatency.Milliseconds(),
		ParserOutcome: diagnostics.parserOutcome, SourceAvailable: diagnostics.sourceAvailable,
		SourceAvailability: diagnostics.sourceAvailability, SourceCount: len(response.Sources), EvidenceStatus: response.EvidenceStatus,
		EvidenceStatuses: append([]EvidenceStatus(nil), response.EvidenceStatuses...), LatencyMilliseconds: elapsed.Milliseconds(),
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
	if len(response.Sources) > 0 {
		return "sourced-answer"
	}
	if knownEvidenceStatus(response.EvidenceStatus) || hasKnownEvidenceStatus(response.EvidenceStatuses) {
		return "qualified-answer"
	}
	if strings.Contains(response.Text, "?") && len(response.Text) < 400 {
		return "clarification"
	}
	return "answer"
}

func hasKnownEvidenceStatus(statuses []EvidenceStatus) bool {
	for _, status := range statuses {
		if knownEvidenceStatus(status) {
			return true
		}
	}
	return false
}

func knownEvidenceStatus(status EvidenceStatus) bool {
	switch status {
	case EvidenceStatusWebUnconfirmed, EvidenceStatusRuntimeUnconfirmed, EvidenceStatusChannelUnconfirmed, EvidenceStatusGeneralUnconfirmed:
		return true
	default:
		return false
	}
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

type evaluationSearchFixture string

func (fixture evaluationSearchFixture) Provider() websearch.Provider { return websearch.ProviderSerper }

func (fixture evaluationSearchFixture) Search(context.Context, string) (websearch.Response, error) {
	switch fixture {
	case "provider-error":
		return websearch.Response{}, &websearch.Error{Kind: websearch.ErrorService, Provider: websearch.ProviderSerper, StatusCode: 503}
	case "empty-response":
		return websearch.Response{}, nil
	case "malformed-source":
		return websearch.Response{Diagnostics: websearch.Diagnostics{ReturnedResults: 1, InvalidURLResults: 1, ParserOutcome: "ok"}}, nil
	default:
		panic("unknown evaluation fixture: " + fixture)
	}
}

func evaluationFixture(name string) webSearcher { return evaluationSearchFixture(name) }

func TestEvaluationFixturesApplyOnEverySearchAttempt(t *testing.T) {
	for _, name := range []string{"provider-error", "empty-response", "malformed-source"} {
		t.Run(name, func(t *testing.T) {
			fixture := evaluationFixture(name)
			for attempt := 1; attempt <= 2; attempt++ {
				response, err := fixture.Search(context.Background(), "query")
				if name == "provider-error" && err == nil {
					t.Fatal("provider-error fixture returned no error")
				}
				if name != "provider-error" && (err != nil || len(response.Results) != 0) {
					t.Fatalf("fixture call = (%+v, %v); want empty normalized results", response, err)
				}
			}
		})
	}
}

type evaluationHost func(context.Context, llm.Request) (llm.Response, error)

func (host evaluationHost) Generate(ctx context.Context, request llm.Request) (llm.Response, error) {
	return host(ctx, request)
}

func TestFailure002RemainsQualifiedAfterBoundedSearchFailure(t *testing.T) {
	var evalCase evaluationCase
	for _, candidate := range loadEvaluationCases(t, "full") {
		if candidate.ID == "failure-002" {
			evalCase = candidate
			break
		}
	}
	host := evaluationHost(func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Message: llm.TextMessage(llm.RoleAssistant, "I couldn't confirm the current stable Go release from usable web sources.")}, nil
	})
	profile := llm.Profile{
		Name: "default", Provider: llm.ProviderVertex, ModelID: "presentation",
		Capabilities: llm.Capabilities{Tools: true, ToolChoice: true},
	}
	registry, err := llm.NewRegistry([]llm.Profile{profile}, map[string]llm.Host{"default": host}, llm.Selection{Primary: "default"})
	if err != nil {
		t.Fatalf("create evaluation registry: %v", err)
	}
	handler := &Handler{cfg: Config{MaxOutputTokens: 256}, registry: registry, webSearchers: []webSearcher{evaluationFixture(evalCase.Fixture)}}
	var observed []generationDiagnostics
	handler.observeGeneration = func(diagnostics generationDiagnostics) { observed = append(observed, diagnostics) }
	response, generateErr := handler.Generate(context.Background(), GenerateRequest{
		Messages: evalCase.Messages, Config: &RequestConfig{MaxOutputTokens: 256, WebSearchEnabled: evalCase.WebSearchEnabled},
	})
	if generateErr != nil || len(observed) != 1 {
		t.Fatalf("failure-002 generation = (%+v, %v), observer calls %d", response, generateErr, len(observed))
	}
	diagnostics := observed[0]
	if !diagnostics.searchAttempted || diagnostics.searchProviderCalls != 2 || diagnostics.modelCalls != 1 {
		t.Fatalf("failure-002 diagnostics = %+v; want one logical Search, two provider calls, and one model call", diagnostics)
	}
	if diagnostics.sourceAvailable || len(response.Sources) != 0 || versionClaimPattern.MatchString(response.Text) {
		t.Fatalf("failure-002 returned unsupported current detail: (%+v, %+v)", diagnostics, response)
	}
	record := newEvaluationRecord(evalCase, 1, response, diagnostics, nil, diagnostics.duration)
	if record.Disposition != "qualified-answer" || len(record.DeterministicViolations) != 0 {
		t.Fatalf("failure-002 record = %+v", record)
	}
}

type evaluationTool struct {
	name        string
	declaration *llm.ToolDefinition
	execute     func() any
	evidence    func(any) Evidence
}

func (tool evaluationTool) Name() string                     { return tool.name }
func (tool evaluationTool) Declaration() *llm.ToolDefinition { return tool.declaration }
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
		declaration: &llm.ToolDefinition{Name: runtimeContextFunctionName, Description: "Return the evaluation runtime context.", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectReadOnly},
		execute: func() any {
			now := time.Now().UTC()
			return map[string]string{"version": "evaluation", "timezone": "UTC", "current_time": now.Format(time.RFC3339), "current_date": now.Format("2006-01-02"), "weekday": now.Weekday().String()}
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
		declaration: &llm.ToolDefinition{Name: ChannelSearchFunctionName, Description: "Search evaluation channel history.", InputSchema: llm.JSONSchema{"type": "object"}, Effect: llm.ToolEffectReadOnly},
		execute:     func() any { return map[string]any{"results": []any{}} },
		evidence:    func(any) Evidence { return Evidence{Kind: EvidenceKindChannelHistory, Tool: ChannelSearchFunctionName} },
	}
}

func evaluationOutputPath(t *testing.T, subset string) string {
	t.Helper()
	directory := strings.TrimSpace(manualTestOptions.evalOutputDirectory)
	if directory == "" {
		directory = t.TempDir()
	}
	return filepath.Join(directory, fmt.Sprintf("search_eval_%s.jsonl", subset))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
