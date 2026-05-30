package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// VersionFromUserAgent parses a semver-like segment after "claude-cli/"
// in the User-Agent. Returns "" when absent. Keeps the billing-line
// version aligned with the configured User-Agent when both are set.
func VersionFromUserAgent(ua string) string {
	const prefix = "claude-cli/"
	rest, ok := strings.CutPrefix(ua, prefix)
	if !ok {
		return ""
	}
	if space := strings.IndexAny(rest, " ;("); space >= 0 {
		return rest[:space]
	}
	return rest
}

// BuildAttributionHeader returns the body-side billing prefix line for
// the system prompt. version is typically parsed from User-Agent or
// taken from configured cc_version; entrypoint matches cc_entrypoint.
func BuildAttributionHeader(version, entrypoint string) string {
	return "x-anthropic-billing-header: cc_version=" + version + "." + randomHex(3) +
		"; cc_entrypoint=" + entrypoint + "; cch=" + randomHex(5) + ";"
}

func randomHex(nBytes int) string {
	raw := make([]byte, (nBytes+1)/2)
	if _, err := rand.Read(raw); err != nil {
		return strings.Repeat("0", nBytes)
	}
	return hex.EncodeToString(raw)[:nBytes]
}

// billingHeaderLinePrefix marks the system block that carries the
// claude-code billing/attribution line. The attribution block is
// always the first system block claude-code sends and the only block
// whose text begins with this token.
const billingHeaderLinePrefix = "x-anthropic-billing-header:"

// billingCCHMarker is the `cch=` key inside the billing line whose
// value is the captured attestation token.
const billingCCHMarker = "cch="

// SubstituteBillingAttestation replaces the `cch=<placeholder>` value in
// a claude-code billing/attribution line with the captured attestation
// token, leaving the rest of the line untouched. It returns the line
// unchanged when cch is empty (no captured token) or when line is not a
// billing line, so callers can apply it unconditionally. When the line
// carries no `cch=` marker the token is appended in the claude-code
// `cch=<value>;` shape.
func SubstituteBillingAttestation(line, cch string) string {
	cch = strings.TrimSpace(cch)
	if cch == "" {
		return line
	}
	if !strings.HasPrefix(strings.TrimSpace(line), billingHeaderLinePrefix) {
		return line
	}
	before, after, ok := strings.Cut(line, billingCCHMarker)
	if !ok {
		return line + " " + billingCCHMarker + cch + ";"
	}
	_, tail, hasTail := strings.Cut(after, ";")
	if !hasTail {
		return before + billingCCHMarker + cch
	}
	return before + billingCCHMarker + cch + ";" + tail
}
