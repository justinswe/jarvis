package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/justinswe/std/errors"
)

// ErrorKind classifies model failures for fallback decisions.
type ErrorKind string

const (
	ErrorTimeout        ErrorKind = "timeout"
	ErrorRateLimit      ErrorKind = "rate-limit"
	ErrorService        ErrorKind = "service"
	ErrorMalformed      ErrorKind = "malformed"
	ErrorEmptyResponse  ErrorKind = "empty-response"
	ErrorInvalidOutput  ErrorKind = "invalid-output"
	ErrorAuthentication ErrorKind = "authentication"
	ErrorAuthorization  ErrorKind = "authorization"
	ErrorQuota          ErrorKind = "quota"
	ErrorCanceled       ErrorKind = "canceled"
	ErrorInvalidRequest ErrorKind = "invalid-request"
	ErrorContextLimit   ErrorKind = "context-limit"
	ErrorSafety         ErrorKind = "safety"
)

// FallbackHint records a provider-safe remediation for an otherwise permanent error.
type FallbackHint string

const (
	// FallbackDifferentProvider permits one guarded compatibility retry on another provider.
	FallbackDifferentProvider FallbackHint = "different-provider"
)

// RequestDiagnostics describes a request without retaining its content.
type RequestDiagnostics struct {
	RequestShapeVersion      string
	TokenLimitField          string
	EffectiveMaxOutputTokens int
	ReasoningRequested       bool
	ReasoningSent            bool
	PayloadBytes             int
	MessageCount             int
	SystemMessageCount       int
	UserMessageCount         int
	AssistantMessageCount    int
	ToolMessageCount         int
	ToolSchemaCount          int
	ToolSchemaFingerprint    string
	ToolSchemas              []ToolSchemaDiagnostics
	InputHasImage            bool
	RequireParameters        bool
	ProviderMessageBytes     int
	ProviderMessageHash      string
}

// ToolSchemaDiagnostics identifies one declaration without retaining its description or schema.
type ToolSchemaDiagnostics struct {
	Name        string
	Fingerprint string
}

// Error is a secret-safe typed provider failure.
type Error struct {
	Kind               ErrorKind
	Provider           Provider
	StatusCode         int
	ProviderStatusCode int
	ErrorType          string
	ProviderCode       string
	ReasonCode         string
	ProviderRequestID  string
	UpstreamProvider   string
	RoutingAttempt     int
	RetryAfterSeconds  int
	PipelineStages     []string
	Scope              string
	FallbackHint       FallbackHint
	Request            RequestDiagnostics
	Err                error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	details := make([]string, 0, 4)
	if e.StatusCode != 0 {
		details = append(details, fmt.Sprintf("http_status=%d", e.StatusCode))
	}
	if e.ProviderStatusCode != 0 && e.ProviderStatusCode != e.StatusCode {
		details = append(details, fmt.Sprintf("provider_status=%d", e.ProviderStatusCode))
	}
	if e.ErrorType != "" {
		details = append(details, "error_type="+e.ErrorType)
	}
	if e.ProviderCode != "" {
		details = append(details, "provider_code="+e.ProviderCode)
	}
	if e.ReasonCode != "" {
		details = append(details, "reason="+e.ReasonCode)
	}
	prefix := fmt.Sprintf("%s %s failure", e.Provider, e.Kind)
	if len(details) > 0 {
		prefix += " (" + strings.Join(details, ", ") + ")"
	}
	if e.Err == nil {
		return prefix
	}
	return prefix + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether orchestration may use a configured fallback.
func (e *Error) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case ErrorTimeout, ErrorRateLimit, ErrorQuota, ErrorService, ErrorMalformed, ErrorEmptyResponse, ErrorInvalidOutput:
		return true
	default:
		return false
	}
}

// Retryable reports whether err permits host fallback.
func Retryable(err error) bool {
	var modelErr *Error
	return errors.As(err, &modelErr) && modelErr.Retryable()
}

// CrossProviderFallback reports whether one guarded compatibility retry is safe.
func CrossProviderFallback(err error) bool {
	var modelErr *Error
	return errors.As(err, &modelErr) && modelErr.FallbackHint == FallbackDifferentProvider
}

func transportError(provider Provider, err error) error {
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: ErrorCanceled, Provider: provider, ErrorType: "request_canceled", Scope: "transport", Err: errors.Wrap(context.Canceled, "provider request canceled")}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrorTimeout, Provider: provider, ErrorType: "transport_timeout", Scope: "transport", Err: errors.Wrap(context.DeadlineExceeded, "provider request timed out")}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Kind: ErrorTimeout, Provider: provider, ErrorType: "transport_timeout", Scope: "transport", Err: errors.New("provider request timed out")}
	}
	return &Error{Kind: ErrorService, Provider: provider, ErrorType: "transport_failure", Scope: "transport", Err: errors.New("provider request failed")}
}

func statusError(provider Provider, status int, errorType, scope string) error {
	return &Error{
		Kind: errorKindForStatus(status), Provider: provider, StatusCode: status, ProviderStatusCode: status,
		ErrorType: errorType, Scope: scope, Err: errors.New("provider returned an error"),
	}
}

func errorKindForStatus(status int) ErrorKind {
	kind := ErrorService
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = ErrorAuthentication
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		kind = ErrorTimeout
	case http.StatusTooManyRequests:
		kind = ErrorRateLimit
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		kind = ErrorInvalidRequest
	case http.StatusRequestEntityTooLarge:
		kind = ErrorContextLimit
	default:
		if status >= 400 && status < 500 {
			kind = ErrorInvalidRequest
		}
	}
	return kind
}
