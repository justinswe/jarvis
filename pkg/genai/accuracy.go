package genai

import (
	"regexp"
	"strings"
	"time"

	googlegenai "google.golang.org/genai"
)

const (
	runtimeContextFunctionName = "get_runtime_context"
	// ChannelSearchFunctionName is the model-facing current-channel history tool.
	ChannelSearchFunctionName = "search_current_channel"

	EvidenceKindRuntimeContext = "runtime_context"
	EvidenceKindChannelHistory = "channel_history"
	EvidenceKindCodeExecution  = "code_execution"
	EvidenceKindWeb            = "web"
)

const (
	accuracyFailureFallback       = "I couldn't verify that response against trusted evidence, so I don't want to guess."
	codeExecutionFailureFallback  = "I couldn't verify that calculation with code execution, so I don't want to guess."
	groundingDisabledFallback     = "Web search is disabled for this server, so I can't confirm current details. I can still explain stable background or help narrow the question."
	provenanceFailureFallback     = "No source was preserved for that earlier claim, so I can't verify where it came from."
	runtimeVerificationFallback   = "I couldn't verify the current runtime value, so I don't want to guess."
	channelHistoryFailureFallback = "I couldn't search stored channel history, so I don't want to guess about earlier messages."
	timezoneClarificationFallback = "Which IANA timezone should I use, such as `America/Los_Angeles` or `Europe/London`?"

	accuracyRetryPrompt      = "This is the single accuracy correction. Use only successful tool results and recorded evidence in the supplied conversation. Correct every conflicting runtime value. Never invent an internal clock, source, search, tool call, or evidence. If provenance was not preserved in a Sources or Evidence used footer, say so explicitly."
	codeExecutionRetryPrompt = "This is the single calculation correction. You must use code execution, check its successful result, and base the answer on that result. If code execution does not succeed, do not provide an unverified numeric answer."
)

const (
	searchTriggerNone             = "none"
	searchTriggerExplicit         = "explicit"
	searchTriggerVolatile         = "volatile"
	searchTriggerImplicitVolatile = "implicit-volatile"
	searchTriggerModelOptional    = "model-optional"
)

