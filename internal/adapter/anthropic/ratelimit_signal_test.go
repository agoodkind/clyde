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

func (f *fakeSink) Throttle(_ context.Context, sig ratelimitsink.Signal) error {
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
// asserts emit vs no-emit, the mapped Claim, the HTTPStatus, and ResetAt for
// each (status, overage-status, representative-claim, reset) combination.
func TestRateLimitSignal(t *testing.T) {
	t.Parallel()

	const fiveHourReset = "1735689600"
	const overageReset = "1735603200" // earlier than fiveHourReset

	tests := []struct {
		name        string
		status      int
		headers     map[string]string
		wantEmit    bool
		wantClaim   ratelimitsink.Claim
		wantStatus  int
		wantResetAt time.Time
	}{
		{
			name:     "200_no_warning_does_not_emit",
			status:   http.StatusOK,
			headers:  map[string]string{},
			wantEmit: false,
		},
		{
			name:   "200_allowed_warning_soft_does_not_emit",
			status: http.StatusOK,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status": "allowed_warning",
			},
			wantEmit: false,
		},
		{
			name:   "200_overage_active_soft_does_not_emit",
			status: http.StatusOK,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Overage-Status": "allowed",
			},
			wantEmit: false,
		},
		{
			name:   "200_surpassed_threshold_soft_does_not_emit",
			status: http.StatusOK,
			headers: map[string]string{
				"anthropic-ratelimit-unified-7d-surpassed-threshold": "1",
			},
			wantEmit: false,
		},
		{
			// A 200 with overage-status: rejected is advisory; Anthropic served
			// the request, so the rotator must not throttle the account.
			name:   "200_overage_rejected_does_not_emit",
			status: http.StatusOK,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Overage-Status":       "rejected",
				"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
				"Anthropic-Ratelimit-Unified-Reset":                fiveHourReset,
			},
			wantEmit: false,
		},
		{
			name:   "429_seven_day_emits",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day",
				"Anthropic-Ratelimit-Unified-Reset":                fiveHourReset,
			},
			wantEmit:    true,
			wantClaim:   ratelimitsink.ClaimSevenDay,
			wantStatus:  http.StatusTooManyRequests,
			wantResetAt: time.Unix(1735689600, 0),
		},
		{
			name:   "429_seven_day_opus_emits",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day_opus",
				"Anthropic-Ratelimit-Unified-Reset":                fiveHourReset,
			},
			wantEmit:    true,
			wantClaim:   ratelimitsink.ClaimSevenDayOpus,
			wantStatus:  http.StatusTooManyRequests,
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
			wantEmit:    true,
			wantClaim:   ratelimitsink.ClaimFiveHour,
			wantStatus:  http.StatusTooManyRequests,
			wantResetAt: time.Unix(1735603200, 0),
		},
		{
			name:   "429_unknown_window_maps_unknown",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": "lunar_cycle",
			},
			wantEmit:    true,
			wantClaim:   ratelimitsink.ClaimUnknown,
			wantStatus:  http.StatusTooManyRequests,
			wantResetAt: time.Time{},
		},
		{
			name:   "429_missing_claim_maps_unknown",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Reset": fiveHourReset,
			},
			wantEmit:    true,
			wantClaim:   ratelimitsink.ClaimUnknown,
			wantStatus:  http.StatusTooManyRequests,
			wantResetAt: time.Unix(1735689600, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := headerFrom(tt.headers)
			class := Classify(&http.Response{StatusCode: tt.status, Header: h}, nil)
			sig, emit := rateLimitSignal(class, h, "tok")
			if emit != tt.wantEmit {
				t.Fatalf("emit = %v, want %v (class=%s overageRejected=%v)", emit, tt.wantEmit, class.Class, class.HasOverageRejected)
			}
			if !tt.wantEmit {
				return
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
			if !sig.ResetAt.Equal(tt.wantResetAt) {
				t.Fatalf("reset_at = %v, want %v", sig.ResetAt, tt.wantResetAt)
			}
			if sig.ObservedAt.IsZero() {
				t.Fatalf("observed_at must be set")
			}
		})
	}
}

// Test200OverageRejectedDoesNotEmitSignal pins the throttle-policy contract:
// a 200 response carrying anthropic-ratelimit-unified-overage-status: rejected
// and representative-claim: five_hour MUST NOT emit a rotator throttle signal,
// because Anthropic served the request and continues to serve other concurrent
// requests on the same account. Only HTTP 429 emits.
func Test200OverageRejectedDoesNotEmitSignal(t *testing.T) {
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
	if emit {
		t.Fatalf("rateLimitSignal must return (_, false) on 200 with overage rejected, got signal %+v", sig)
	}
	if sig.Provider != "" || sig.AccessToken != "" || sig.Claim != "" || sig.HTTPStatus != 0 {
		t.Fatalf("rateLimitSignal must return a zero Signal on no-emit, got %+v", sig)
	}
	if !sig.ResetAt.IsZero() || !sig.ObservedAt.IsZero() {
		t.Fatalf("rateLimitSignal must return zero timestamps on no-emit, got reset=%v observed=%v", sig.ResetAt, sig.ObservedAt)
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
	if recorded[0].AccessToken != "test-token" {
		t.Fatalf("access token = %q, want test-token", recorded[0].AccessToken)
	}
}

// TestClientDoesNotEmitOn200OverageRejected drives do() against a 200 carrying
// overage-status: rejected and asserts the client reports no signal. The
// header is advisory on a 200: Anthropic served the request and keeps serving
// other simultaneous requests on the same account, so the rotator must not
// throttle. Only 429 emits.
func TestClientDoesNotEmitOn200OverageRejected(t *testing.T) {
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

	if recorded := sink.recorded(); len(recorded) != 0 {
		t.Fatalf("sink received %d signals on 200 with overage rejected, want 0", len(recorded))
	}
}

// TestClientDoesNotEmitOnCleanSuccess drives do() against a clean 200 and
// asserts the client reports nothing.
func TestClientDoesNotEmitOnCleanSuccess(t *testing.T) {
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

	if recorded := sink.recorded(); len(recorded) != 0 {
		t.Fatalf("sink received %d signals on clean success, want 0", len(recorded))
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
