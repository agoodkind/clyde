package anthropic

import (
	"errors"
	"fmt"
)

// ErrorKind names the documented Anthropic envelope
// `error.type` values that flow through both HTTP-level rejections
// and SSE `event: error` frames on a 200 stream. The constants are
// the named enum the SSE parser and the upstream-to-adapter mapping
// switch on so a future typo is a compile-time error (CLYDE-439).
type ErrorKind string

// Anthropic error.type enum.
const (
	// ErrorKindNone is the zero value used when the failure was an
	// HTTP-level rejection without a wire-typed Anthropic envelope.
	ErrorKindNone ErrorKind = ""
	// ErrorKindRateLimit maps to `upstream_rate_limited` in the
	// OpenAI route family. The 5h or per-request quota was tripped.
	ErrorKindRateLimit ErrorKind = "rate_limit_error"
	// ErrorKindOverloaded is a retryable server-side capacity signal.
	ErrorKindOverloaded ErrorKind = "overloaded_error"
	// ErrorKindAuth maps to `upstream_auth_failed` in the OpenAI
	// route family.
	ErrorKindAuth ErrorKind = "authentication_error"
	// ErrorKindInvalidRequest maps to `upstream_malformed_request`
	// in the OpenAI route family.
	ErrorKindInvalidRequest ErrorKind = "invalid_request_error"
	// ErrorKindAPI is the generic upstream-side failure bucket.
	ErrorKindAPI ErrorKind = "api_error"
)

// UpstreamError annotates a request failure with classification.
type UpstreamError struct {
	// Classification carries the four-class routing decision and the
	// header-derived flags. Always populated.
	Classification Classification
	// Status is the upstream HTTP status code when the call reached a
	// response boundary; 0 for transport errors.
	Status int
	// Message is the human-readable message the client built (e.g.
	// the FormatRateLimitMessage friendly text or a truncated body).
	Message string
	// Cause is the underlying error when the failure was a transport
	// error or a wrapped library error. Nil for synthesized HTTP
	// status errors.
	Cause error
	// ErrorType is the Anthropic envelope error.type carried on SSE
	// `event: error` frames in a 200 stream. [ErrorKindNone] when
	// the failure was an HTTP-level rejection where Status carries
	// the routing signal.
	//
	// Upstream rejection of a request can arrive as either:
	//   - Non-2xx HTTP status (handled by the client's status-code
	//     branch, with Status set and ErrorType=[ErrorKindNone])
	//   - HTTP 200 + SSE event:error frame as the first stream event
	//     (handled by the stream parser, with Status=0 and ErrorType
	//     carrying the Anthropic envelope type)
	// The downstream classifier prefers ErrorType when set so the
	// adapter error envelope reflects the upstream cause even when
	// the wire HTTP status was 200 (CLYDE-439).
	ErrorType ErrorKind
}

// Error implements error.
func (e *UpstreamError) Error() string {
	if e == nil {
		return ""
	}
	switch {
	case e.Status > 0 && e.Message != "":
		return fmt.Sprintf("anthropic %d: %s", e.Status, e.Message)
	case e.Status > 0:
		return fmt.Sprintf("anthropic %d", e.Status)
	case e.Cause != nil:
		return fmt.Sprintf("anthropic upstream error: %v", e.Cause)
	default:
		return "anthropic upstream error"
	}
}

// Unwrap exposes the underlying cause for [errors.Is] and [errors.As].
func (e *UpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Class is a convenience accessor for callers that only need the
// routing decision.
func (e *UpstreamError) Class() ResponseClass {
	if e == nil {
		return ResponseClassUnknown
	}
	return e.Classification.Class
}

// Retryable mirrors Classification.Retryable.
func (e *UpstreamError) Retryable() bool {
	if e == nil {
		return false
	}
	return e.Classification.Retryable
}

// AsUpstreamError is a small helper for callers that prefer a single
// call site over manual [errors.As]. Returns the typed value (or nil)
// and a found bool.
func AsUpstreamError(err error) (*UpstreamError, bool) {
	if err == nil {
		return nil, false
	}
	var target *UpstreamError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
