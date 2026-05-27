package hook

import (
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/providers/claude/claudepath"
)

// canonicalTranscriptPath picks the rotator-agnostic path to persist for a
// SessionStart hook payload. Claude's `transcript_path` is the literal path it
// opened, which under the OAuth rotator is rooted at an ephemeral
// $CLAUDE_CONFIG_DIR that cleanupLaunchCredentialDir removes after the launch.
// Persisting that path verbatim leaves a dead pointer in the session metadata
// after the rotator dir is cleaned, which breaks every downstream consumer that
// opens the transcript by stored path (compaction, prober, auto-rename).
//
// Resolution order:
//
//  1. Empty reportedPath returns empty (caller skips the write).
//  2. reportedPath already under <home>/.claude/projects/ is kept verbatim.
//  3. Derive <home>/.claude/projects/<encode(workspaceRoot)>/<sessionID>.jsonl
//     and use it when both inputs are present and the derived file exists.
//  4. EvalSymlinks(reportedPath) and use the resolved target when it lands
//     under <home>/.claude/projects/.
//  5. Derived path again as a last resort when inputs were present, since the
//     file may appear after the hook fires.
//  6. reportedPath as-is when no better answer is available.
func canonicalTranscriptPath(realHome, reportedPath, workspaceRoot, sessionID string) string {
	reportedPath = strings.TrimSpace(reportedPath)
	if reportedPath == "" {
		return ""
	}
	realHome = strings.TrimSpace(realHome)
	if realHome == "" {
		return reportedPath
	}

	canonicalRoot := claudepath.CanonicalProjectsRoot(realHome)
	// Verbatim acceptance is a syntactic check: only paths literally under the
	// real-home projects tree (no symlink hops) skip canonicalization. Paths
	// that look like rotator hops, even if they happen to resolve through a
	// symlink back into the canonical tree, must be rewritten to the
	// rotator-agnostic location.
	if pathUnderDir(reportedPath, canonicalRoot) {
		return reportedPath
	}

	workspaceRoot = strings.TrimSpace(workspaceRoot)
	sessionID = strings.TrimSpace(sessionID)
	if workspaceRoot != "" && sessionID != "" {
		derived := claudepath.TranscriptPathForWorkspace(realHome, workspaceRoot, sessionID)
		if fileExists(derived) {
			return derived
		}
		if resolved, ok := resolveThroughSymlinks(reportedPath); ok && resolvedUnderRoot(resolved, canonicalRoot) {
			return resolved
		}
		return derived
	}

	if resolved, ok := resolveThroughSymlinks(reportedPath); ok && resolvedUnderRoot(resolved, canonicalRoot) {
		return resolved
	}
	return reportedPath
}

// resolvedUnderRoot answers the post-EvalSymlinks containment question. It
// normalizes the root through [filepath.EvalSymlinks] because the resolved
// target may carry platform-specific symlink prefixes (e.g. /var -> /private/var
// on macOS) that an un-resolved root will not match.
func resolvedUnderRoot(resolvedTarget, root string) bool {
	if pathUnderDir(resolvedTarget, root) {
		return true
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	return pathUnderDir(resolvedTarget, resolvedRoot)
}

func pathUnderDir(target, dir string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

func resolveThroughSymlinks(p string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return resolved, true
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
