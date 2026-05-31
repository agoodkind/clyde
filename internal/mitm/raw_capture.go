package mitm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// captureConcernInput is the request shape the concern classifier matches
// capture rules against to bucket one captured exchange.
type captureConcernInput struct {
	Provider            string
	Host                string
	Method              string
	Path                string
	RequestContentType  string
	ResponseContentType string
}

// resolveCaptureConcern returns the concern label for one captured exchange.
// Configured capture rules (or the built-in defaults) win first as curated
// overrides; when none match, the concern is auto-derived from the request via
// [autoClassifyConcern] so every exchange lands in a meaningful per-provider
// bucket instead of a single opaque "unknown".
func resolveCaptureConcern(rules []config.MITMCaptureRouteRule, input captureConcernInput) string {
	if len(rules) == 0 {
		rules = config.DefaultMITMCaptureRouteRules()
	}
	for _, rule := range rules {
		if captureRuleMatches(rule, input) {
			return rule.Concern
		}
	}
	return autoClassifyConcern(input)
}

// autoClassifyConcern derives a stable, low-cardinality concern label from the
// request when no curated rule matched. The label is always provider-prefixed
// so a query can group by provider first; it falls back to "<provider>.other"
// when the path carries no usable token, and to "unknown" only when the
// provider itself is unknown. concern is a descriptive grouping column on the
// capture store (indexed for GROUP BY), not a control-flow value, so deriving
// it heuristically is safe.
func autoClassifyConcern(input captureConcernInput) string {
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	if provider == "" {
		return "unknown"
	}
	if token := concernTokenFromPath(provider, input.Path); token != "" {
		return provider + "." + token
	}
	return provider + ".other"
}

// concernTokenFromPath extracts a single semantic token from a request path.
// It prefers a gRPC "<pkg>.<Xxx>Service/Method" service name, then the first
// path segment that is not a version marker, generic prefix, the provider's own
// name, or an opaque id. Returns "" when nothing meaningful is found.
func concernTokenFromPath(provider, path string) string {
	if svc := grpcServiceToken(path); svc != "" {
		return svc
	}
	skip := map[string]bool{
		"": true, "v1": true, "v2": true, "v3": true, "api": true,
		"backend-api": true, "ces": true, "tev1": true, "rest": true,
		provider: true,
	}
	for seg := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		lower := strings.ToLower(seg)
		if skip[lower] || strings.Contains(lower, ".") || concernSegmentIsIDLike(lower) {
			continue
		}
		if slug := concernSlug(lower); slug != "" {
			return slug
		}
	}
	return ""
}

// grpcServiceToken returns the slugified service name from a gRPC-style first
// path segment ("aiserver.v1.AnalyticsService/Batch" -> "analytics"), or "".
func grpcServiceToken(path string) string {
	first, _, _ := strings.Cut(strings.Trim(path, "/"), "/")
	if !strings.Contains(first, ".") {
		return ""
	}
	for part := range strings.SplitSeq(first, ".") {
		if len(part) > len("Service") && strings.HasSuffix(part, "Service") {
			return concernSlug(strings.TrimSuffix(part, "Service"))
		}
	}
	return ""
}

// concernSegmentIsIDLike reports whether a segment looks like an opaque id (a
// long hex/uuid run) that would only inflate concern cardinality.
func concernSegmentIsIDLike(seg string) bool {
	if len(seg) < 12 {
		return false
	}
	for _, r := range seg {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r == '-':
		default:
			return false
		}
	}
	return true
}

// concernSlug lowercases value and collapses every run of non-alphanumeric
// runes to a single underscore, bounding the result to 40 characters so concern
// labels stay short and queryable.
func concernSlug(value string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 40 {
		return out[:40]
	}
	return out
}

func captureRuleMatches(rule config.MITMCaptureRouteRule, input captureConcernInput) bool {
	if rule.Provider != "" && !strings.EqualFold(string(rule.Provider), strings.TrimSpace(input.Provider)) {
		return false
	}
	if rule.Host != "" && normalizeCaptureHost(rule.Host) != normalizeCaptureHost(input.Host) {
		return false
	}
	if rule.Method != "" && !strings.EqualFold(string(rule.Method), strings.TrimSpace(input.Method)) {
		return false
	}
	if rule.PathExact != "" && rule.PathExact != input.Path {
		return false
	}
	if rule.PathPrefix != "" && !strings.HasPrefix(input.Path, rule.PathPrefix) {
		return false
	}
	if rule.PathContains != "" && !strings.Contains(input.Path, rule.PathContains) {
		return false
	}
	if rule.ContentTypeContains != "" && !captureContentTypeMatches(rule.ContentTypeContains, input) {
		return false
	}
	return rule.Concern != ""
}

func normalizeCaptureHost(host string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
}

func captureContentTypeMatches(needle string, input captureConcernInput) bool {
	needle = strings.ToLower(strings.TrimSpace(needle))
	requestContentType := strings.ToLower(input.RequestContentType)
	responseContentType := strings.ToLower(input.ResponseContentType)
	return strings.Contains(requestContentType, needle) || strings.Contains(responseContentType, needle)
}

// safePathPart sanitizes an arbitrary string (host, upstream slug) into a
// filesystem-safe token. The drift transcript path and the TLS leaf-cert log
// host both derive on-disk or log identifiers from untrusted input, so unsafe
// runes collapse to '-' and the result is bounded to 80 characters.
func safePathPart(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "/" {
		return "root"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.Trim(out, "-.")
	if out == "" {
		return "capture"
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

// headerBlock renders an HTTP status line plus header set into the wire-format
// byte block the intercepted-TLS and hook response writers stream back to the
// hijacked client connection.
func headerBlock(statusLine string, h http.Header) []byte {
	var buf bytes.Buffer
	buf.WriteString(statusLine)
	_ = h.Write(&buf)
	buf.WriteString("\r\n")
	return buf.Bytes()
}

// sha256Hex returns the lowercase hex-encoded SHA-256 digest of body, used to
// fingerprint captured request and response payloads for correlation.
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