var (
	quotedTextPattern = regexp.MustCompile("(?s)\"[^\"]*\"|“[^”]*”|`[^`]*`|(?:^|\\s)'[^'\\n]+'(?:$|\\s|[.,!?;:])")
	spacePattern      = regexp.MustCompile(`\s+`)

	provenanceIntentPattern   = regexp.MustCompile(`(?i)\b(?:where did (?:you|that|it).*\b(?:get|come)\b.*\bfrom|what (?:was|is) (?:your|the) source|which source|how do you know(?: that| this)?|source for (?:that|this)|why did you say that)\b`)
	runtimeIntentPattern      = regexp.MustCompile(`(?i)\b(?:what(?:'s| is) (?:the )?(?:current )?(?:time|date|day|weekday|year)|what (?:time|day|date|weekday|year) is (?:it|today)|what day of (?:the )?week is it|current (?:time|date|day|weekday|year)|today'?s date|what version (?:are you|is jarvis)|which jarvis version|jarvis version)\b`)
	runtimeVersionOnlyPattern = regexp.MustCompile(`(?i)^\s*(?:what version are you|what version is jarvis|which jarvis version|what is (?:the )?(?:current )?jarvis version|jarvis version)\s*[?.!]*\s*$`)
	localTimePattern          = regexp.MustCompile(`(?i)\b(?:my|local)\s+(?:time|date|day)\b|\b(?:time|date|day)\b[^.!?]*\b(?:for me|locally)\b`)
	ianaTimezonePattern       = regexp.MustCompile(`\b[A-Z][A-Za-z_+-]*/[A-Z][A-Za-z_+-]+(?:/[A-Z][A-Za-z_+-]+)?\b`)

	explicitSearchPattern    = regexp.MustCompile(`(?i)\b(?:search|browse|look up|lookup|research|verify|fact[ -]?check|cite|find sources?)\b`)
	volatilePattern          = regexp.MustCompile(`(?i)\b(?:current|currently|latest|newest|today|tonight|right now|recent|breaking)\b.*\b(?:officeholder|president|prime minister|governor|mayor|ceo|release|version|price|stock|score|standings|weather|forecast|news|market|election|law|rule|schedule)\b|\b(?:officeholder|president|prime minister|governor|mayor|ceo|release|version|price|stock|score|standings|weather|forecast|news|market|election|law|rule|schedule)\b.*\b(?:current|currently|latest|newest|today|tonight|right now|recent|breaking)\b|\b(?:weather|forecast|news|stock price|sports score|election results?)\b`)
	implicitVolatilePattern  = regexp.MustCompile(`(?i)\bwho (?:is|are) (?:the )?(?:president|prime minister|governor|mayor|ceo|officeholder)\b|\bwhat(?:'s| is) (?:the )?(?:price|score|standings|weather|forecast|news)\b`)
	recencyLanguagePattern   = regexp.MustCompile(`(?i)\b(?:what(?:'s| is) happening|what happened most recently|anything new|catch me up|recent developments|what just happened)\b`)
	broadRecencyPattern      = regexp.MustCompile(`(?i)^\s*(?:what(?:'s| is) happening|what happened most recently|anything new|catch me up|what just happened|what(?:'s| is) new|what(?:'s| is) the latest|(?:tell me about )?recent developments)\s*[?.!]*\s*$`)
	todayPattern             = regexp.MustCompile(`(?i)\b(?:today|tonight)\b`)
	channelScopePattern      = regexp.MustCompile(`(?i)\b(?:this|the|current) channel\b|\bchannel (?:history|messages?)\b`)
	channelLookupPattern     = regexp.MustCompile(`(?i)\b(?:search|find|look up|lookup|what did|who said|when did|where did)\b`)
	historicalMessagePattern = regexp.MustCompile(`(?i)\b(?:earlier|previous|past|older) (?:channel )?messages?\b|\bwhat did\b[^.!?\n]{0,80}\b(?:say|post|write|mention|share)(?:d)?\b[^.!?\n]{0,40}\b(?:here|earlier|before)\b`)
	webScopePattern          = regexp.MustCompile(`(?i)\b(?:web|internet|online|google|websites?|external sources?)\b`)

	computationPattern = regexp.MustCompile(`(?i)\b(?:calculate|compute|evaluate|solve|equation|exact(?:ly)?|statistics?|standard deviation|variance|median|percentile|unit conversion|convert\s+[-+]?\d+(?:\.\d+)?|data analysis|analy[sz]e (?:this )?(?:data|dataset))\b|\b(?:mean|average|sum|correlation|regression)\b[^.!?]*\d|\bhow many\s+(?:millimeters?|centimeters?|meters?|kilometers?|inches?|feet|yards?|miles?|grams?|kilograms?|ounces?|pounds?)\s+(?:are\s+)?in\b|[-+]?\d+(?:\.\d+)?\s*(?:\+|-|\*|/|\^|=)\s*[-+]?\d`)
	numericPattern     = regexp.MustCompile(`\d`)

	internalClockPattern         = regexp.MustCompile(`(?i)\b(?:my|an?|the) internal clock\b|\binternal clock says\b`)
	inventedProvenancePattern    = regexp.MustCompile(`(?i)\b(?:i (?:got|pulled|retrieved) (?:that|it) from|my source (?:was|is)|i (?:used|called|checked|searched|browsed)|according to my (?:search|clock|source))\b`)
	noProvenancePattern          = regexp.MustCompile(`(?i)\b(?:no source (?:was|is)|source was not|wasn't (?:recorded|preserved)|cannot verify where|can't verify where|do not have (?:a )?(?:recorded|preserved) source|don't have (?:a )?(?:recorded|preserved) source)\b`)
	targetedClarificationPattern = regexp.MustCompile(`(?i)^\s*(?:which|what|where|when|who|could you|can you|would you|do you|are you asking)\b[^\n]{0,380}\?\s*$`)

	timeClaimPattern          = regexp.MustCompile(`(?i)\b(?:[01]?\d|2[0-3]):[0-5]\d(?::[0-5]\d)?\s*(?:a\.?m\.?|p\.?m\.?)?(?:\s+(?:UTC|GMT|[ECMP][DS]T))?\b`)
	dateClaimPattern          = regexp.MustCompile(`(?i)\b(?:\d{4}-\d{2}-\d{2}|\d{1,2}/\d{1,2}/\d{4}|(?:January|February|March|April|May|June|July|August|September|October|November|December)\s+\d{1,2},\s+\d{4})\b`)
	yearClaimPattern          = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
	weekdayClaimPattern       = regexp.MustCompile(`(?i)\b(?:Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday)\b`)
	versionClaimPattern       = regexp.MustCompile(`(?i)\bv?\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.-]+)?\b`)
	timezoneClaimPattern      = regexp.MustCompile(`\b(?:UTC|GMT|[ECMP][DS]T|[A-Z][A-Za-z_+-]*/[A-Z][A-Za-z_+-]+(?:/[A-Z][A-Za-z_+-]+)?)\b`)
	currentClaimPattern       = regexp.MustCompile(`(?i)\b(?:current (?:time|date|day|weekday|year)|time (?:is|right now)|today is|today'?s date|right now it(?:'s| is)|jarvis(?:'s)? version|running version)\b`)
	runtimeSubjectPattern     = regexp.MustCompile(`(?i)\b(?:time|date|day|weekday|year|version|when|schedule|meeting)\b`)
	runtimeYearRequestPattern = regexp.MustCompile(`(?i)\b(?:time|date|day|weekday|year)\b`)
	directTimeRequestPattern  = regexp.MustCompile(`(?i)\b(?:what(?:'s| is) (?:the )?(?:current )?time|what time is (?:it|today)|current time|my time|local time|time for me)\b`)
	directDateRequestPattern  = regexp.MustCompile(`(?i)\b(?:what(?:'s| is) (?:the )?(?:current )?date|what date is (?:it|today)|current date|today'?s date)\b`)
	directDayRequestPattern   = regexp.MustCompile(`(?i)\b(?:what(?:'s| is) (?:the )?(?:current )?(?:day|weekday)|what (?:day|weekday) is (?:it|today)|what day of (?:the )?week is it|current (?:day|weekday))\b`)
	directYearRequestPattern  = regexp.MustCompile(`(?i)\b(?:what(?:'s| is) (?:the )?(?:current )?year|what year is (?:it|today)|current year)\b`)
)

