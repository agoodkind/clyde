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
