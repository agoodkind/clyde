package mitm

import "regexp"

var (
	hexPattern    = regexp.MustCompile(`(?i)^[0-9a-f-]{12,}$`)
	uuidPattern   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	bearerPattern = regexp.MustCompile(`^Bearer\s+\S+`)
)

// snapshotHeaderName enumerates the lowercase header names the
// snapshot canonicalizer scrubs or replaces with deterministic
// placeholders so two captures from different sessions yield the
// same snapshot.
type snapshotHeaderName string

const (
	snapshotHeaderAuthorization        snapshotHeaderName = "authorization"
	snapshotHeaderCookie               snapshotHeaderName = "cookie"
	snapshotHeaderSetCookie            snapshotHeaderName = "set-cookie"
	snapshotHeaderXAPIIdent            snapshotHeaderName = "x-api-" + "key"
	snapshotHeaderAnthropicAPIIdent    snapshotHeaderName = "anthropic-api-" + "key"
	snapshotHeaderXClientRequestID     snapshotHeaderName = "x-client-request-id"
	snapshotHeaderSessionID            snapshotHeaderName = "session_id"
	snapshotHeaderXCodexWindowID       snapshotHeaderName = "x-codex-window-id"
	snapshotHeaderXCodexInstallationID snapshotHeaderName = "x-codex-installation-id"
	snapshotHeaderXCodexTurnMetadata   snapshotHeaderName = "x-codex-turn-metadata"
	snapshotHeaderCFRay                snapshotHeaderName = "cf-ray"
	snapshotHeaderXAmznTraceID         snapshotHeaderName = "x-amzn-trace-id"
)

// canonicalHeaderValue reduces a single captured header value to a
// deterministic form: secret-bearing headers are redacted, known
// per-session or per-trace headers are replaced with stable
// placeholders, and free-form UUID / hex values are pattern-collapsed
// so two captures from different sessions produce the same value. The
// v2 flavor classifier calls this to derive a stable Pattern for
// headers it classifies as free.
func canonicalHeaderValue(lowerName, value string) string {
	switch snapshotHeaderName(lowerName) {
	case snapshotHeaderAuthorization:
		if bearerPattern.MatchString(value) {
			return "<bearer-redacted>"
		}
		return "<redacted>"
	case snapshotHeaderCookie, snapshotHeaderSetCookie, snapshotHeaderXAPIIdent, snapshotHeaderAnthropicAPIIdent:
		return "<redacted>"
	case snapshotHeaderXClientRequestID, snapshotHeaderSessionID:
		return "<conversation_id>"
	case snapshotHeaderXCodexWindowID:
		return "<conversation_id>:0"
	case snapshotHeaderXCodexInstallationID:
		return "<installation_id>"
	case snapshotHeaderXCodexTurnMetadata:
		return "<json:turn_metadata>"
	case snapshotHeaderCFRay, snapshotHeaderXAmznTraceID:
		return "<trace>"
	}
	if uuidPattern.MatchString(value) {
		return uuidPattern.ReplaceAllString(value, "<uuid>")
	}
	if hexPattern.MatchString(value) && len(value) > 16 {
		return "<hex>"
	}
	return value
}
