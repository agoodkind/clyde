package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

type usageWarningProbeConfig struct {
	HTTPClient *http.Client
	BaseURL    string
	Token      string
	AccountID  string
	Now        func() time.Time
}

type whamUsageResponse struct {
	RateLimit whamRateLimit `json:"rate_limit"`
}

type whamRateLimit struct {
	PrimaryWindow   *whamUsageWindow `json:"primary_window"`
	SecondaryWindow *whamUsageWindow `json:"secondary_window"`
}

type whamUsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// ProbeUsageWarnings is part of Clyde's typed adapter surface.
func ProbeUsageWarnings(ctx context.Context, cfg usageWarningProbeConfig) ([]adapterruntime.UsageWindowNoticeInput, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	endpoint, err := whamUsageURL(cfg.BaseURL)
	if err != nil {
		slog.WarnContext(ctx, "adapter.codex.usage_probe.build_url_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("build codex usage URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		slog.WarnContext(ctx, "adapter.codex.usage_probe.create_request_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("create codex usage request: %w", err)
	}
	if token := strings.TrimSpace(cfg.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accountID := strings.TrimSpace(cfg.AccountID); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	req.Header.Set(CodexOriginatorHeader, CodexOriginatorValue)

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "adapter.codex.usage_probe.http_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("perform codex usage request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage probe: unexpected status %d", resp.StatusCode)
	}

	var payload whamUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		slog.WarnContext(ctx, "adapter.codex.usage_probe.decode_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("decode codex usage response: %w", err)
	}

	now := cfg.Now().UTC()
	windows := make([]adapterruntime.UsageWindowNoticeInput, 0, 2)
	if warning, ok := usageWindowNoticeInput("codex", "weekly", payload.RateLimit.SecondaryWindow, now); ok {
		windows = append(windows, warning)
	}
	if warning, ok := usageWindowNoticeInput("codex", "primary", payload.RateLimit.PrimaryWindow, now); ok {
		windows = append(windows, warning)
	}
	return windows, nil
}

func usageWindowNoticeInput(
	provider string,
	windowKey string,
	window *whamUsageWindow,
	now time.Time,
) (adapterruntime.UsageWindowNoticeInput, bool) {
	if window == nil {
		return adapterruntime.UsageWindowNoticeInput{
			Provider: "", WindowKey: "", LimitLabel: "", UsedPercent: 0, ResetsAt: time.
					Time{},

			Kind: "",
		}, false
	}
	limitLabel := usageWindowLabel(window.LimitWindowSeconds)
	if strings.TrimSpace(limitLabel) == "" {
		return adapterruntime.UsageWindowNoticeInput{
			Provider: "", WindowKey: "", LimitLabel: "", UsedPercent: 0, ResetsAt: time.
					Time{},

			Kind: "",
		}, false
	}
	return adapterruntime.UsageWindowNoticeInput{
		Provider:    provider,
		WindowKey:   windowKey,
		LimitLabel:  limitLabel,
		UsedPercent: window.UsedPercent,
		ResetsAt:    usageWindowResetAt(window, now), Kind: "",
	}, true
}

func usageWindowResetAt(window *whamUsageWindow, now time.Time) time.Time {
	if window == nil {
		return time.Time{}
	}
	if window.ResetAt > 0 {
		return time.Unix(window.ResetAt, 0).UTC()
	}
	if window.ResetAfterSeconds > 0 {
		return now.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
	}
	return time.Time{}
}

func usageWindowLabel(limitWindowSeconds int64) string {
	const secondsPerMinute = int64(60)
	const minutesPerHour = int64(60)
	const minutesPerDay = int64(24) * minutesPerHour
	const minutesPerWeek = int64(7) * minutesPerDay
	const minutesPerMonth = int64(30) * minutesPerDay
	const roundingBiasMinutes = int64(3)

	windowMinutes := limitWindowSeconds / secondsPerMinute
	if windowMinutes <= minutesPerDay+roundingBiasMinutes {
		adjustedMinutes := windowMinutes + roundingBiasMinutes
		hours := max(adjustedMinutes/minutesPerHour, 1)
		return fmt.Sprintf("%dh", hours)
	}
	if windowMinutes <= minutesPerWeek+roundingBiasMinutes {
		return "weekly"
	}
	if windowMinutes <= minutesPerMonth+roundingBiasMinutes {
		return "monthly"
	}
	return "annual"
}

func whamUsageURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		slog.Warn("adapter.codex.usage_probe.parse_base_url_failed", "concern", "adapter.providers.codex.request", "base_url", baseURL, "err", err)
		return "", fmt.Errorf("parse codex usage base URL: %w", err)
	}
	parsed.Path = "/backend-api/wham/usage"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