// AccuracyPolicy contains deterministic accuracy requirements for one request.
type AccuracyPolicy struct {
	RequiredFunctionNames  []string
	GroundingRequired      bool
	CodeExecutionEnabled   bool
	RuntimeContextRelevant bool
	ProvenanceInquiry      bool
}

// Evidence records safe provenance from a successful tool or provider-managed capability.
type Evidence struct {
	Kind       string
	Tool       string
	Attributes map[string]string
}

// EvidenceProducer creates safe evidence after a successful function execution.
type EvidenceProducer interface {
	Evidence(any) (Evidence, bool)
}

// ClassifyAccuracyPolicy selects deterministic controls from only the current request.
func ClassifyAccuracyPolicy(request string) AccuracyPolicy {
	request = sanitizeText(request)
	if request == "" {
		return AccuracyPolicy{}
	}
	if provenanceIntentPattern.MatchString(request) {
		return AccuracyPolicy{ProvenanceInquiry: true}
	}

	unquoted := quotedTextPattern.ReplaceAllString(request, " ")
	unquoted = spacePattern.ReplaceAllString(unquoted, " ")
	policy := AccuracyPolicy{}
	if runtimeIntentPattern.MatchString(unquoted) || localTimePattern.MatchString(unquoted) {
		policy.RuntimeContextRelevant = true
		policy.RequiredFunctionNames = appendRequiredFunction(policy.RequiredFunctionNames, runtimeContextFunctionName)
	}
	channelHistory := channelHistoryIntent(unquoted)
	if channelHistory {
		policy.RequiredFunctionNames = appendRequiredFunction(policy.RequiredFunctionNames, ChannelSearchFunctionName)
	}
	explicitWebSearch := explicitSearchPattern.MatchString(unquoted) && (!channelHistory || webScopePattern.MatchString(unquoted))
	policy.GroundingRequired = explicitWebSearch || (channelHistory && webScopePattern.MatchString(unquoted)) ||
		volatilePattern.MatchString(unquoted) || implicitVolatilePattern.MatchString(unquoted) || recencyLanguagePattern.MatchString(unquoted)
	if runtimeVersionOnlyPattern.MatchString(unquoted) && !explicitSearchPattern.MatchString(unquoted) {
		policy.GroundingRequired = false
	}
	if todayPattern.MatchString(unquoted) && !policy.RuntimeContextRelevant {
		policy.GroundingRequired = policy.GroundingRequired || !channelHistory
		policy.RuntimeContextRelevant = true
		policy.RequiredFunctionNames = appendRequiredFunction(policy.RequiredFunctionNames, runtimeContextFunctionName)
	}
	policy.CodeExecutionEnabled = computationPattern.MatchString(unquoted)
	if strings.Contains(strings.ToLower(unquoted), "time complexity") && !numericPattern.MatchString(unquoted) {
		policy.GroundingRequired = false
		policy.CodeExecutionEnabled = false
	}
	return policy
}

