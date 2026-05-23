package anthropic

import (
	"net/http"
	"strconv"
	"strings"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

const (
	NoticeKindEarlyWarning5H  = "early_warning_5h"
	NoticeKindEarlyWarning7D  = "early_warning_7d"
	NoticeKindEarlyWarningOVR = "early_warning_overage"
)

func EarlyWarningUsageWindows(h http.Header) []adapterruntime.UsageWindowNoticeInput {
	if ClassifyHeaders(h, http.StatusOK).Class == ResponseClassSuccessNoWarning {
		return nil
	}
	windows := make([]adapterruntime.UsageWindowNoticeInput, 0, 3)
	for _, claim := range []string{"five_hour", "seven_day", "overage"} {
		headerClaim := earlyWarningHeaderClaim(claim)
		threshold := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-" + headerClaim + "-surpassed-threshold"))
		resetsAt := parseUnix(h.Get("anthropic-ratelimit-unified-" + headerClaim + "-reset"))
		util, hasUtil := parseUtilization(h.Get("anthropic-ratelimit-unified-" + headerClaim + "-utilization"))

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

func earlyWarningKind(claim string) string {
	switch claim {
	case "five_hour":
		return NoticeKindEarlyWarning5H
	case "overage":
		return NoticeKindEarlyWarningOVR
	default:
		return NoticeKindEarlyWarning7D
	}
}

func rateLimitLabel(claim string) string {
	switch claim {
	case "five_hour":
		return "session limit"
	case "seven_day":
		return "weekly limit"
	case "seven_day_opus":
		return "Opus limit"
	case "seven_day_sonnet":
		return "Sonnet limit"
	case "overage":
		return "extra usage limit"
	default:
		return "usage limit"
	}
}

func earlyWarningHeaderClaim(claim string) string {
	switch claim {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "7d"
	case "overage":
		return "overage"
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
