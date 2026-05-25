package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

// fakeSink records the signals it receives so a client-level test can assert
// the client emits exactly once at the right branch with the right shape.
type fakeSink struct {
	mu      sync.Mutex
	signals []ratelimitsink.Signal
	err     error
}

func (f *fakeSink) Observe(_ context.Context, sig ratelimitsink.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return f.err
}

func (f *fakeSink) recorded() []ratelimitsink.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ratelimitsink.Signal, len(f.signals))
	copy(out, f.signals)
	return out
}

func headerFrom(pairs map[string]string) http.Header {
	h := http.Header{}
	for key, value := range pairs {
		h.Set(key, value)
	}
	return h
}

// TestRateLimitSignal mirrors the header matrix from classify_test.go and
// asserts the mapped Claim, the HTTPStatus, the mapped Status, and ResetAt
// for each (status, overage-status, representative-claim, reset)
// combination. The rotator now observes every response, not just 429, so
// every entry asserts emit=true.
func TestRateLimitSignal(t *testing.T) {
	t.Parallel()

	const fiveHourReset = "1735689600"
	const overageReset = "1735603200" // earlier than fiveHourReset

	tests := []struct {
		name        string
		status      int
		headers     map[string]string
		wantClaim   ratelimitsink.Claim
		wantStatus  int
		wantSink    ratelimitsink.Status
		wantResetAt time.Time
	}{
		{
			name:       "200_no_warning_observes_unknown",
			status:     http.StatusOK,
			headers:    map[string]string{},
			wantClaim:  ratelimitsink.ClaimUnknown,
			wantStatus: http.StatusOK,
			wantSink:   ratelimitsink.StatusUnknown,
		},
		{
			name:   "200_allowed_observes_allowed",
			status: http.StatusOK,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status": "allowed",
			},
			wantClaim:  ratelimitsink.ClaimUnknown,
			wantStatus: http.StatusOK,
			wantSink:   ratelimitsink.StatusAllowed,
		},
		{
			name:   "200_allowed_warning_observes_allowed_warning",
			status: http.StatusOK,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status": "allowed_warning",
			},
			wantClaim:  ratelimitsink.ClaimUnknown,
			wantStatus: http.StatusOK,
			wantSink:   ratelimitsink.StatusAllowedWarning,
		},
		{
			name:   "200_overage_active_observes_unknown_status",
			status: http.StatusOK,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Overage-Status": "allowed",
			},
			wantClaim:  ratelimitsink.ClaimUnknown,
			wantStatus: http.StatusOK,
			wantSink:   ratelimitsink.StatusUnknown,
		},
		{
			name:   "200_surpassed_threshold_observes_unknown_status",
			status: http.StatusOK,
			headers: map[string]string{
				"anthropic-ratelimit-unified-7d-surpassed-threshold": "1",
			},
			wantClaim:  ratelimitsink.ClaimUnknown,
			wantStatus: http.StatusOK,
			wantSink:   ratelimitsink.StatusUnknown,
		},
		{
			// A 200 with overage-status: rejected is advisory; Anthropic
			// served the request. The signal is still emitted (so the
			// operator surface can record the warning), but the rotator's
			// quotaStatus reads as unknown so selection does not skip the
			// account.
			name:   "200_overage_rejected_observes_unknown_status",
			status: http.StatusOK,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Overage-Status":       "rejected",
				"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
				"Anthropic-Ratelimit-Unified-Reset":                fiveHourReset,
			},
			wantClaim:   ratelimitsink.ClaimFiveHour,
			wantStatus:  http.StatusOK,
			wantSink:    ratelimitsink.StatusUnknown,
			wantResetAt: time.Unix(1735689600, 0),
		},
		{
			name:   "429_seven_day_observes_rejected",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day",
				"Anthropic-Ratelimit-Unified-Reset":                fiveHourReset,
			},
			wantClaim:   ratelimitsink.ClaimSevenDay,
			wantStatus:  http.StatusTooManyRequests,
			wantSink:    ratelimitsink.StatusRejected,
			wantResetAt: time.Unix(1735689600, 0),
		},
		{
			name:   "429_seven_day_opus_observes_rejected",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day_opus",
				"Anthropic-Ratelimit-Unified-Reset":                fiveHourReset,
			},
			wantClaim:   ratelimitsink.ClaimSevenDayOpus,
			wantStatus:  http.StatusTooManyRequests,
			wantSink:    ratelimitsink.StatusRejected,
			wantResetAt: time.Unix(1735689600, 0),
		},
		{
			name:   "429_uses_earliest_of_reset_and_overage_reset",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
				"Anthropic-Ratelimit-Unified-Reset":                fiveHourReset,
				"Anthropic-Ratelimit-Unified-Overage-Reset":        overageReset,
			},
			wantClaim:   ratelimitsink.ClaimFiveHour,
			wantStatus:  http.StatusTooManyRequests,
			wantSink:    ratelimitsink.StatusRejected,
			wantResetAt: time.Unix(1735603200, 0),
		},
		{
			name:   "429_unknown_window_maps_unknown",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": "lunar_cycle",
			},
			wantClaim:   ratelimitsink.ClaimUnknown,
			wantStatus:  http.StatusTooManyRequests,
			wantSink:    ratelimitsink.StatusRejected,
			wantResetAt: time.Time{},
		},
		{
			name:   "429_missing_claim_maps_unknown",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Reset": fiveHourReset,
			},
			wantClaim:   ratelimitsink.ClaimUnknown,
			wantStatus:  http.StatusTooManyRequests,
			wantSink:    ratelimitsink.StatusRejected,
			wantResetAt: time.Unix(1735689600, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := headerFrom(tt.headers)
			class := Classify(&http.Response{StatusCode: tt.status, Header: h}, nil)
			sig, emit := rateLimitSignal(class, h, "tok")
			if !emit {
				t.Fatalf("emit = false, want true (Observe runs on every response)")
			}
			if sig.Provider != "anthropic" {
				t.Fatalf("provider = %q, want anthropic", sig.Provider)
			}
			if sig.AccessToken != "tok" {
				t.Fatalf("access token = %q, want tok", sig.AccessToken)
			}
			if sig.Claim != tt.wantClaim {
				t.Fatalf("claim = %q, want %q", sig.Claim, tt.wantClaim)
			}
			if sig.HTTPStatus != tt.wantStatus {
				t.Fatalf("status = %d, want %d", sig.HTTPStatus, tt.wantStatus)
			}
			if sig.Status != tt.wantSink {
				t.Fatalf("sink status = %q, want %q", sig.Status, tt.wantSink)
			}
			if !sig.ResetAt.Equal(tt.wantResetAt) {
				t.Fatalf("reset_at = %v, want %v", sig.ResetAt, tt.wantResetAt)
			}
			if sig.ObservedAt.IsZero() {
				t.Fatalf("observed_at must be set")
			}
		})
	}
}