func classifySearchTrigger(request string, policy AccuracyPolicy, webSearchEnabled bool) string {
	request = sanitizeText(request)
	unquoted := quotedTextPattern.ReplaceAllString(request, " ")
	unquoted = spacePattern.ReplaceAllString(unquoted, " ")
	channelHistory := channelHistoryIntent(unquoted)
	if explicitSearchPattern.MatchString(unquoted) && (!channelHistory || webScopePattern.MatchString(unquoted)) {
		return searchTriggerExplicit
	}
	if volatilePattern.MatchString(unquoted) || recencyLanguagePattern.MatchString(unquoted) {
		return searchTriggerVolatile
	}
	if implicitVolatilePattern.MatchString(unquoted) {
		return searchTriggerImplicitVolatile
	}
	if policy.GroundingRequired {
		return searchTriggerVolatile
	}
	if webSearchEnabled {
		return searchTriggerModelOptional
	}
	return searchTriggerNone
}

func broadRecencyNeedsClarification(messages []Message) bool {
	request := currentRequest(messages)
	if !broadRecencyPattern.MatchString(request) {
		return false
	}

	currentUserFound := false
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		if !currentUserFound {
			currentUserFound = true
			continue
		}
		if broadRecencyPattern.MatchString(sanitizeText(messages[i].Content)) {
			return false
		}
	}
	return !recencyLanguagePattern.MatchString(historicalContext(messages))
}

func isTargetedClarification(text string) bool {
	return targetedClarificationPattern.MatchString(sanitizeText(text))
}

func channelHistoryIntent(request string) bool {
	return historicalMessagePattern.MatchString(request) ||
		(channelScopePattern.MatchString(request) && channelLookupPattern.MatchString(request))
}

func appendRequiredFunction(names []string, name string) []string {
	for _, existing := range names {
		if existing == name {
			return names
		}
	}
	return append(names, name)
}

func mergeAccuracyPolicies(configured, classified AccuracyPolicy) AccuracyPolicy {
	result := configured
	result.RequiredFunctionNames = append([]string(nil), result.RequiredFunctionNames...)
	for _, name := range classified.RequiredFunctionNames {
		result.RequiredFunctionNames = appendRequiredFunction(result.RequiredFunctionNames, name)
	}
	result.GroundingRequired = result.GroundingRequired || classified.GroundingRequired
	result.CodeExecutionEnabled = result.CodeExecutionEnabled || classified.CodeExecutionEnabled
	result.RuntimeContextRelevant = result.RuntimeContextRelevant || classified.RuntimeContextRelevant
	result.ProvenanceInquiry = result.ProvenanceInquiry || classified.ProvenanceInquiry
	if result.ProvenanceInquiry {
		result.RequiredFunctionNames = nil
		result.GroundingRequired = false
		result.CodeExecutionEnabled = false
		result.RuntimeContextRelevant = false
	}
	return result
}

func currentRequest(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		content := sanitizeText(messages[i].Content)
		if index := strings.LastIndex(content, "CURRENT REQUEST:\n"); index >= 0 {
			return strings.TrimSpace(content[index+len("CURRENT REQUEST:\n"):])
		}
		return content
	}
	return ""
}

func requiresTimezoneClarification(request string, policy AccuracyPolicy) bool {
	return policy.RuntimeContextRelevant && localTimePattern.MatchString(request) && !ianaTimezonePattern.MatchString(request)
}

func historicalContext(messages []Message) string {
	request := currentRequest(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		content := sanitizeText(messages[i].Content)
		if index := strings.LastIndex(content, "CURRENT REQUEST:\n"); index >= 0 {
			return strings.TrimSpace(content[:index])
		}
		if content == request {
			return ""
		}
	}
	return ""
}

