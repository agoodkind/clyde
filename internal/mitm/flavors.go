package mitm

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// FlavorSignature uniquely identifies one caller flavor for an
// upstream. Two records with the same signature are considered the
// same flavor and contribute to the same per-flavor reference.
type FlavorSignature struct {
	UserAgent       string
	BetaFingerprint string
	BodyKeys        []string
}

// FlavorSlug returns a stable, filesystem-safe identifier for the
// flavor. Used as the directory name under
// `research/<upstream>/snapshots/<flavor>/`. The slug is a short hex
// hash of the signature so distinct flavors always sort to distinct
// directories even when their human-readable names collide.
func (s FlavorSignature) FlavorSlug() string {
	humanPart := flavorHumanPrefix(s)
	hashPart := flavorHashSuffix(s)
	if humanPart == "" {
		return hashPart
	}
	return humanPart + "-" + hashPart
}

func flavorHumanPrefix(s FlavorSignature) string {
	ua := strings.ToLower(s.UserAgent)
	switch {
	case strings.Contains(ua, "claude-cli"):
		// Distinguish probe vs interactive by the body shape.
		if hasKey(s.BodyKeys, "tools") || hasKey(s.BodyKeys, "system") {
			return "claude-code-interactive"
		}
		if hasKey(s.BodyKeys, "messages") {
			return "claude-code-probe"
		}
		return "claude-code-other"
	case strings.Contains(ua, "codex"):
		return "codex"
	case strings.Contains(ua, "claude.app"):
		return "claude-desktop"
	case strings.Contains(ua, "curl"):
		return "curl"
	case ua == "":
		return "unknown"
	}
	return slugifyUA(ua)
}

func flavorHashSuffix(s FlavorSignature) string {
	keys := append([]string{}, s.BodyKeys...)
	sort.Strings(keys)
	raw := s.UserAgent + "|" + s.BetaFingerprint + "|" + strings.Join(keys, ",")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:4])
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyUA(ua string) string {
	parts := strings.SplitN(ua, "/", 2)
	base := strings.ToLower(parts[0])
	clean := slugRE.ReplaceAllString(base, "-")
	clean = strings.Trim(clean, "-")
	if clean == "" {
		return "unknown"
	}
	if len(clean) > 24 {
		clean = clean[:24]
	}
	return clean
}

// betaFingerprint reduces a comma-joined Anthropic-Beta header value
// to a stable canonical form: lowercase, trim each flag, sort.
// Different orderings of the same flag set produce identical
// fingerprints; flag drift (one upstream version adds or removes a
// flag) produces a different fingerprint.
func betaFingerprint(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	sort.Strings(cleaned)
	return strings.Join(cleaned, ",")
}

func hasKey(keys []string, target string) bool {
	return slices.Contains(keys, target)
}
