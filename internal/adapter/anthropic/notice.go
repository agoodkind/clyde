package anthropic

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	NoticeKindOverageActive   = "overage_active"
	NoticeKindOverageWarning  = "overage_warning"
	NoticeKindEarlyWarning5H  = "early_warning_5h"
	NoticeKindEarlyWarning7D  = "early_warning_7d"
	NoticeKindEarlyWarningOVR = "early_warning_overage"
)

// Notice is the in-band billing message emitted by EvaluateNotice.
type Notice struct {
	Kind     string
	Text     string
	ResetsAt time.Time
}

// EvaluateNotice inspects unified Anthropic rate-limit headers from a successful
// messages response and returns one synthetic notice, or nil when no notice applies.
//
// The classifier in classify.go owns the "is there any warning at
// all?" gate; EvaluateNotice short-circuits on
// ResponseClassSuccessNoWarning so the per-claim formatting logic
// only runs when at least one warning flag is set.
func EvaluateNotice(h http.Header, now time.Time, usageThresholdsUsedPercent []float64) *Notice {
	if ClassifyHeaders(h, http.StatusOK).Class == ResponseClassSuccessNoWarning {
		return nil
	}
	status := strings.ToLower(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-status")))
	overageStatus := strings.ToLower(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-overage-status")))
	repClaim := strings.ToLower(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-representative-claim")))

	if status == "rejected" && (overageStatus == "allowed" || overageStatus == "allowed_warning") {
		overageResetAt := parseUnix(h.Get("anthropic-ratelimit-unified-overage-reset"))
		fallbackResetAt := parseUnix(h.Get("anthropic-ratelimit-unified-reset"))
		resetsAt := overageResetAt
		if resetsAt.IsZero() {
			resetsAt = fallbackResetAt
		}

		if overageStatus == "allowed_warning" {
			return &Notice{
				Kind:     NoticeKindOverageWarning,
				Text:     "You're close to your extra usage spending limit",
				ResetsAt: resetsAt,
			}
		}

		return &Notice{
			Kind:     NoticeKindOverageActive,
			Text:     overageActiveText(repClaim, resetsAt, now),
			ResetsAt: resetsAt,
		}
	}

	if status != "allowed_warning" && !hasSurpassedThreshold(h) {
		return nil
	}

	if n := earlyWarningNotice(h, now, usageThresholdsUsedPercent); n != nil {
		return n
	}

	return nil
}

func hasSurpassedThreshold(h http.Header) bool {
	for _, claim := range []string{"5h", "7d", "overage"} {
		if strings.TrimSpace(h.Get("anthropic-ratelimit-unified-"+claim+"-surpassed-threshold")) != "" {
			return true
		}
	}
	return false
}

func earlyWarningNotice(h http.Header, now time.Time, usageThresholdsUsedPercent []float64) *Notice {
	for _, claim := range []string{"five_hour", "seven_day", "overage"} {
		headerClaim := earlyWarningHeaderClaim(claim)
		threshold := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-" + headerClaim + "-surpassed-threshold"))
		resetsAt := parseUnix(h.Get("anthropic-ratelimit-unified-" + headerClaim + "-reset"))
		util, hasUtil := parseUtilization(h.Get("anthropic-ratelimit-unified-" + headerClaim + "-utilization"))

		if threshold == "" && !hasUtil && resetsAt.IsZero() {
			continue
		}
		if threshold == "" && !hasUtil && !strings.EqualFold(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-status")), "allowed_warning") {
			continue
		}

		text := earlyWarningText(claim, util, hasUtil, resetsAt, now, usageThresholdsUsedPercent)
		if text == "" {
			return nil
		}
		kind := earlyWarningKind(claim)
		return &Notice{
			Kind:     kind,
			Text:     text,
			ResetsAt: resetsAt,
		}
	}

	return nil
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

func overageActiveText(claim string, resetsAt time.Time, now time.Time) string {
	limit := "usage"
	if claimName := rateLimitLabel(claim); claimName != "" {
		limit = claimName
	}
	if resetsAt.IsZero() {
		return "You're now using extra usage"
	}
	return fmt.Sprintf("You're now using extra usage · %s resets %s", limit, formatResetTime(resetsAt, now))
}

func earlyWarningText(
	claim string,
	utilization float64,
	hasUtil bool,
	resetsAt time.Time,
	now time.Time,
	usageThresholdsUsedPercent []float64,
) string {
	limit := rateLimitLabel(claim)
	if limit == "" {
		limit = "usage"
	}
	if !hasUtil {
		return ""
	}
	usedPercent := utilization * 100
	highestThreshold, ok := highestUsageThreshold(usedPercent, usageThresholdsUsedPercent)
	if !ok {
		return ""
	}
	used := int(highestThreshold)
	if used < 1 {
		used = 0
	}
	if resetsAt.IsZero() {
		return fmt.Sprintf("You've used %d%% of your %s", used, limit)
	}
	return fmt.Sprintf("You've used %d%% of your %s · resets %s", used, limit, formatResetTime(resetsAt, now))
}

func highestUsageThreshold(usedPercent float64, thresholdsUsedPercent []float64) (float64, bool) {
	var highest float64
	matched := false
	for _, threshold := range thresholdsUsedPercent {
		if usedPercent >= threshold {
			highest = threshold
			matched = true
		}
	}
	return highest, matched
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
