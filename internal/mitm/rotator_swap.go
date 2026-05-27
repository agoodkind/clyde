package mitm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

// RotatorSink is the minimal subset of the OAuth rotator that the MITM
// proxy depends on. Declaring the interface inside the MITM package
// keeps it free of a hard import on the rotator implementation; the
// daemon wires the concrete *oauthrotation.Rotator into the proxy at
// startup and the proxy invokes only the three methods below.
type RotatorSink interface {
	// Token returns the access token and account id the rotator wants
	// the next request to carry. The MITM proxy attaches the returned
	// token as the Authorization bearer for the upstream call.
	Token(ctx context.Context, name provider.Name) (string, provider.AccountID, error)
	// TokenAfterAuthFailure recovers from an upstream 401 by returning a
	// fresh token for the same account, or signaling that no fresh token
	// exists. The proxy calls this once per inbound request.
	TokenAfterAuthFailure(ctx context.Context, name provider.Name, failedToken string) (string, error)
	// Observe records the upstream rate-limit shape so the rotator can
	// update its in-memory selection state. The proxy calls it on every
	// upstream response whose bearer the proxy swapped.
	Observe(ctx context.Context, sig ratelimitsink.Signal) error
}

// AnthropicProviderName is the rotator key the MITM proxy uses when
// talking to the Anthropic provider's OAuth slot store. It matches the
// constant the anthropic adapter package uses.
const AnthropicProviderName provider.Name = "anthropic"

// authSchemePrefix + anthropicAccessTokenMarker together form the
// case-sensitive Authorization header value the Claude Code client
// emits on OAuth-authenticated requests. The scheme is the literal
// HTTP authentication scheme name plus a trailing space; the marker is
// the wire-format token prefix Anthropic stamps on OAuth access
// tokens. Service tokens (sk-ant-api03-...) and cookie traffic do not
// carry this combined prefix and flow through MITM untouched.
const (
	authSchemePrefix              = "Bearer "
	anthropicAccessTokenMarker    = "sk-ant-"
	anthropicAccessTokenAuthValue = authSchemePrefix + anthropicAccessTokenMarker
)

// unifiedStatus is the closed set of values the Anthropic rate-limit
// status header carries on the wire. Modelling the values as a named
// string type lets the mapper switch on typed constants rather than
// raw string literals.
type unifiedStatus string

const (
	unifiedStatusAllowed        unifiedStatus = "allowed"
	unifiedStatusAllowedWarning unifiedStatus = "allowed_warning"
	unifiedStatusRejected       unifiedStatus = "rejected"
)

// representativeClaim is the closed set of values the Anthropic
// representative-claim header carries on the wire. Same reason as
// unifiedStatus.
type representativeClaim string

const (
	representativeClaimFiveHour     representativeClaim = "five_hour"
	representativeClaimSevenDay     representativeClaim = "seven_day"
	representativeClaimSevenDayOpus representativeClaim = "seven_day_opus"
)

// swapAuthorizationForRotator inspects req's Authorization header. When
// it carries an Anthropic OAuth bearer, the helper asks the rotator for
// a fresh token via sink.Token and rewrites the header to use the
// rotator-owned token. Returns the access token now on the outbound
// request, or "" when no swap happened (missing header, non-anthropic
// bearer, nil sink, or a rotator error).
//
// A rotator error leaves the request header unchanged and returns
// "" so the caller proceeds with the original bearer rather than
// failing the request outright.
func swapAuthorizationForRotator(req *http.Request, sink RotatorSink, log *slog.Logger) string {
	if sink == nil || req == nil {
		return ""
	}
	header := req.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	if !strings.HasPrefix(header, anthropicAccessTokenAuthValue) {
		return ""
	}
	ctx := req.Context()
	freshToken, account, err := sink.Token(ctx, AnthropicProviderName)
	if err != nil {
		if log != nil {
			log.WarnContext(ctx, "mitm.rotator_swap.token_failed",
				"provider", string(AnthropicProviderName),
				"err", err,
			)
		}
		return ""
	}
	if freshToken == "" {
		return ""
	}
	req.Header.Set("Authorization", authSchemePrefix+freshToken)
	if log != nil {
		log.DebugContext(ctx, "mitm.rotator_swap.swapped",
			"provider", string(AnthropicProviderName),
			"account", string(account),
			"token_fp", tokenFingerprint(freshToken),
		)
	}
	return freshToken
}

