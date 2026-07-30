package genai

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyAccuracyPolicySelectsDeterministicCapabilities(t *testing.T) {
	tests := []struct {
		name    string
		request string
		want    AccuracyPolicy
	}{
		{name: "runtime", request: "What time is it in America/Los_Angeles?", want: AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName}, RuntimeContextRelevant: true}},
		{name: "identity", request: "What model are you?", want: AccuracyPolicy{ModelIdentityRelevant: true}},
		{name: "channel", request: "Search this channel for deploy.", want: AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}}},
		{name: "channel and web", request: "Search this channel and the web for deploy guidance.", want: AccuracyPolicy{RequiredFunctionNames: []string{ChannelSearchFunctionName}, WebSearchRequired: true}},
		{name: "current", request: "What is the latest stable Go release?", want: AccuracyPolicy{WebSearchRequired: true}},
		{name: "configuration", request: "Disable web search.", want: AccuracyPolicy{}},
		{name: "calculation", request: "Calculate 17 * 29 exactly.", want: AccuracyPolicy{}},
		{name: "standalone arithmetic", request: "What is 10+1?", want: AccuracyPolicy{}},
		{name: "conversion", request: "Convert 10 km to miles.", want: AccuracyPolicy{}},
		{name: "product capacity", request: "Is a Savage Arms A22 Takedown 22 Long Rifle 18in Rimfire Semi Automatic - 10+1 Rounds a fun range gun?", want: AccuracyPolicy{WebSearchRequired: true}},
		{name: "ammunition size", request: "What is a good current price for 7.62x39 ammunition?", want: AccuracyPolicy{WebSearchRequired: true}},
		{name: "greeting", request: "hello", want: AccuracyPolicy{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, ClassifyAccuracyPolicy(test.request))
		})
	}
}

func TestResolveIntentRequestUsesOnlyEllipticalFollowup(t *testing.T) {
	context := IntentContext{CurrentRequest: "What about 9mm?", PreviousUserRequest: "Will AR500 steel work with 7.62?"}
	assert.Equal(t, "Previous request: Will AR500 steel work with 7.62?\nCurrent follow-up: What about 9mm?", ResolveIntentRequest(context))
	context.CurrentRequest = "What is a good 9mm price?"
	assert.Equal(t, context.CurrentRequest, ResolveIntentRequest(context))
}

func TestBroadRecencyClarificationIsBoundedToFirstRequest(t *testing.T) {
	assert.True(t, broadRecencyNeedsClarification([]Message{{Role: "user", Content: "What's happening?"}}))
	assert.False(t, broadRecencyNeedsClarification([]Message{
		{Role: "user", Content: "What's happening?"},
		{Role: "assistant", Content: "Which topic?"},
		{Role: "user", Content: "What's happening?"},
	}))
}

func TestTimezoneClarificationRequiresLocalIANAZone(t *testing.T) {
	policy := ClassifyAccuracyPolicy("What time is it for me?")
	assert.True(t, requiresTimezoneClarification("What time is it for me?", policy))
	assert.False(t, requiresTimezoneClarification("What time is it in Europe/London?", ClassifyAccuracyPolicy("What time is it in Europe/London?")))
}

func TestProvenanceValidationRecognizesSupportingAndConsultedSourceFooters(t *testing.T) {
	policy := AccuracyPolicy{ProvenanceInquiry: true}
	for _, footer := range []string{"-# Sources: [1 · example.com](https://example.com)", "-# Sources consulted: [1 · example.com](https://example.com)", "-# Evidence used: runtime context"} {
		history := "[2026-07-16T00:00:00Z] Jarvis [bot]: It was 5:06 PM PDT.\n" + footer
		assert.Empty(t, accuracyValidationFailure("The recorded claim was 5:06 PM PDT.", "Where did that come from?", history, policy, nil))
	}
}

func TestAccuracyValidationRequiresSuccessfulRuntimeEvidence(t *testing.T) {
	policy := AccuracyPolicy{RequiredFunctionNames: []string{runtimeContextFunctionName}, RuntimeContextRelevant: true}
	assert.Equal(t, "missing_runtime_evidence", accuracyValidationFailure("It is noon UTC.", "What time is it?", "", policy, nil))
	evidence := []Evidence{{Kind: EvidenceKindRuntimeContext, Attributes: map[string]string{
		"current_time": "2026-07-16T18:30:45Z", "timezone": "UTC", "current_date": "2026-07-16", "weekday": "Thursday",
	}}}
	assert.Empty(t, accuracyValidationFailure("It is 18:30 UTC on 2026-07-16, a Thursday.", "What time is it?", "", policy, evidence))
}