func accuracyValidationFailure(text, request, history string, policy AccuracyPolicy, evidence []Evidence) string {
	if internalClockPattern.MatchString(text) {
		return "invented_internal_clock"
	}
	if policy.ProvenanceInquiry {
		hasRecordedProvenance := strings.Contains(history, "-# Sources:") || strings.Contains(history, "-# Evidence used:")
		if !hasRecordedProvenance && inventedProvenancePattern.MatchString(text) && !noProvenancePattern.MatchString(text) {
			return "invented_provenance"
		}
		for _, claim := range runtimeClaimLiterals(text) {
			if !hasRecordedProvenance || !strings.Contains(strings.ToLower(history), strings.ToLower(claim)) {
				return "unsupported_provenance_runtime_claim"
			}
		}
		return ""
	}

	if requiredFunction(policy, ChannelSearchFunctionName) {
		if _, ok := evidenceByKind(evidence, EvidenceKindChannelHistory); !ok {
			return "missing_channel_history_evidence"
		}
	}
	runtimeEvidence, hasRuntimeEvidence := evidenceByKind(evidence, EvidenceKindRuntimeContext)
	if policy.RuntimeContextRelevant {
		if !hasRuntimeEvidence {
			return "missing_runtime_evidence"
		}
		if reason := validateRuntimeClaims(text, request, runtimeEvidence.Attributes); reason != "" {
			return reason
		}
		return ""
	}
	if volunteeredRuntimeClaim(text, request) {
		return "unsolicited_runtime_claim"
	}
	return ""
}

func requiredFunction(policy AccuracyPolicy, name string) bool {
	for _, required := range policy.RequiredFunctionNames {
		if required == name {
			return true
		}
	}
	return false
}

func volunteeredRuntimeClaim(text, request string) bool {
	if runtimeSubjectPattern.MatchString(request) {
		return false
	}
	if currentClaimPattern.MatchString(text) {
		return true
	}
	for _, claim := range timeClaimPattern.FindAllString(text, -1) {
		if timezoneClaimPattern.MatchString(claim) && !strings.Contains(strings.ToLower(request), strings.ToLower(claim)) {
			return true
		}
	}
	return false
}

func validateRuntimeClaims(text, request string, attributes map[string]string) string {
	current, err := time.Parse(time.RFC3339, attributes["current_time"])
	if err != nil {
		return "invalid_runtime_evidence"
	}
	if location, locationErr := time.LoadLocation(attributes["timezone"]); locationErr == nil {
		current = current.In(location)
	}
	timeClaims := timeClaimPattern.FindAllString(text, -1)
	if directTimeRequestPattern.MatchString(request) && len(timeClaims) == 0 {
		return "missing_runtime_time_claim"
	}
	for _, claim := range timeClaims {
		if !timeClaimMatches(claim, current) {
			return "runtime_time_mismatch"
		}
	}
	dateClaims := dateClaimPattern.FindAllString(text, -1)
	if directDateRequestPattern.MatchString(request) && len(dateClaims) == 0 {
		return "missing_runtime_date_claim"
	}
	for _, claim := range dateClaims {
		if !dateClaimMatches(claim, current) {
			return "runtime_date_mismatch"
		}
	}
	if runtimeYearRequestPattern.MatchString(request) {
		yearClaims := yearClaimPattern.FindAllString(text, -1)
		if directYearRequestPattern.MatchString(request) && len(yearClaims) == 0 {
			return "missing_runtime_year_claim"
		}
		for _, claim := range yearClaims {
			if claim != current.Format("2006") {
				return "runtime_year_mismatch"
			}
		}
	}
	weekdayClaims := weekdayClaimPattern.FindAllString(text, -1)
	if directDayRequestPattern.MatchString(request) && len(weekdayClaims) == 0 && len(dateClaims) == 0 {
		return "missing_runtime_day_claim"
	}
	for _, claim := range weekdayClaims {
		if !strings.EqualFold(claim, attributes["weekday"]) {
			return "runtime_weekday_mismatch"
		}
	}
	timezoneClaims := timezoneClaimPattern.FindAllString(text, -1)
	if directTimeRequestPattern.MatchString(request) && len(timezoneClaims) == 0 {
		return "missing_runtime_timezone_claim"
	}
	for _, claim := range timezoneClaims {
		zone, _ := current.Zone()
		if !strings.EqualFold(claim, attributes["timezone"]) && !strings.EqualFold(claim, zone) && !(claim == "GMT" && zone == "UTC") {
			return "runtime_timezone_mismatch"
		}
	}
	versionClaims := []string(nil)
	if strings.Contains(strings.ToLower(request), "version") || strings.Contains(strings.ToLower(text), "version") {
		versionClaims = versionClaimPattern.FindAllString(text, -1)
	}
	if strings.Contains(strings.ToLower(request), "version") && len(versionClaims) == 0 {
		return "missing_runtime_version_claim"
	}
	if version := strings.TrimSpace(attributes["version"]); version != "" {
		for _, claim := range versionClaims {
			if strings.TrimPrefix(strings.ToLower(claim), "v") != strings.TrimPrefix(strings.ToLower(version), "v") {
				return "runtime_version_mismatch"
			}
		}
	} else if len(versionClaims) > 0 {
		return "runtime_version_mismatch"
	}
	return ""
}

