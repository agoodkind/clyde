package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type UsageWarning struct {
	Kind     string
	Text     string
	ResetsAt time.Time
}

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

var usageWarningThresholds = []float64{75, 95}

func ProbeUsageWarnings(ctx context.Context, cfg usageWarningProbeConfig) ([]UsageWarning, error) {
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage probe: unexpected status %d", resp.StatusCode)
	}

	var payload whamUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	now := cfg.Now().UTC()
	warnings := make([]UsageWarning, 0, 2)
	if warning, ok := warningForUsageWindow("weekly", payload.RateLimit.SecondaryWindow, now); ok {
		warnings = append(warnings, warning)
	}
	if warning, ok := warningForUsageWindow("primary", payload.RateLimit.PrimaryWindow, now); ok {
		warnings = append(warnings, warning)
	}
	return warnings, nil
}

func warningForUsageWindow(kindPrefix string, window *whamUsageWindow, now time.Time) (UsageWarning, bool) {
	if window == nil {
		return UsageWarning{}, false
	}
	highestThreshold, ok := highestUsageThreshold(window.UsedPercent)
	if !ok {
		return UsageWarning{}, false
	}
	limitLabel := usageWindowLabel(window.LimitWindowSeconds)
	remainingPercent := 100 - highestThreshold
	return UsageWarning{
		Kind: fmt.Sprintf("codex_%s_remaining_%d", kindPrefix, int(remainingPercent)),
		Text: fmt.Sprintf(
			"Heads up, you have less than %.0f%% of your %s limit left. Run /status for a breakdown.",
			remainingPercent,
			limitLabel,
		),
		ResetsAt: usageWindowResetAt(window, now),
	}, true
}

func highestUsageThreshold(usedPercent float64) (float64, bool) {
	var highest float64
	matched := false
	for _, threshold := range usageWarningThresholds {
		if usedPercent >= threshold {
			highest = threshold
			matched = true
		}
	}
	return highest, matched
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
		hours := adjustedMinutes / minutesPerHour
		if hours < 1 {
			hours = 1
		}
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
