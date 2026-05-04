package codex

import (
	"context"
	"encoding/json"
	"fmt"
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

func ProbeUsageWarnings(ctx context.Context, cfg usageWarningProbeConfig) ([]adapterruntime.UsageWindowNoticeInput, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	endpoint, err := whamUsageURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(cfg.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accountID := strings.TrimSpace(cfg.AccountID); accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	req.Header.Set(CodexOriginatorHeader, CodexOriginatorValue)

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage probe: unexpected status %d", resp.StatusCode)
	}

	var payload whamUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
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
		return adapterruntime.UsageWindowNoticeInput{}, false
	}
	limitLabel := usageWindowLabel(window.LimitWindowSeconds)
	if strings.TrimSpace(limitLabel) == "" {
		return adapterruntime.UsageWindowNoticeInput{}, false
	}
	return adapterruntime.UsageWindowNoticeInput{
		Provider:    provider,
		WindowKey:   windowKey,
		LimitLabel:  limitLabel,
		UsedPercent: window.UsedPercent,
		ResetsAt:    usageWindowResetAt(window, now),
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
		return "", err
	}
	parsed.Path = "/backend-api/wham/usage"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
