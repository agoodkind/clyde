// Package anthropic implements Anthropic wire models and helpers.
// response headers. Mirrors the user-visible strings the upstream CLI shows
// (e.g. "You've hit your weekly limit · resets 3:45pm (PDT)") so OpenAI-spec
// clients like Cursor surface something actionable instead of the raw 429
// JSON envelope.
package anthropic

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FormatRateLimitMessage inspects anthropic-ratelimit-unified-* headers on a
// 429 response and returns a human-readable summary, or "" if no unified
// rate-limit signal is present (in which case callers should fall back to
// the raw upstream body).
func FormatRateLimitMessage(h http.Header) string {
	claim := strings.ToLower(h.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	overage := strings.ToLower(h.Get("Anthropic-Ratelimit-Unified-Overage-Status"))
	if claim == "" && overage == "" {
		return ""
	}

	resetAt := parseUnix(h.Get("Anthropic-Ratelimit-Unified-Reset"))
	overageResetAt := parseUnix(h.Get("Anthropic-Ratelimit-Unified-Overage-Reset"))
	overageDisabled := strings.ToLower(h.Get("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"))

	resetMsg := ""
	if !resetAt.IsZero() {
		resetMsg = " · resets " + formatResetTime(resetAt, anthropicClock.Now())
	}

	if overage == "rejected" {
		earliest := earliestReset(resetAt, overageResetAt)
		suffix := ""
		if !earliest.IsZero() {
			suffix = " · resets " + formatResetTime(earliest, anthropicClock.Now())
		}
		if overageDisabled == "out_of_credits" {
			return "You're out of extra usage" + suffix
		}
		return "You've hit your limit" + suffix
	}

	limit := limitNameForClaim(claim)
	return "You've hit your " + limit + resetMsg
}

func limitNameForClaim(claim string) string {
	switch claim {
	case "five_hour":
		return "session limit"
	case "seven_day":
		return "weekly limit"
	case "seven_day_opus":
		return "Opus limit"
	case "seven_day_sonnet":
		return "Sonnet limit"
	default:
		return "usage limit"
	}
}

func parseUnix(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

func earliestReset(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Before(b):
		return a
	default:
		return b
	}
}

// formatResetTime mirrors the upstream CLI: "3:45pm (PDT)" within 24h,
// "Apr 22, 3pm (PDT)" further out. Keeps the lowercase am/pm and bracketed
// timezone abbreviation so output matches what users have seen elsewhere.
func formatResetTime(t, now time.Time) string {
	local := t.Local()
	zone, _ := local.Zone()

	hoursUntil := local.Sub(now).Hours()
	minute := local.Minute()

	timeFmt := "3pm"
	if minute != 0 {
		timeFmt = "3:04pm"
	}

	if hoursUntil > 24 {
		dateFmt := "Jan 2"
		if local.Year() != now.Year() {
			dateFmt = "Jan 2 2006"
		}
		out := local.Format(dateFmt + ", " + timeFmt)
		if zone != "" {
			out += fmt.Sprintf(" (%s)", zone)
		}
		return out
	}

	out := local.Format(timeFmt)
	if zone != "" {
		out += fmt.Sprintf(" (%s)", zone)
	}
	return out
}
