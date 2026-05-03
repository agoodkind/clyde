package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeUsageWarningsReturnsWeeklyAndPrimaryWarnings(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("path=%q want /backend-api/wham/usage", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("authorization=%q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
			t.Fatalf("chatgpt-account-id=%q", got)
		}
		if got := r.Header.Get(CodexOriginatorHeader); got != CodexOriginatorValue {
			t.Fatalf("originator=%q", got)
		}
		_, _ = w.Write([]byte(`{
            "rate_limit": {
                "primary_window": {
                    "used_percent": 95,
                    "limit_window_seconds": 18000,
                    "reset_after_seconds": 7200
                },
                "secondary_window": {
                    "used_percent": 80,
                    "limit_window_seconds": 604800,
                    "reset_at": 1735689600
                }
            }
        }`))
	}))
	defer server.Close()

	warnings, err := ProbeUsageWarnings(context.Background(), usageWarningProbeConfig{
		HTTPClient:            server.Client(),
		BaseURL:               server.URL + "/backend-api/codex/responses",
		Token:                 "token-123",
		AccountID:             "acct-123",
		Now:                   func() time.Time { return time.Unix(1735682400, 0).UTC() },
		ThresholdsUsedPercent: []float64{75, 95},
	})
	if err != nil {
		t.Fatalf("ProbeUsageWarnings: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings len=%d want 2", len(warnings))
	}
	if warnings[0].Text != "Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown." {
		t.Fatalf("weekly warning=%q", warnings[0].Text)
	}
	if warnings[0].Kind != "codex_weekly_remaining_25" {
		t.Fatalf("weekly kind=%q", warnings[0].Kind)
	}
	if got := warnings[0].ResetsAt.UTC(); !got.Equal(time.Unix(1735689600, 0).UTC()) {
		t.Fatalf("weekly reset=%v", got)
	}
	if warnings[1].Text != "Heads up, you have less than 5% of your 5h limit left. Run /status for a breakdown." {
		t.Fatalf("primary warning=%q", warnings[1].Text)
	}
	if warnings[1].Kind != "codex_primary_remaining_5" {
		t.Fatalf("primary kind=%q", warnings[1].Kind)
	}
}

func TestProbeUsageWarningsSkipsBelowThreshold(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
            "rate_limit": {
                "primary_window": {
                    "used_percent": 74,
                    "limit_window_seconds": 18000
                },
                "secondary_window": {
                    "used_percent": 50,
                    "limit_window_seconds": 604800
                }
            }
        }`))
	}))
	defer server.Close()

	warnings, err := ProbeUsageWarnings(context.Background(), usageWarningProbeConfig{
		HTTPClient:            server.Client(),
		BaseURL:               server.URL + "/backend-api/codex/responses",
		ThresholdsUsedPercent: []float64{75, 95},
	})
	if err != nil {
		t.Fatalf("ProbeUsageWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings=%+v want none", warnings)
	}
}

func TestProbeUsageWarningsUsesConfiguredThresholds(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
            "rate_limit": {
                "secondary_window": {
                    "used_percent": 80,
                    "limit_window_seconds": 604800,
                    "reset_at": 1735689600
                }
            }
        }`))
	}))
	defer server.Close()

	warnings, err := ProbeUsageWarnings(context.Background(), usageWarningProbeConfig{
		HTTPClient:            server.Client(),
		BaseURL:               server.URL + "/backend-api/codex/responses",
		ThresholdsUsedPercent: []float64{90},
	})
	if err != nil {
		t.Fatalf("ProbeUsageWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings=%+v want none", warnings)
	}
}