func runtimeClaimLiterals(text string) []string {
	var claims []string
	for _, pattern := range []*regexp.Regexp{timeClaimPattern, dateClaimPattern, weekdayClaimPattern, versionClaimPattern} {
		claims = append(claims, pattern.FindAllString(text, -1)...)
	}
	return claims
}

func dateClaimMatches(claim string, current time.Time) bool {
	for _, layout := range []string{"2006-01-02", "1/2/2006", "January 2, 2006"} {
		parsed, err := time.Parse(layout, claim)
		if err == nil {
			return parsed.Year() == current.Year() && parsed.YearDay() == current.YearDay()
		}
	}
	return false
}

func timeClaimMatches(claim string, current time.Time) bool {
	normalized := strings.ToUpper(strings.TrimSpace(claim))
	fields := strings.Fields(normalized)
	if len(fields) > 1 {
		last := fields[len(fields)-1]
		if timezoneClaimPattern.MatchString(last) {
			normalized = strings.Join(fields[:len(fields)-1], " ")
		}
	}
	normalized = strings.ReplaceAll(normalized, ".", "")
	var parsed time.Time
	var err error
	for _, layout := range []string{"15:04:05", "15:04", "3:04:05 PM", "3:04 PM"} {
		parsed, err = time.Parse(layout, normalized)
		if err == nil {
			break
		}
	}
	if err != nil || parsed.Hour() != current.Hour() || parsed.Minute() != current.Minute() {
		return false
	}
	if strings.Count(normalized, ":") == 2 && parsed.Second() != current.Second() {
		return false
	}
	return true
}

func responseHasSuccessfulCodeExecution(resp *googlegenai.GenerateContentResponse) bool {
	if resp == nil {
		return false
	}
	for _, candidate := range resp.Candidates {
		if candidate == nil || candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part != nil && part.CodeExecutionResult != nil && part.CodeExecutionResult.Outcome == googlegenai.OutcomeOK {
				return true
			}
		}
	}
	return false
}

func evidenceByKind(evidence []Evidence, kind string) (Evidence, bool) {
	for i := len(evidence) - 1; i >= 0; i-- {
		if evidence[i].Kind == kind {
			return evidence[i], true
		}
	}
	return Evidence{}, false
}

func uniqueEvidence(evidence []Evidence) []Evidence {
	seen := make(map[string]struct{}, len(evidence))
	result := make([]Evidence, 0, len(evidence))
	for _, item := range evidence {
		key := item.Kind + "\x00" + item.Tool
		if _, ok := seen[key]; ok || strings.TrimSpace(item.Kind) == "" {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

func evidenceKinds(evidence []Evidence) []string {
	kinds := make([]string, 0, len(evidence))
	for _, item := range uniqueEvidence(evidence) {
		kinds = append(kinds, item.Kind)
	}
	return kinds
}

func accuracyFallback(policy AccuracyPolicy, failure string) string {
	switch {
	case policy.ProvenanceInquiry:
		return provenanceFailureFallback
	case failure == "missing_channel_history_evidence":
		return channelHistoryFailureFallback
	case strings.HasPrefix(failure, "missing_runtime") || strings.HasPrefix(failure, "runtime_") || failure == "invalid_runtime_evidence":
		return runtimeVerificationFallback
	default:
		return accuracyFailureFallback
	}
}
