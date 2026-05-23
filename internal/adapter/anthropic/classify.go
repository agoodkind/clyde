// Package anthropic implements Anthropic wire models and helpers.
//
// Classify maps the observed transport state, HTTP status, and unified
// rate-limit response headers into one of four mutually exclusive
// classes that downstream code can route on without re-reading
// individual headers:
//
//  1. ResponseClassFatalError. The upstream call failed in a way that
//     should surface to the client as a native error (transport error,
//     upstream 4xx other than 429, upstream 5xx other than 503).
//     Callers should emit a structured OpenAI-shaped error to Cursor
//     rather than synthesize assistant chat content.
//
//  2. ResponseClassRetryableError. The upstream call failed in a way
//     that is reasonable to retry: 429 rate limit, 502/503/504, or a
//     transport error that callers may treat as transient. The error
//     itself is still terminal for this attempt; the class only
//     advertises that an automated retry or wait-and-retry instruction
//     is appropriate.
//
//  3. ResponseClassSuccessWithWarning. The upstream returned 200 OK,
//     but the unified rate-limit headers indicate a non-fatal warning
//     condition (active overage, approaching threshold, overage warn
//     state). Callers should emit a non-fatal notice through the
//     notice surface, not an assistant-shaped failure message.
//
//  4. ResponseClassSuccessNoWarning. The upstream returned 200 OK with
//     no notable rate-limit warning headers. No notice needed.
//
// Classify is a pure header/status inspector and does not consult
// time, retry counts, or any other ambient state; downstream code
// owns the resulting policy.
//
// # Caller inventory
//
// The Anthropic-related header consumers in this repo are:
//
//   - internal/adapter/anthropic/classify.go (Classify, ClassifyHeaders).
//     Owns the four-class routing decision and the warning flags.
//
//   - internal/adapter/anthropic/notice.go (EvaluateNotice).
//     Owns success-path notice text. Uses ClassifyHeaders as its
//     entry gate so the "is there a warning?" decision lives in one
//     place; only runs the detail formatters when the gate fires.
//
//   - internal/adapter/anthropic/ratelimit_message.go
//     (FormatRateLimitMessage). Owns 429 user-facing text. Reads
//     additional headers (representative_claim, reset, overage_reset,
//     overage_disabled_reason) that Classify intentionally does not
//     surface, since those are formatting inputs, not routing inputs.
//
//   - internal/adapter/runtime/notice.go (EvaluateNoticeFromHeaders,
//     NoticeForStreamHeaders). Wraps notice.go for the OpenAI-shaped
//     stream/collect emit path; does not read raw headers itself.
//
// New consumers should prefer Classify or ClassifyHeaders for routing
// decisions and use the dedicated formatter functions only for
// user-facing strings.
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
	// caller passes a nil http.Response and no transport error.
	ResponseClassUnknown ResponseClass = iota
	ResponseClassFatalError
	ResponseClassRetryableError
	ResponseClassSuccessWithWarning
	ResponseClassSuccessNoWarning
)

// String returns a stable label suitable for log fields and tests.
func (c ResponseClass) String() string {
	switch c {
	case ResponseClassFatalError:
		return "fatal_upstream_error"
	case ResponseClassRetryableError:
		return "retryable_upstream_error"
	case ResponseClassSuccessWithWarning:
		return "success_with_warning_headers"
	case ResponseClassSuccessNoWarning:
		return "success_no_warning"
	default:
		return "unknown"
	}
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
//
// The function is intentionally permissive about header casing and
// whitespace: net/http already canonicalizes header names, but the
// values come straight from the wire and Anthropic has historically
// emitted both "rejected" and " Rejected " in different builds.
func Classify(resp *http.Response, transportErr error) Classification {
	if transportErr != nil {
		return Classification{
			Class:          ResponseClassRetryableError,
			TransportError: transportErr,
			Retryable:      true,
		}
	}
	if resp == nil {
		return Classification{Class: ResponseClassUnknown}
	}

	out := Classification{Status: resp.StatusCode}

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
// a synthetic *http.Response. It returns the same Classification
// shape Classify would produce.
//
// Pass http.StatusOK when the call succeeded; the function will then
// inspect the warning headers and return either
// ResponseClassSuccessNoWarning or ResponseClassSuccessWithWarning.
// For non-2xx statuses, the function mirrors Classify's status-only
// branch (no header inspection beyond what Classify already does).
func ClassifyHeaders(h http.Header, status int) Classification {
	resp := &http.Response{StatusCode: status, Header: h}
	return Classify(resp, nil)
}

func populateWarningFlags(out *Classification, h http.Header) {
	status := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Status")))
	overage := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-Status")))

	out.AllowedWarning = status == "allowed_warning"
	switch overage {
	case "rejected":
		out.HasOverageRejected = true
	case "allowed", "allowed_warning":
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
