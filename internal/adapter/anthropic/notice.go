package anthropic

import (
	"net/http"
	"strconv"
	"strings"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

const (
	// NoticeKindEarlyWarning5H is part of Clyde's typed adapter surface.
	NoticeKindEarlyWarning5H = "early_warning_5h"
	// NoticeKindEarlyWarning7D is part of Clyde's typed adapter surface.
	NoticeKindEarlyWarning7D = "early_warning_7d"
	// NoticeKindEarlyWarningOVR is part of Clyde's typed adapter surface.
	NoticeKindEarlyWarningOVR = "early_warning_overage"
)

// EarlyWarningUsageWindows is part of Clyde's typed adapter surface.
func EarlyWarningUsageWindows(h http.Header) []adapterruntime.UsageWindowNoticeInput {
	if ClassifyHeaders(h, http.StatusOK).Class == ResponseClassSuccessNoWarning {
		return nil
	}
	windows := make([]adapterruntime.UsageWindowNoticeInput, 0, 3)
	for _, claim := range []string{"five_hour", "seven_day", "overage"} {
		headerClaim := earlyWarningHeaderClaim(claim)
		threshold := strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-" + headerClaim + "-Surpassed-Threshold"))
		resetsAt := parseUnix(h.Get("Anthropic-Ratelimit-Unified-" + headerClaim + "-Reset"))
		util, hasUtil := parseUtilization(h.Get("Anthropic-Ratelimit-Unified-" + headerClaim + "-Utilization"))

		if threshold == "" && !hasUtil && resetsAt.IsZero() {
			continue
		}
		if threshold == "" && !hasUtil && !strings.EqualFold(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Status")), "allowed_warning") {
			continue
		}
		if !hasUtil {
			continue
		}
		limitLabel := rateLimitLabel(claim)
		if limitLabel == "" {
			limitLabel = "usage"
		}
		windows = append(windows, adapterruntime.UsageWindowNoticeInput{
			Provider:    "anthropic",
			WindowKey:   claim,
			LimitLabel:  limitLabel,
			UsedPercent: util * 100,
			ResetsAt:    resetsAt,
			Kind:        earlyWarningKind(claim),
		})
	}
	return windows
}

// rateLimitClaim is the closed enum of Anthropic ratelimit-claim
// strings the adapter recognizes on early-warning headers.
type rateLimitClaim string

const (
	rateLimitClaimFiveHour       rateLimitClaim = "five_hour"
	rateLimitClaimSevenDay       rateLimitClaim = "seven_day"
	rateLimitClaimSevenDayOpus   rateLimitClaim = "seven_day_opus"
	rateLimitClaimSevenDaySonnet rateLimitClaim = "seven_day_sonnet"
	rateLimitClaimOverage        rateLimitClaim = "overage"
)

func earlyWarningKind(claim string) string {
	switch rateLimitClaim(claim) {
	case rateLimitClaimFiveHour:
		return NoticeKindEarlyWarning5H
	case rateLimitClaimOverage:
		return NoticeKindEarlyWarningOVR
	case rateLimitClaimSevenDay, rateLimitClaimSevenDayOpus, rateLimitClaimSevenDaySonnet:
		return NoticeKindEarlyWarning7D
	default:
		return NoticeKindEarlyWarning7D
	}
}

func rateLimitLabel(claim string) string {
	switch rateLimitClaim(claim) {
	case rateLimitClaimFiveHour:
		return "session limit"
	case rateLimitClaimSevenDay:
		return "weekly limit"
	case rateLimitClaimSevenDayOpus:
		return "Opus limit"
	case rateLimitClaimSevenDaySonnet:
		return "Sonnet limit"
	case rateLimitClaimOverage:
		return "extra usage limit"
	default:
		return "usage limit"
	}
}

func earlyWarningHeaderClaim(claim string) string {
	switch rateLimitClaim(claim) {
	case rateLimitClaimFiveHour:
		return "5h"
	case rateLimitClaimSevenDay:
		return "7d"
	case rateLimitClaimOverage:
		return "overage"
	case rateLimitClaimSevenDayOpus, rateLimitClaimSevenDaySonnet:
		return "7d"
	default:
		return "7d"
	}
}

func parseUtilization(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	if value > 1 {
		value /= 100
	}
	if value < 0 {
		return 0, false
	}
	return value, true
}
