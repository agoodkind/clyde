package anthropic

import (
	"net/http"
	"strings"
)

// ResponseClass enumerates the four outcomes documented above. It is
// intentionally small so callers can switch on it.
type ResponseClass int

const (
	// ResponseClassUnknown is the zero value, only returned when the
	// caller passes a nil [http.Response] and no transport error.
	ResponseClassUnknown ResponseClass = iota
	// ResponseClassFatalError is part of Clyde's typed adapter surface.
	ResponseClassFatalError
	// ResponseClassRetryableError is part of Clyde's typed adapter surface.
	ResponseClassRetryableError
	// ResponseClassSuccessWithWarning is part of Clyde's typed adapter surface.
	ResponseClassSuccessWithWarning
	// ResponseClassSuccessNoWarning is part of Clyde's typed adapter surface.
	ResponseClassSuccessNoWarning
)

// String returns a stable label suitable for log fields and tests.
func (c ResponseClass) String() string {
	switch c {
	case ResponseClassUnknown:
		return "unknown"
	case ResponseClassFatalError:
		return "fatal_upstream_error"
	case ResponseClassRetryableError:
		return "retryable_upstream_error"
	case ResponseClassSuccessWithWarning:
		return "success_with_warning_headers"
	case ResponseClassSuccessNoWarning:
		return "success_no_warning"
	}
	return "unknown"
}

// Classification is the structured result returned by Classify. The
// Class field carries the routing decision; the remaining fields
// expose what was observed so callers can build user-facing errors or
// notices without re-parsing headers.
type Classification struct {
	// Class is the routing decision. Always populated.
	Class ResponseClass
	// Status is the upstream HTTP status code, or 0 when the call
	// failed before a response was received.
	Status int
	// TransportError is non-nil when the http client itself returned
	// an error (DNS, dial, TLS, EOF before headers).
	TransportError error
	// Retryable is a convenience mirror of
	// Class == ResponseClassRetryableError.
	Retryable bool
	// HasOverageRejected mirrors
	// anthropic-ratelimit-unified-overage-status: rejected so callers
	// can distinguish the "200 with overage rejected" case the plan
	// calls out explicitly.
	HasOverageRejected bool
	// HasOverageActive mirrors overage-status in {allowed, allowed_warning}.
	HasOverageActive bool
	// SurpassedThreshold is true when any of the
	// anthropic-ratelimit-unified-*-surpassed-threshold headers are
	// non-empty.
	SurpassedThreshold bool
	// AllowedWarning mirrors anthropic-ratelimit-unified-status:
	// allowed_warning.
	AllowedWarning bool
}

// Classify inspects (resp, transportErr) and returns the four-way
// routing class plus the supporting observations. Either resp or
// transportErr must be non-nil; passing both nil yields
// ResponseClassUnknown.
// ResponseClassFatalError is part of Clyde's typed adapter surface.
//
// ResponseClassRetryableError is part of Clyde's typed adapter surface.
// The function is intentionally permissive about header casing and
// ResponseClassSuccessWithWarning is part of Clyde's typed adapter surface.
// whitespace: net/http already canonicalizes header names, but the
// ResponseClassSuccessNoWarning is part of Clyde's typed adapter surface.
// values come straight from the wire and Anthropic has historically
// emitted both "rejected" and " Rejected " in different builds.
func Classify(resp *http.Response, transportErr error) Classification {
	if transportErr != nil {
		return Classification{
			Class:          ResponseClassRetryableError,
			TransportError: transportErr,
			Retryable:      true, Status: 0, HasOverageRejected: false, HasOverageActive: false, SurpassedThreshold: false, AllowedWarning: false,
		}
	}
	if resp == nil {
		return Classification{Class: ResponseClassUnknown, Status: 0, TransportError: nil, Retryable: false, HasOverageRejected: false, HasOverageActive: false, SurpassedThreshold: false, AllowedWarning: false}
	}

	out := Classification{Status: resp.StatusCode, Class: 0, TransportError: nil, Retryable: false, HasOverageRejected: false, HasOverageActive: false, SurpassedThreshold: false, AllowedWarning: false}

	switch resp.StatusCode {
	case http.StatusOK:
		populateWarningFlags(&out, resp.Header)
		if out.HasOverageRejected || out.HasOverageActive || out.SurpassedThreshold || out.AllowedWarning {
			out.Class = ResponseClassSuccessWithWarning
		} else {
			out.Class = ResponseClassSuccessNoWarning
		}
	case http.StatusTooManyRequests:
		populateWarningFlags(&out, resp.Header)
		out.Class = ResponseClassRetryableError
		out.Retryable = true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		out.Class = ResponseClassRetryableError
		out.Retryable = true
	default:
		out.Class = ResponseClassFatalError
	}
	return out
}

// ClassifyHeaders is a header-only entry point for callers that
// already know the upstream returned a particular status (typically
// 200) and just want the warning-flag detection without constructing
// a synthetic *[http.Response]. It returns the same Classification
// shape Classify would produce.
//
// Pass [http.StatusOK] when the call succeeded; the function will then
// inspect the warning headers and return either
// ResponseClassSuccessNoWarning or ResponseClassSuccessWithWarning.
// For non-2xx statuses, the function mirrors Classify's status-only
// branch (no header inspection beyond what Classify already does).
func ClassifyHeaders(h http.Header, status int) Classification {
	resp := &http.Response{StatusCode: status, Header: h}
	return Classify(resp, nil)
}

// rateLimitOverageStatus is the closed enum of values Anthropic emits
// on the `Anthropic-Ratelimit-Unified-Overage-Status` header.
type rateLimitOverageStatus string

const (
	rateLimitOverageRejected       rateLimitOverageStatus = "rejected"
	rateLimitOverageAllowed        rateLimitOverageStatus = "allowed"
	rateLimitOverageAllowedWarning rateLimitOverageStatus = "allowed_warning"
)

func populateWarningFlags(out *Classification, h http.Header) {
	status := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Status")))
	overage := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-Status")))

	out.AllowedWarning = status == "allowed_warning"
	switch rateLimitOverageStatus(overage) {
	case rateLimitOverageRejected:
		out.HasOverageRejected = true
	case rateLimitOverageAllowed, rateLimitOverageAllowedWarning:
		out.HasOverageActive = true
	}
	for _, claim := range []string{"5h", "7d", "overage"} {
		v := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-" + claim + "-surpassed-threshold"))
		if v != "" {
			out.SurpassedThreshold = true
			return
		}
	}
}
