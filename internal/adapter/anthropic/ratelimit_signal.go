package anthropic

import (
	"net/http"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

// providerName is the value the rate-limit Signal carries so the rotation
// layer can route the observation to the Anthropic provider's accounts.
const providerName = "anthropic"

// representativeClaim is the closed set of unified representative-claim header
// values this package recognizes. Values arrive on the wire as the
// anthropic-ratelimit-unified-representative-claim header; an unrecognized or
// missing value maps to ClaimUnknown.
type representativeClaim string

const (
	representativeClaimFiveHour     representativeClaim = "five_hour"
	representativeClaimSevenDay     representativeClaim = "seven_day"
	representativeClaimSevenDayOpus representativeClaim = "seven_day_opus"
)

// unknownClaimWarnedOnce guards the unknown-window warning so a given
// unrecognized representative-claim value logs at most once for the lifetime
// of the process. The map is keyed by the raw (normalized) header value; a
// missing value is keyed by the empty string. This is the unknown-window
// warning the plan intended: it lives at the signal boundary, not in
// Classify, which is a pure header/status inspector that must not log. The
// stored value is a [*sync.Once].
var unknownClaimWarnedOnce sync.Map

// rateLimitSignal builds a [ratelimitsink.Signal] from an observed upstream
// response and reports whether the client should emit it. The signal is
// emitted on every response, not only on 429: the rotator's Observe entry
// point records the last status per account so a clean response clears any
// stale rejection within 60s.
//
// accessToken is the bearer token used for the request; it is carried on the
// Signal so the rotation layer can reverse-look-up the slot to update. The
// caller must never log the token.
func rateLimitSignal(class Classification, h http.Header, accessToken string) (ratelimitsink.Signal, bool) {
	claim := mapRepresentativeClaim(h.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	resetAt := earliestReset(
		parseUnix(h.Get("Anthropic-Ratelimit-Unified-Reset")),
		parseUnix(h.Get("Anthropic-Ratelimit-Unified-Overage-Reset")),
	)
	status := mapUnifiedStatus(h.Get("Anthropic-Ratelimit-Unified-Status"), class.Status)

	return ratelimitsink.Signal{
		Provider:    providerName,
		AccessToken: accessToken,
		Status:      status,
		Claim:       claim,
		ResetAt:     resetAt,
		ObservedAt:  anthropicClock.Now(),
		HTTPStatus:  class.Status,
	}, true
}

// unifiedStatusHeader is the closed set of unified-status header values this
// package recognizes. Values arrive on the wire as the
// anthropic-ratelimit-unified-status header; any other value maps to
// StatusUnknown.
type unifiedStatusHeader string

const (
	unifiedStatusAllowed        unifiedStatusHeader = "allowed"
	unifiedStatusAllowedWarning unifiedStatusHeader = "allowed_warning"
	unifiedStatusRejected       unifiedStatusHeader = "rejected"
)

// mapUnifiedStatus maps the wire anthropic-ratelimit-unified-status value to
// a typed [ratelimitsink.Status]. A missing header on a 429 still implies the
// account was rejected, so HTTP 429 with no status header maps to
// StatusRejected; other missing-header cases map to StatusUnknown so the
// rotator does not synthesize a state Anthropic did not report.
func mapUnifiedStatus(raw string, httpStatus int) ratelimitsink.Status {
	value := unifiedStatusHeader(strings.ToLower(strings.TrimSpace(raw)))
	switch value {
	case unifiedStatusAllowed:
		return ratelimitsink.StatusAllowed
	case unifiedStatusAllowedWarning:
		return ratelimitsink.StatusAllowedWarning
	case unifiedStatusRejected:
		return ratelimitsink.StatusRejected
	}
	if httpStatus == http.StatusTooManyRequests {
		return ratelimitsink.StatusRejected
	}
	return ratelimitsink.StatusUnknown
}

// mapRepresentativeClaim maps the unified representative-claim header value to
// a [ratelimitsink.Claim]. Recognized values map to their window; any other or
// missing value maps to ClaimUnknown and logs the unknown-window warning once
// per distinct value so a novel upstream window surfaces without log spam.
func mapRepresentativeClaim(raw string) ratelimitsink.Claim {
	value := representativeClaim(strings.ToLower(strings.TrimSpace(raw)))
	switch value {
	case representativeClaimFiveHour:
		return ratelimitsink.ClaimFiveHour
	case representativeClaimSevenDay:
		return ratelimitsink.ClaimSevenDay
	case representativeClaimSevenDayOpus:
		return ratelimitsink.ClaimSevenDayOpus
	default:
		warnUnknownClaimOnce(string(value))
		return ratelimitsink.ClaimUnknown
	}
}

// warnUnknownClaimOnce logs the unknown representative-claim value at most once
// per distinct value. It loads-or-stores a [*sync.Once] keyed by the value so
// concurrent requests carrying the same unknown window collapse to a single
// log line.
func warnUnknownClaimOnce(value string) {
	loaded, _ := unknownClaimWarnedOnce.LoadOrStore(value, &sync.Once{})
	once, ok := loaded.(*sync.Once)
	if !ok {
		return
	}
	once.Do(func() {
		anthropicRequestLog.Logger().Warn("anthropic.ratelimit.unknown_window",
			"subcomponent", "anthropic",
			"representative_claim", value,
		)
	})
}
