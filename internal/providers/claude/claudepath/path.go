// Package claudepath holds pure helpers for the on-disk layout claude uses
// under ~/.claude/projects/. It depends only on the standard library so any
// package can compute the canonical transcript path without pulling in the
// rest of the claude provider (which depends on the session domain).
package claudepath

import (
	"os"
	"path/filepath"
	"strings"
)

// EncodeWorkspaceRoot encodes a filesystem path using Claude Code's project
// directory naming rule: replace / and . with -. This matches the encoded
// directory name claude writes under ~/.claude/projects/.
func EncodeWorkspaceRoot(path string) string {
	encoded := strings.ReplaceAll(path, "/", "-")
	encoded = strings.ReplaceAll(encoded, ".", "-")
	return encoded
}

// ProjectDir returns the encoded project directory name for a clyde root path
// whose parent is the project root (legacy layout: <project>/.claude/clyde).
func ProjectDir(clydeRoot string) string {
	projectRoot := filepath.Dir(filepath.Dir(clydeRoot))
	return EncodeWorkspaceRoot(projectRoot)
}

// TranscriptPath returns the path to a session's transcript file in Claude's
// storage when the project root is the parent of the clyde root.
// Format: <home>/.claude/projects/<project-dir>/<session-id>.jsonl
func TranscriptPath(homeDir, clydeRoot, sessionID string) string {
	return filepath.Join(homeDir, ".claude", "projects", ProjectDir(clydeRoot), sessionID+".jsonl")
}

// TranscriptPathForWorkspace returns the canonical transcript path for a
// session whose original cwd was workspaceRoot. This is the SOT-derived path
// claude itself would use when CLAUDE_CONFIG_DIR is unset, and is the
// rotator-agnostic location that survives launch-credential dir cleanup.
func TranscriptPathForWorkspace(homeDir, workspaceRoot, sessionID string) string {
	return filepath.Join(homeDir, ".claude", "projects", EncodeWorkspaceRoot(workspaceRoot), sessionID+".jsonl")
}

// CanonicalProjectsRoot returns <home>/.claude/projects. Callers use this to
// recognize whether a reported transcript path already lives under the real
// home tree and is therefore safe to persist verbatim.
func CanonicalProjectsRoot(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "projects")
}

// ResolveTranscriptPath returns the best-effort path to a session's transcript
// for code that needs to open the file. The stored value wins when the file
// exists on disk so operator-edited paths and historical placements keep
// working. When the stored value is dead the canonical path derived from
// homeDir + workspaceRoot + sessionID is returned, because that is where
// claude itself reads and writes when CLAUDE_CONFIG_DIR is unset and is the
// symlink target rotator launches point back at. The stored value is the last
// resort, returned only when no canonical can be derived.
//
// The caller is still responsible for opening the file and handling errors;
// this helper only picks which path to try.
func ResolveTranscriptPath(homeDir, stored, workspaceRoot, sessionID string) string {
	stored = strings.TrimSpace(stored)
	if stored != "" && fileExists(stored) {
		return stored
	}
	homeDir = strings.TrimSpace(homeDir)
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	sessionID = strings.TrimSpace(sessionID)
	if homeDir != "" && workspaceRoot != "" && sessionID != "" {
		return TranscriptPathForWorkspace(homeDir, workspaceRoot, sessionID)
	}
	return stored
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
