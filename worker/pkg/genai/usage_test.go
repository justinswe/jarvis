package genai

import (
	"context"
	"errors"
	"testing"

	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUsageRecorder struct {
	reports []UsageReport
}

func (r *fakeUsageRecorder) RecordUsage(report UsageReport) { r.reports = append(r.reports, report) }

// usageResponse builds one model round carrying token accounting and an upstream model identity.
func usageResponse(text, actualModelID string, usage llm.Usage) llm.Response {
	return llm.Response{
		Message:  llm.TextMessage(llm.RoleAssistant, text),
		Usage:    usage,
		Metadata: llm.ResponseMetadata{ActualModelID: actualModelID},
	}
}

// reportFor finds the recorded report for one model identifier.
func reportFor(t *testing.T, reports []UsageReport, modelID string) UsageReport {
	t.Helper()
	for _, report := range reports {
		if report.ModelID == modelID {
			return report
		}
	}
	t.Fatalf("no usage report for model %q", modelID)
	return UsageReport{}
}

func TestUsageRecorderReportsOneRoundWithGuildAttribution(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{
		usageResponse("final", "upstream/primary", llm.Usage{InputTokens: 20, OutputTokens: 7, ReasoningTokens: 1, TotalTokens: 28}),
	}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderGoogleAI, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	recorder := &fakeUsageRecorder{}
	handler.cfg.UsageRecorder = recorder

	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		GuildID:  "guild-1",
		Tier:     "pro",
		Config:   &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	require.Len(t, recorder.reports, 1)

	report := recorder.reports[0]
	assert.Equal(t, "guild-1", report.GuildID)
	assert.Equal(t, "pro", report.Tier)
	assert.Equal(t, string(llm.ProviderGoogleAI), report.Provider)
	assert.Equal(t, "upstream/primary", report.ModelID)
	assert.Equal(t, len(primary.requests), report.Calls)
	assert.Equal(t, 20, report.Usage.InputTokens)
	assert.Equal(t, 7, report.Usage.OutputTokens)
	assert.Equal(t, 1, report.Usage.ReasoningTokens)
	assert.Equal(t, 28, report.Usage.TotalTokens)
}

func TestUsageRecorderSplitsTokensByRespondingModelAcrossFallback(t *testing.T) {
	primary := &scriptedHost{
		responses: []llm.Response{usageResponse("A useful partial answer.", "upstream/primary", llm.Usage{InputTokens: 11, TotalTokens: 11})},
		errors:    []error{&llm.Error{Kind: llm.ErrorService, Provider: llm.ProviderOpenRouter, StatusCode: 502, Err: errors.New("upstream detail")}},
	}
	fallback := &scriptedHost{responses: []llm.Response{
		usageResponse("A recovered answer.", "upstream/fallback", llm.Usage{InputTokens: 4, OutputTokens: 9, TotalTokens: 13}),
	}}
	profiles := []llm.Profile{
		{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "primary-model"},
		{Name: "fallback", Provider: llm.ProviderNVIDIANIM, ModelID: "fallback-model"},
	}
	handler := neutralHandler(t, profiles, map[string]llm.Host{"primary": primary, "fallback": fallback},
		llm.Selection{Primary: "primary", Fallback: "fallback"})
	recorder := &fakeUsageRecorder{}
	handler.cfg.UsageRecorder = recorder

	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		GuildID:  "guild-1",
		Config:   &RequestConfig{MaxOutputTokens: 256, PrimaryModelProfile: "primary", FallbackModelProfile: "fallback"},
	})
	require.NoError(t, err)
	require.Len(t, recorder.reports, 2)

	primaryReport := reportFor(t, recorder.reports, "upstream/primary")
	assert.Equal(t, string(llm.ProviderOpenRouter), primaryReport.Provider)
	assert.Equal(t, 11, primaryReport.Usage.InputTokens)

	fallbackReport := reportFor(t, recorder.reports, "upstream/fallback")
	assert.Equal(t, string(llm.ProviderNVIDIANIM), fallbackReport.Provider)
	assert.Equal(t, 4, fallbackReport.Usage.InputTokens)
	assert.Equal(t, 9, fallbackReport.Usage.OutputTokens)
}

func TestUsageRecorderSkipsRequestsWithoutGuild(t *testing.T) {
	primary := &scriptedHost{responses: []llm.Response{
		usageResponse("hello", "upstream/primary", llm.Usage{InputTokens: 3, TotalTokens: 3}),
	}}
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderGoogleAI, ModelID: "model"}
	handler := neutralHandler(t, []llm.Profile{profile}, map[string]llm.Host{"primary": primary}, llm.Selection{Primary: "primary"})
	recorder := &fakeUsageRecorder{}
	handler.cfg.UsageRecorder = recorder

	_, err := handler.Generate(context.Background(), GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}}, Config: &RequestConfig{MaxOutputTokens: 256},
	})
	require.NoError(t, err)
	assert.Empty(t, recorder.reports)
}

func TestRecordRoundFallsBackThroughModelIdentities(t *testing.T) {
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderVertex, ModelID: "profile-model"}
	tests := []struct {
		name     string
		response llm.Response
		want     string
	}{
		{name: "upstream identity wins", response: usageResponse("", "upstream/model", llm.Usage{}), want: "upstream/model"},
		{name: "response model id", response: llm.Response{ModelID: "response-model"}, want: "response-model"},
		{name: "profile model id", response: llm.Response{}, want: "profile-model"},
		{name: "separator sanitized", response: llm.Response{ModelID: "vendor|model"}, want: "vendor_model"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace := neutralOrchestrationTrace{}
			trace.recordRound(profile, test.response)
			require.Len(t, trace.roundUsage, 1)
			for key := range trace.roundUsage {
				assert.Equal(t, test.want, key.modelID)
			}
		})
	}
}

func TestRecordRoundTruncatesOversizedModelIdentifiers(t *testing.T) {
	oversized := make([]byte, maxModelIDBytes+40)
	for index := range oversized {
		oversized[index] = 'm'
	}
	trace := neutralOrchestrationTrace{}
	trace.recordRound(llm.Profile{Provider: llm.ProviderVertex}, llm.Response{ModelID: string(oversized)})
	for key := range trace.roundUsage {
		assert.Len(t, key.modelID, maxModelIDBytes)
	}
}
