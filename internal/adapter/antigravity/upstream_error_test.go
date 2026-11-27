package antigravity_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/adapter/errcontract"
)

func TestParseUpstreamError(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		class     errcontract.UpstreamCodeClass
		code      string
		retryable bool
		delay     time.Duration
		message   string
	}{
		{
			name:    "unauthenticated",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"code":401,"status":"UNAUTHENTICATED","message":"Sign in again"}}`,
			class:   errcontract.UpstreamClassAuth,
			code:    "UNAUTHENTICATED",
			message: "Sign in again",
		},
		{
			name:   "validation required",
			status: http.StatusForbidden,
			body: `{"error":{"code":403,"message":"Validation required","details":[` +
				`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED",` +
				`"metadata":{"validation_link":"https://accounts.google.com/verify"}}]}}`,
			class:   errcontract.UpstreamClassAuth,
			code:    "VALIDATION_REQUIRED",
			message: "https://accounts.google.com/verify",
		},
		{
			name:   "retry info overrides quota reset",
			status: http.StatusTooManyRequests,
			body: `{"error":{"code":429,"details":[` +
				`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED",` +
				`"metadata":{"quotaResetDelay":"9s"}},` +
				`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2.5s"}]}}`,
			class:     errcontract.UpstreamClassRateLimit,
			code:      "RATE_LIMIT_EXCEEDED",
			retryable: true,
			delay:     2500 * time.Millisecond,
		},
		{
			name:   "quota reset fallback",
			status: http.StatusTooManyRequests,
			body: `{"error":{"details":[` +
				`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"invalid"},` +
				`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED",` +
				`"metadata":{"quotaResetDelay":"3s"}}]}}`,
			class:     errcontract.UpstreamClassRateLimit,
			code:      "RATE_LIMIT_EXCEEDED",
			retryable: true,
			delay:     3 * time.Second,
		},
		{
			name:   "quota exhausted",
			status: http.StatusTooManyRequests,
			body: `{"error":{"details":[` +
				`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"},` +
				`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]}}`,
			class: errcontract.UpstreamClassRateLimit,
			code:  "QUOTA_EXHAUSTED",
			delay: 2 * time.Second,
		},
		{
			name:   "model capacity exhausted",
			status: http.StatusTooManyRequests,
			body: `{"error":{"details":[` +
				`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED"}]}}`,
			class: errcontract.UpstreamClassRateLimit,
			code:  "MODEL_CAPACITY_EXHAUSTED",
		},
		{
			name:   "daily quota",
			status: http.StatusTooManyRequests,
			body: `{"error":{"details":[` +
				`{"@type":"type.googleapis.com/google.rpc.QuotaFailure",` +
				`"violations":[{"quotaId":"GenerateRequestsPerDay"}]},` +
				`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`,
			class: errcontract.UpstreamClassRateLimit,
			code:  "RATE_LIMIT_EXCEEDED",
		},
		{
			name:      "unavailable",
			status:    http.StatusServiceUnavailable,
			body:      `{"error":{"code":503,"status":"UNAVAILABLE","message":"Service unavailable"}}`,
			class:     errcontract.UpstreamClassServerError,
			code:      "UNAVAILABLE",
			retryable: true,
			message:   "Service unavailable",
		},
		{
			name:      "malformed envelope",
			status:    http.StatusBadGateway,
			body:      `{"error":{"message":"partial payload must stay private"}`,
			class:     errcontract.UpstreamClassServerError,
			retryable: true,
			message:   "Bad Gateway",
		},
	}

	for _, test := range tests {
		t.Run(
			test.name,
			func(t *testing.T) {
				got := antigravity.ParseUpstreamError(test.status, []byte(test.body))
				if got.Status != test.status || got.Class != test.class || got.Code != test.code ||
					got.Retryable != test.retryable || got.RetryAfter != test.delay {
					t.Fatalf("unexpected classification: %+v", got)
				}

				if !strings.Contains(got.Error(), test.message) {
					t.Fatalf("error %q does not include %q", got, test.message)
				}

				if strings.Contains(got.Message, "partial payload must stay private") {
					t.Fatal("error exposed a malformed response body")
				}
			},
		)
	}
}
