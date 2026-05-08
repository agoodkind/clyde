package sessionrename

import (
	"crypto/sha256"
	"encoding/hex"
)

// fingerprintLength is the number of hex characters returned by
// fingerprint. SHA-256 truncated to 16 hex chars (64 bits) is more
// than enough to dedupe per-session source text against the same
// session's prior attempt.
const fingerprintLength = 16

// fingerprint hashes the (kind, text) pair so the AutoNameSourceHash
// comparison is a single equality check on a short hex string. The
// kind prefix prevents a customTitle and a first-user-message that
// happen to share the same text from colliding on the same hash.
func fingerprint(kind SourceKind, text string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + text))
	return hex.EncodeToString(sum[:fingerprintLength/2])
}
