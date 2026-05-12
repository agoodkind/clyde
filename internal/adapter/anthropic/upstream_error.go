// Package anthropic implements Anthropic wire models and helpers.
// failed /v1/messages calls. It bundles the Classification (so
// downstream code can route on the four classes without re-parsing
// headers or status codes), the original error message, and the
// upstream HTTP status when present.
//
// Downstream consumers should use errors.As(err, &target) to recover
// the structured shape:
//
//	var ue *anthropic.UpstreamError
//	if errors.As(err, &ue) {
//	    switch ue.Class() {
//	    case anthropic.ResponseClassRetryableError: ...
//	    case anthropic.ResponseClassFatalError:     ...
//	    }
//	}
//
// UpstreamError implements error and unwraps to the underlying
// transport error when one was the root cause, so existing
// errors.Is(err, [context.Canceled]) checks keep working.
package anthropic

import (
	"errors"
	"fmt"
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

// Unwrap exposes the underlying cause for errors.Is and errors.As.
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
// call site over manual errors.As. Returns the typed value (or nil)
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