// observeAnthropicResponse builds a [ratelimitsink.Signal] from the
// upstream response headers and forwards it to the rotator. The helper
// short-circuits when sink is nil, swappedToken is empty (no swap
// occurred upstream so the rotator has no account to update), or resp
// is nil. The status used in the Signal is resp.StatusCode; this
// mirrors what the adapter passes to Observe via Classification.Status.
//
// A non-nil error from sink.Observe is logged but not propagated: the
// observation is best-effort and must not block client response
// delivery.
func observeAnthropicResponse(ctx context.Context, sink RotatorSink, resp *http.Response, swappedToken string, log *slog.Logger) {
	if sink == nil || resp == nil || swappedToken == "" {
		return
	}
	signal := buildAnthropicSignal(resp.Header, resp.StatusCode, swappedToken)
	if err := sink.Observe(ctx, signal); err != nil {
		if log != nil {
			log.WarnContext(ctx, "mitm.rotator_swap.observe_failed",
				"provider", string(AnthropicProviderName),
				"http_status", resp.StatusCode,
				"err", err,
			)
		}
	}
}

// buildAnthropicSignal mirrors the header parsing the anthropic adapter
// performs in rateLimitSignal. The logic is duplicated here rather than
// imported to keep the MITM package free of a hard dependency on the
// adapter; the shape (Anthropic-Ratelimit-Unified-* headers) is stable
// and is already part of the public wire contract.
func buildAnthropicSignal(h http.Header, httpStatus int, accessToken string) ratelimitsink.Signal {
	claim := mapAnthropicClaim(h.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	resetAt := earliestResetTime(
		parseUnixTime(h.Get("Anthropic-Ratelimit-Unified-Reset")),
		parseUnixTime(h.Get("Anthropic-Ratelimit-Unified-Overage-Reset")),
	)
	status := mapAnthropicUnifiedStatus(h.Get("Anthropic-Ratelimit-Unified-Status"), httpStatus)
	return ratelimitsink.Signal{
		Provider:    string(AnthropicProviderName),
		AccessToken: accessToken,
		Status:      status,
		Claim:       claim,
		ResetAt:     resetAt,
		ObservedAt:  currentTime(),
		HTTPStatus:  httpStatus,
	}
}

// mapAnthropicUnifiedStatus mirrors the adapter's mapUnifiedStatus: it
// translates the wire anthropic-ratelimit-unified-status value into a
// typed [ratelimitsink.Status]. A missing header on a 429 still implies
// rejection; other missing-header cases map to StatusUnknown so the
// rotator does not synthesize a state Anthropic did not report.
func mapAnthropicUnifiedStatus(raw string, httpStatus int) ratelimitsink.Status {
	value := unifiedStatus(strings.ToLower(strings.TrimSpace(raw)))
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

// mapAnthropicClaim mirrors the adapter's mapRepresentativeClaim
// without the unknown-window warning side effect. Unknown values map
// to ClaimUnknown silently here; the adapter still emits the warning
// on its own call path.
func mapAnthropicClaim(raw string) ratelimitsink.Claim {
	value := representativeClaim(strings.ToLower(strings.TrimSpace(raw)))
	switch value {
	case representativeClaimFiveHour:
		return ratelimitsink.ClaimFiveHour
	case representativeClaimSevenDay:
		return ratelimitsink.ClaimSevenDay
	case representativeClaimSevenDayOpus:
		return ratelimitsink.ClaimSevenDayOpus
	default:
		return ratelimitsink.ClaimUnknown
	}
}

// parseUnixTime parses a positive unix-seconds string into a
// [time.Time]. Empty, malformed, or non-positive input yields the zero
// time so caller logic can treat "no reset known" as a single case.
func parseUnixTime(s string) time.Time {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// earliestResetTime returns the soonest non-zero of a and b. The
// rotator uses the earliest reset across the unified and overage
// windows so selection waits for the first window to clear.
func earliestResetTime(a, b time.Time) time.Time {
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

// tokenFingerprint returns a short, non-reversible identifier for an
// access token suitable for slog fields. The full token never leaves
// the rotator; the fingerprint lets operators correlate slot updates
// across logs without leaking the secret. The 12-hex-char prefix gives
// 48 bits of entropy, more than enough to distinguish a handful of
// accounts on a single machine.
func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}