// Test200OverageRejectedObservesWithoutRejectedStatus pins the
// throttle-policy contract: a 200 response carrying
// anthropic-ratelimit-unified-overage-status: rejected and
// representative-claim: five_hour DOES emit an Observe signal (so the
// operator surface can record the warning), but the carried sink Status is
// not Rejected. The rotator therefore must not treat the slot as fresh-
// rejected for selection, because Anthropic served the request and keeps
// serving other concurrent requests on the same account.
func Test200OverageRejectedObservesWithoutRejectedStatus(t *testing.T) {
	t.Parallel()
	h := headerFrom(map[string]string{
		"Anthropic-Ratelimit-Unified-Overage-Status":       "rejected",
		"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
		"Anthropic-Ratelimit-Unified-Reset":                "1735689600",
	})
	class := Classify(&http.Response{StatusCode: http.StatusOK, Header: h}, nil)
	if !class.HasOverageRejected {
		t.Fatalf("Classification.HasOverageRejected must remain true so notice surfaces the warning")
	}
	sig, emit := rateLimitSignal(class, h, "tok")
	if !emit {
		t.Fatal("rateLimitSignal must emit on 200 so the rotator records the observation")
	}
	if sig.Status == ratelimitsink.StatusRejected {
		t.Fatalf("sink status must not be StatusRejected on a 200 (Anthropic served it), got %q", sig.Status)
	}
}

// TestMapRepresentativeClaimUnknownWarnsOnce asserts the unknown-window
// warning fires once per distinct value: repeated calls with the same unknown
// value reuse the same sync.Once, and a distinct unknown value gets its own.
func TestMapRepresentativeClaimUnknownWarnsOnce(t *testing.T) {
	// Not parallel: this test inspects the package-level once-per-value guard.
	const unknownA = "test_unknown_window_alpha"
	const unknownB = "test_unknown_window_beta"
	t.Cleanup(func() {
		unknownClaimWarnedOnce.Delete(unknownA)
		unknownClaimWarnedOnce.Delete(unknownB)
	})

	for range 3 {
		if got := mapRepresentativeClaim(unknownA); got != ratelimitsink.ClaimUnknown {
			t.Fatalf("claim = %q, want unknown", got)
		}
	}
	if got := mapRepresentativeClaim(unknownB); got != ratelimitsink.ClaimUnknown {
		t.Fatalf("claim = %q, want unknown", got)
	}

	onceA, okA := unknownClaimWarnedOnce.Load(unknownA)
	if !okA {
		t.Fatalf("unknown value %q must register a sync.Once", unknownA)
	}
	if _, ok := onceA.(*sync.Once); !ok {
		t.Fatalf("stored guard for %q is not *sync.Once", unknownA)
	}
	if _, ok := unknownClaimWarnedOnce.Load(unknownB); !ok {
		t.Fatalf("distinct unknown value %q must register its own sync.Once", unknownB)
	}
}

