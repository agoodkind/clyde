package anthropic

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func timeUTC() time.Time { return time.Unix(1700000000, 0).UTC() }

func TestClassifyTransportError(t *testing.T) {
	c := Classify(nil, errors.New("dial tcp: connection refused"))
	if c.Class != ResponseClassRetryableError {
		t.Fatalf("transport error should be retryable, got %s", c.Class)
	}
	if !c.Retryable {
		t.Fatalf("Retryable convenience flag should mirror class")
	}
	if c.TransportError == nil {
		t.Fatalf("TransportError should be preserved")
	}
}

func TestClassifyNilInputs(t *testing.T) {
	c := Classify(nil, nil)
	if c.Class != ResponseClassUnknown {
		t.Fatalf("nil resp + nil err must be Unknown, got %s", c.Class)
	}
}

func TestClassify429IsRetryable(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	c := Classify(resp, nil)
	if c.Class != ResponseClassRetryableError {
		t.Fatalf("429 must be retryable, got %s", c.Class)
	}
	if !c.Retryable {
		t.Fatalf("Retryable flag should be true on 429")
	}
}

func TestClassify5xxRetryable(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		resp := &http.Response{StatusCode: code, Header: http.Header{}}
		c := Classify(resp, nil)
		if c.Class != ResponseClassRetryableError {
			t.Fatalf("status %d should be retryable, got %s", code, c.Class)
		}
	}
}

func TestClassify4xxFatal(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		resp := &http.Response{StatusCode: code, Header: http.Header{}}
		c := Classify(resp, nil)
		if c.Class != ResponseClassFatalError {
			t.Fatalf("status %d should be fatal, got %s", code, c.Class)
		}
		if c.Retryable {
			t.Fatalf("status %d should not be retryable", code)
		}
	}
}

func TestClassify200NoHeadersIsClean(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	c := Classify(resp, nil)
	if c.Class != ResponseClassSuccessNoWarning {
		t.Fatalf("200 with no warning headers must be clean success, got %s", c.Class)
	}
	if c.AllowedWarning || c.HasOverageRejected || c.HasOverageActive || c.SurpassedThreshold {
		t.Fatalf("clean 200 should have no flags set, got %+v", c)
	}
}

// TestClassify200OverageRejectedIsSuccessWithWarning is the regression
// guard for the bug the plan calls out: a successful 200 response with
// anthropic-ratelimit-unified-overage-status: rejected must NOT be
// classed as a fatal error; it is a successful response with a notice.
func TestClassify200OverageRejectedIsSuccessWithWarning(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-Overage-Status": []string{"rejected"},
		},
	}
	c := Classify(resp, nil)
	if c.Class != ResponseClassSuccessWithWarning {
		t.Fatalf("200 + overage rejected must be SuccessWithWarning, got %s", c.Class)
	}
	if !c.HasOverageRejected {
		t.Fatalf("HasOverageRejected flag must be true")
	}
	if c.Class == ResponseClassFatalError || c.Class == ResponseClassRetryableError {
		t.Fatalf("200 must never classify as an error class")
	}
}

func TestClassify200AllowedWarningIsSuccessWithWarning(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-Status": []string{"allowed_warning"},
		},
	}
	c := Classify(resp, nil)
	if c.Class != ResponseClassSuccessWithWarning {
		t.Fatalf("200 + allowed_warning must be SuccessWithWarning, got %s", c.Class)
	}
	if !c.AllowedWarning {
		t.Fatalf("AllowedWarning flag must be true")
	}
}

func TestClassify200OverageActiveIsSuccessWithWarning(t *testing.T) {
	for _, value := range []string{"allowed", "allowed_warning"} {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Anthropic-Ratelimit-Unified-Overage-Status": []string{value},
			},
		}
		c := Classify(resp, nil)
		if c.Class != ResponseClassSuccessWithWarning {
			t.Fatalf("200 + overage-status=%q must be SuccessWithWarning, got %s", value, c.Class)
		}
		if !c.HasOverageActive {
			t.Fatalf("HasOverageActive flag must be true for overage-status=%q", value)
		}
	}
}

func TestClassify200SurpassedThresholdIsSuccessWithWarning(t *testing.T) {
	for _, claim := range []string{"5h", "7d", "overage"} {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				http.CanonicalHeaderKey("anthropic-ratelimit-unified-" + claim + "-surpassed-threshold"): []string{"1"},
			},
		}
		c := Classify(resp, nil)
		if c.Class != ResponseClassSuccessWithWarning {
			t.Fatalf("200 + %s surpassed-threshold must be SuccessWithWarning, got %s", claim, c.Class)
		}
		if !c.SurpassedThreshold {
			t.Fatalf("SurpassedThreshold flag must be true for claim %s", claim)
		}
	}
}

func TestClassifyHeaderCasingTolerant(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-Overage-Status": []string{"  Rejected  "},
		},
	}
	c := Classify(resp, nil)
	if c.Class != ResponseClassSuccessWithWarning {
		t.Fatalf("classifier must trim and lowercase header values; got %s", c.Class)
	}
	if !c.HasOverageRejected {
		t.Fatalf("HasOverageRejected must be true after normalizing whitespace and case")
	}
}

func TestClassifyHeadersMatchesClassify(t *testing.T) {
	h := http.Header{
		"Anthropic-Ratelimit-Unified-Overage-Status": []string{"rejected"},
	}
	a := Classify(&http.Response{StatusCode: http.StatusOK, Header: h}, nil)
	b := ClassifyHeaders(h, http.StatusOK)
	if a.Class != b.Class {
		t.Fatalf("ClassifyHeaders class = %s, Classify class = %s", b.Class, a.Class)
	}
	if a.HasOverageRejected != b.HasOverageRejected {
		t.Fatalf("ClassifyHeaders flags must match Classify flags")
	}
}

// TestEvaluateNoticeShortCircuitsCleanSuccess locks in that the
// classifier owns the warning-presence gate: a successful 200 with no
// warning headers must yield no notice without reaching the per-claim
// formatters.
func TestEvaluateNoticeShortCircuitsCleanSuccess(t *testing.T) {
	if got := EvaluateNotice(http.Header{}, timeUTC(), []float64{75, 95}); got != nil {
		t.Fatalf("clean 200 should produce no notice, got %+v", got)
	}
}

func TestEvaluateNoticeUsesConfiguredThresholds(t *testing.T) {
	t.Parallel()
	h := http.Header{
		"Anthropic-Ratelimit-Unified-Status":         []string{"allowed_warning"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.80"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       []string{"1735689600"},
	}
	if got := EvaluateNotice(h, timeUTC(), []float64{90}); got != nil {
		t.Fatalf("configured threshold should suppress 80%% notice, got %+v", got)
	}
	got := EvaluateNotice(h, timeUTC(), []float64{75, 90})
	if got == nil {
		t.Fatal("expected notice at configured 75%% threshold")
	}
	if !strings.Contains(got.Text, "You've used 75% of your weekly limit") {
		t.Fatalf("notice text=%q", got.Text)
	}
}

func TestClassifyClassStringStable(t *testing.T) {
	cases := map[ResponseClass]string{
		ResponseClassUnknown:            "unknown",
		ResponseClassFatalError:         "fatal_upstream_error",
		ResponseClassRetryableError:     "retryable_upstream_error",
		ResponseClassSuccessWithWarning: "success_with_warning_headers",
		ResponseClassSuccessNoWarning:   "success_no_warning",
	}
	for class, want := range cases {
		if got := class.String(); got != want {
			t.Fatalf("class %d string = %q, want %q", class, got, want)
		}
	}
}
