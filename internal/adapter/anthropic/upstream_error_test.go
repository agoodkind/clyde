package anthropic

import (
	"errors"
	"net/http"
	"testing"
)

func TestUpstreamErrorPreservesClassification(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	ue := &UpstreamError{
		Classification: Classify(resp, nil),
		Status:         http.StatusTooManyRequests,
		Message:        "rate limited",
	}
	if ue.Class() != ResponseClassRetryableError {
		t.Fatalf("Class() = %s, want retryable", ue.Class())
	}
	if !ue.Retryable() {
		t.Fatalf("Retryable() must be true for 429")
	}
}

func TestUpstreamErrorAsHelper(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}}
	wrapped := &UpstreamError{
		Classification: Classify(resp, nil),
		Status:         http.StatusBadRequest,
		Message:        "bad request body",
	}
	if got, ok := AsUpstreamError(wrapped); !ok || got != wrapped {
		t.Fatalf("AsUpstreamError did not return the typed value")
	}
	if got, ok := AsUpstreamError(nil); ok || got != nil {
		t.Fatalf("AsUpstreamError(nil) should return false, nil")
	}
	if got, ok := AsUpstreamError(errors.New("bare")); ok || got != nil {
		t.Fatalf("AsUpstreamError must not match bare errors")
	}
}

func TestUpstreamErrorTransportPathIsRetryable(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	ue := &UpstreamError{
		Classification: Classify(nil, cause),
		Cause:          cause,
	}
	if ue.Class() != ResponseClassRetryableError {
		t.Fatalf("transport-error wrapped UpstreamError must be retryable, got %s", ue.Class())
	}
	if !errors.Is(errors.Unwrap(ue), cause) {
		t.Fatalf("Unwrap should expose the underlying transport cause")
	}
}

func TestUpstreamErrorErrorString(t *testing.T) {
	cases := []struct {
		name string
		ue   *UpstreamError
		want string
	}{
		{
			name: "status_with_message",
			ue:   &UpstreamError{Status: 429, Message: "You've hit your limit"},
			want: "anthropic 429: You've hit your limit",
		},
		{
			name: "status_only",
			ue:   &UpstreamError{Status: 503},
			want: "anthropic 503",
		},
		{
			name: "transport_only",
			ue:   &UpstreamError{Cause: errors.New("eof")},
			want: "anthropic upstream error: eof",
		},
		{
			name: "empty",
			ue:   &UpstreamError{},
			want: "anthropic upstream error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ue.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}
