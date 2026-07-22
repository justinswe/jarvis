// Package llm defines provider-neutral model hosting contracts.
package llm

import (
	"context"
	"encoding/json"
	"strings"
)

// Provider identifies a model API.
type Provider string

const (
	ProviderGoogleAI   Provider = "google-ai"
	ProviderVertex     Provider = "vertex"
	ProviderNVIDIANIM  Provider = "nvidia-nim"
	ProviderOpenRouter Provider = "openrouter"
)

// Role identifies the author of a conversation turn.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// NormalizeRole accepts the legacy Vertex name for assistant turns.
func NormalizeRole(role string) (Role, bool) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case string(RoleSystem):
		return RoleSystem, true
	case string(RoleUser):
		return RoleUser, true
	case string(RoleAssistant), "model":
		return RoleAssistant, true
	case string(RoleTool):
		return RoleTool, true
	default:
		return "", false
	}
}

// Image is inline image input.
type Image struct {
	Data     []byte
	MIMEType string
}

// Part is one visible text or image message part.
type Part struct {
	Text  string
	Image *Image
}

// Message is a provider-neutral conversation turn.
type Message struct {
	Role         Role
	Parts        []Part
	ToolCalls    []ToolCall
	ToolResult   *ToolResult
	Continuation *ContinuationState
}

// ContinuationState is opaque assistant-turn state owned by a host adapter.
// Its payload is deliberately inaccessible outside this package so reasoning
// signatures cannot leak into logs or provider-neutral orchestration policy.
type ContinuationState struct {
	provider    Provider
	profileName string
	modelID     string
	format      string
	payload     []byte
}

// ReusableWith reports whether state may be replayed to the exact profile that
// produced it. Provider/model equality alone is insufficient for opaque state.
func (s *ContinuationState) ReusableWith(profile Profile) bool {
	return s != nil && s.provider == profile.Provider && s.profileName == profile.Name && s.modelID == profile.ModelID && len(s.payload) > 0
}

func newContinuationState(profile Profile, format string, payload []byte) *ContinuationState {
	if format == "" || len(payload) == 0 {
		return nil
	}
	return &ContinuationState{
		provider: profile.Provider, profileName: profile.Name, modelID: profile.ModelID,
		format: format, payload: append([]byte(nil), payload...),
	}
}

func (s *ContinuationState) decode(profile Profile, format string, target any) bool {
	if !s.ReusableWith(profile) || s.format != format {
		return false
	}
	return json.Unmarshal(s.payload, target) == nil
}

// Text returns the concatenated visible text parts.
func (m Message) Text() string {
	var builder strings.Builder
	for _, part := range m.Parts {
		builder.WriteString(part.Text)
	}
	return builder.String()
}

// TextMessage creates a one-part text message.
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Parts: []Part{{Text: text}}}
}

// ReasoningEffort requests a neutral reasoning level.
type ReasoningEffort string

const (
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// ToolEffect describes whether a tool may change external state.
type ToolEffect string

const (
	ToolEffectReadOnly ToolEffect = "read-only"
	ToolEffectMutation ToolEffect = "mutation"
)

// JSONSchema is a provider-neutral JSON Schema object.
type JSONSchema map[string]any

// ToolDefinition describes a model-callable function.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema JSONSchema
	Effect      ToolEffect
}

// ToolCall is a model request to invoke a function.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// ToolResult records one completed function call.
type ToolResult struct {
	CallID string
	Name   string
	Output any
	Error  *ToolResultError
}

// ToolResultError is safe to expose to a model.
type ToolResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MarshalOutput produces a stable JSON representation for compatible APIs.
func (r ToolResult) MarshalOutput() ([]byte, error) {
	if r.Error != nil {
		return json.Marshal(map[string]any{"error": r.Error})
	}
	return json.Marshal(map[string]any{"output": r.Output})
}

// Capabilities are capabilities confirmed by provider metadata.
type Capabilities struct {
	Tools             bool
	ToolChoice        bool
	Images            bool
	Reasoning         bool
	ReasoningControls bool
	ContextTokens     int
	MaxInputTokens    int
	MaxOutputTokens   int
}

// Profile binds an operator-defined name to one provider model.
type Profile struct {
	Name         string
	Provider     Provider
	ModelID      string
	Capabilities Capabilities
}

// ToolsEnabled reports whether the profile supports provider-neutral tool routing.
func (p Profile) ToolsEnabled() bool {
	return p.Capabilities.Tools && p.Capabilities.ToolChoice
}

// ToolChoiceMode controls provider-neutral function selection.
type ToolChoiceMode string

const (
	ToolChoiceAutomatic ToolChoiceMode = "automatic"
	ToolChoiceRequired  ToolChoiceMode = "required"
	ToolChoiceDisabled  ToolChoiceMode = "disabled"
	ToolChoiceFunction  ToolChoiceMode = "function"
)

// ToolChoice selects automatic, required, disabled, or one specifically required function.
type ToolChoice struct {
	Mode         ToolChoiceMode
	FunctionName string
}

// Request is one model round. Tool execution belongs to the caller.
type Request struct {
	Profile         Profile
	System          string
	Messages        []Message
	MaxOutputTokens int
	ReasoningEffort ReasoningEffort
	Tools           []ToolDefinition
	ToolChoice      ToolChoice
}

func effectiveMaxOutputTokens(profile Profile, requested int) int {
	result := requested
	if limit := profile.Capabilities.MaxOutputTokens; limit > 0 && (result <= 0 || result > limit) {
		result = limit
	}
	if contextLimit := profile.Capabilities.ContextTokens; contextLimit > 1 && result >= contextLimit {
		result = contextLimit - 1
	}
	return result
}

// FinishMetadata describes why one model round ended.
type FinishMetadata struct {
	Reason       string
	NativeReason string
	Blocked      bool
}

// Usage reports provider token accounting when available.
type Usage struct {
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	TotalTokens     int
}

// ResponseMetadata identifies the configured route and the safe upstream responder.
type ResponseMetadata struct {
	ConfiguredProvider Provider
	ConfiguredProfile  string
	ActualModelID      string
	ProviderRequestID  string
	UpstreamProvider   string
	RoutingAttempt     int
	RoutingAttempts    []RoutingAttemptMetadata
	PipelineStages     []string
}

// RoutingAttemptMetadata identifies one safe OpenRouter upstream attempt.
type RoutingAttemptMetadata struct {
	Provider string
	Model    string
	Status   int
}

// Response is one provider-neutral model round.
type Response struct {
	Message  Message
	Finish   FinishMetadata
	Usage    Usage
	ModelID  string
	Metadata ResponseMetadata
}

// Text returns visible assistant text.
func (r Response) Text() string { return r.Message.Text() }

// Host performs one model round.
type Host interface {
	Generate(context.Context, Request) (Response, error)
}

// Prober validates model availability and returns confirmed capabilities.
type Prober interface {
	Probe(context.Context, Profile) (Capabilities, error)
}