func newSignalTestClient(t *testing.T, status int, headers map[string]string, sink ratelimitsink.Sink) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		http:  &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}},
		oauth: &staticToken{},
		cfg: Config{
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			RateLimitSink:         sink,
		},
	}
}

// TestClientEmitsRateLimitSignalOn429 drives do() against a 429 and asserts
// the client reports exactly one signal carrying the request token.
func TestClientEmitsRateLimitSignalOn429(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	cli := newSignalTestClient(t, http.StatusTooManyRequests, map[string]string{
		"anthropic-ratelimit-unified-representative-claim": "seven_day",
		"anthropic-ratelimit-unified-reset":                "1735689600",
	}, sink)

	_, err := cli.Do(context.Background(), Request{
		Model:    "claude-test",
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err == nil {
		t.Fatalf("Do must return an error on 429")
	}
	recorded := sink.recorded()
	if len(recorded) != 1 {
		t.Fatalf("sink received %d signals, want 1", len(recorded))
	}
	if recorded[0].Claim != ratelimitsink.ClaimSevenDay {
		t.Fatalf("claim = %q, want seven_day", recorded[0].Claim)
	}
	if recorded[0].HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorded[0].HTTPStatus)
	}
	if recorded[0].Status != ratelimitsink.StatusRejected {
		t.Fatalf("sink status = %q, want StatusRejected on 429", recorded[0].Status)
	}
	if recorded[0].AccessToken != "test-token" {
		t.Fatalf("access token = %q, want test-token", recorded[0].AccessToken)
	}
}

// TestClientEmitsObserveOn200OverageRejected drives do() against a 200
// carrying overage-status: rejected and asserts the client emits one
// Observe signal whose Status is not Rejected. The rotator therefore
// records the observation without taking the slot out of rotation.
func TestClientEmitsObserveOn200OverageRejected(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	cli := newSignalTestClient(t, http.StatusOK, map[string]string{
		"anthropic-ratelimit-unified-overage-status":       "rejected",
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
		"anthropic-ratelimit-unified-reset":                "1735689600",
	}, sink)

	resp, err := cli.Do(context.Background(), Request{
		Model:    "claude-test",
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err != nil {
		t.Fatalf("Do returned error on 200: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorded := sink.recorded()
	if len(recorded) != 1 {
		t.Fatalf("sink received %d signals on 200, want 1", len(recorded))
	}
	if recorded[0].Status == ratelimitsink.StatusRejected {
		t.Fatalf("sink status = %q, must not be StatusRejected (Anthropic served the request)", recorded[0].Status)
	}
}

// TestClientEmitsObserveOnCleanSuccess drives do() against a clean 200 and
// asserts the client emits one Observe signal carrying an unknown status (no
// unified-status header on the response).
func TestClientEmitsObserveOnCleanSuccess(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	cli := newSignalTestClient(t, http.StatusOK, map[string]string{}, sink)

	resp, err := cli.Do(context.Background(), Request{
		Model:    "claude-test",
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err != nil {
		t.Fatalf("Do returned error on clean 200: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorded := sink.recorded()
	if len(recorded) != 1 {
		t.Fatalf("sink received %d signals on clean success, want 1", len(recorded))
	}
	if recorded[0].Status != ratelimitsink.StatusUnknown {
		t.Fatalf("sink status = %q, want StatusUnknown on bare 200", recorded[0].Status)
	}
}

// TestClientNilSinkIsNoOp confirms a nil sink does not panic on a hard limit.
func TestClientNilSinkIsNoOp(t *testing.T) {
	t.Parallel()
	cli := newSignalTestClient(t, http.StatusTooManyRequests, map[string]string{
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
	}, nil)

	_, err := cli.Do(context.Background(), Request{
		Model:    "claude-test",
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err == nil {
		t.Fatalf("Do must return an error on 429")
	}
}
