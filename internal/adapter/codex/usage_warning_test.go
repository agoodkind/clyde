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
                    "used_percent": 88,
                    "limit_window_seconds": 18000,
                    "reset_after_seconds": 2580
                },
                "secondary_window": {
                    "used_percent": 93,
                    "limit_window_seconds": 604800,
                    "reset_at": 1735855200
                }
            }
        }`))
	}))
	defer server.Close()

	now := time.Unix(1735682400, 0).UTC()
	warnings, err := ProbeUsageWarnings(context.Background(), usageWarningProbeConfig{
		HTTPClient: server.Client(),
		BaseURL:    server.URL + "/backend-api/codex/responses",
		Token:      "token-123",
		AccountID:  "acct-123",
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ProbeUsageWarnings: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings len=%d want 2", len(warnings))
	}
	if warnings[0].Provider != "codex" || warnings[0].WindowKey != "weekly" {
		t.Fatalf("weekly window=%+v", warnings[0])
	}
	if warnings[0].LimitLabel != "weekly" {
		t.Fatalf("weekly limit label=%q", warnings[0].LimitLabel)
	}
	if warnings[0].UsedPercent != 93 {
		t.Fatalf("weekly used percent=%v", warnings[0].UsedPercent)
	}
	if got := warnings[0].ResetsAt.UTC(); !got.Equal(time.Unix(1735855200, 0).UTC()) {
		t.Fatalf("weekly reset=%v", got)
	}
	if warnings[1].Provider != "codex" || warnings[1].WindowKey != "primary" {
		t.Fatalf("primary window=%+v", warnings[1])
	}
	if warnings[1].LimitLabel != "5h" {
		t.Fatalf("primary limit label=%q", warnings[1].LimitLabel)
	}
	if warnings[1].UsedPercent != 88 {
		t.Fatalf("primary used percent=%v", warnings[1].UsedPercent)
	}
	if got := warnings[1].ResetsAt.UTC(); !got.Equal(now.Add(43 * time.Minute)) {
		t.Fatalf("primary reset=%v", got)
	}
}

func TestProbeUsageWarningsReturnsWindowsBelowThresholds(t *testing.T) {
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
		HTTPClient: server.Client(),
		BaseURL:    server.URL + "/backend-api/codex/responses",
	})
	if err != nil {
		t.Fatalf("ProbeUsageWarnings: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings len=%d want 2", len(warnings))
	}
	if warnings[0].UsedPercent != 50 || warnings[1].UsedPercent != 74 {
		t.Fatalf("warnings=%+v", warnings)
	}
}

func TestProbeUsageWarningsReturnsConfiguredWindowsWithoutFiltering(t *testing.T) {
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
		HTTPClient: server.Client(),
		BaseURL:    server.URL + "/backend-api/codex/responses",
	})
	if err != nil {
		t.Fatalf("ProbeUsageWarnings: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings len=%d want 1", len(warnings))
	}
	if warnings[0].UsedPercent != 80 {
		t.Fatalf("used percent=%v", warnings[0].UsedPercent)
	}
	if warnings[0].WindowKey != "weekly" {
		t.Fatalf("window key=%q", warnings[0].WindowKey)
	}
}
