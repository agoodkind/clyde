package mitm

import (
	"regexp"
	"strings"
)

// Churn detection for wire-baseline learning. A value churns when it is
// effectively unique per request and so carries no wire identity: request
// byte sizes (content-length), per-session ids, per-request request ids, and
// signed attestation blobs. The detectors here are name-agnostic and decide
// from the value shape, not a hardcoded header or path list, so a new churning
// field is handled without a code change.

// uuidPattern (a canonical hyphenated UUID) is declared in header_canonical.go
// and reused here for churn detection.

// prefixedIDPattern matches a prefixed opaque id such as `cse_01GCDZ4cWW1qo9SHkFuaPZhf`
// or `msg_01ABCdef...`: a short alphabetic prefix, an underscore, then a long
// alphanumeric body. The 10-char minimum body keeps real path words with
// underscores (`count_tokens`, `mcp_servers`, `event_logging`) from matching.
var prefixedIDPattern = regexp.MustCompile(`^[A-Za-z]+_[0-9A-Za-z]{10,}$`)

// highEntropyMinLen is the minimum length before a single bare token is
// considered a high-entropy id (ULID, long hex, attestation blob). Shorter
// tokens like product names and version strings stay below it.
const highEntropyMinLen = 20

// headerValueIsVolatile reports whether a header value churns per request and
// so carries no wire identity. Volatile values are bare integers (sizes and
// counters such as content-length), UUIDs (per-session ids), and long
// high-entropy tokens (request ids, attestation blobs). Stable identity values
// such as product user-agents, comma-joined beta sets, and dotted or dashed
// version strings are not volatile. The decision is from value shape only, so
// no header name is hardcoded.
func headerValueIsVolatile(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}
	if isAllDigits(v) {
		return true
	}
	if uuidPattern.MatchString(v) {
		return true
	}
	return isHighEntropyToken(v)
}

// allValuesVolatile reports whether every observed value of a header is
// volatile-shaped. It is used at snapshot-build time, where corpus dedup may
// have collapsed many churning values to a single representative, so a header
// can look constant yet still be volatile by shape.
func allValuesVolatile(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, v := range values {
		if !headerValueIsVolatile(v) {
			return false
		}
	}
	return true
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// isHighEntropyToken reports whether s is a single long token that mixes
// letters and digits, the shape of ULIDs, long hex hashes, and signed
// attestation blobs. Values carrying spaces or commas are structured or list
// values (user-agents, beta sets) and are never high-entropy tokens.
func isHighEntropyToken(s string) bool {
	if len(s) < highEntropyMinLen {
		return false
	}
	if strings.ContainsAny(s, " ,") {
		return false
	}
	hasLetter := false
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

// templateChurningPath replaces churning id-like segments of a path (or a full
// URL) with the `{id}` placeholder so per-session endpoints collapse to one
// shape. A request path such as `/v1/code/sessions/cse_01.../worker/events`
// becomes `/v1/code/sessions/{id}/worker/events`, while `/v1/messages` is left
// unchanged. Real path words keep their identity because they are neither
// numeric, UUID, prefixed-id, nor high-entropy tokens.
func templateChurningPath(path string) string {
	if path == "" {
		return path
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if pathSegmentIsID(seg) {
			segments[i] = "{id}"
		}
	}
	return strings.Join(segments, "/")
}

// pathSegmentIsID reports whether one path segment is a churning opaque id.
func pathSegmentIsID(seg string) bool {
	if seg == "" {
		return false
	}
	if isAllDigits(seg) && len(seg) >= 7 {
		return true
	}
	if uuidPattern.MatchString(seg) {
		return true
	}
	if prefixedIDPattern.MatchString(seg) {
		return true
	}
	return isHighEntropyToken(seg)
}
