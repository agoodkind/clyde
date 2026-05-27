package session

import (
	"os"
	"strings"

	"goodkind.io/clyde/internal/providers/claude/claudepath"
)

// EffectiveTranscriptPath returns the best on-disk path for a session's
// provider-native transcript. For Claude sessions it routes through
// claudepath.ResolveTranscriptPath, so a leaked rotator-prefixed value left
// in older metadata (or restored from a backup) transparently falls back to
// the canonical path under <home>/.claude/projects/<encode(workspaceRoot)>/.
// For non-Claude providers it returns ProviderTranscriptPath verbatim.
//
// Callers still need to handle file-open errors. This helper only chooses
// which path to try first.
func EffectiveTranscriptPath(sess *Session) string {
	if sess == nil {
		return ""
	}
	stored := strings.TrimSpace(sess.Metadata.ProviderTranscriptPath())
	if sess.ProviderID() != ProviderClaude {
		return stored
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return stored
	}
	return claudepath.ResolveTranscriptPath(home, stored, sess.Metadata.WorkspaceRoot, sess.Metadata.ProviderSessionID())
}
