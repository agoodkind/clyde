package parser

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TranscriptPathForSession resolves a Claude session id to its on-disk
// transcript path by scanning ~/.claude/projects for <sessionID>.jsonl. It is
// index-free so a caller that already holds a session id can read the transcript
// without the debounced conversation index. The MITM reorient-injection hook
// uses it: the intercepted /v1/messages request carries the session id at
// metadata.user_id.session_id, which is the transcript file's base name (a fork
// can copy the id into a second file, handled below).
//
// The scan mirrors [Parser.Discover]: it walks the same projects root and skips
// subagent transcripts. When more than one file carries the id (a fork copied
// the id into a second transcript), the most recently modified file wins. The
// second return is false when no transcript is found or the home dir is
// unavailable.
func TranscriptPathForSession(sessionID string) (string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("providers.claude.parser.home_failed", "concern", concern, "component", "claude", "err", err)
		return "", false
	}
	root := filepath.Join(home, ".claude", "projects")
	target := sessionID + ".jsonl"
	best := ""
	var bestMtime time.Time
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, entryErr error) error {
		if entryErr != nil {
			if errors.Is(entryErr, os.ErrPermission) || errors.Is(entryErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("walk Claude transcript entry: %w", entryErr)
		}
		if entry.IsDir() {
			if entry.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(entry.Name(), target) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil
			}
			slog.Warn("providers.claude.parser.session_path_stat_failed", "concern", concern, "component", "claude", "path", path, "err", infoErr)
			return nil
		}
		if best == "" || info.ModTime().After(bestMtime) {
			best = path
			bestMtime = info.ModTime()
		}
		return nil
	})
	if walkErr != nil {
		if !errors.Is(walkErr, os.ErrNotExist) {
			slog.Warn("providers.claude.parser.session_path_walk_failed", "concern", concern, "component", "claude", "root", root, "err", walkErr)
		}
		return "", false
	}
	if best == "" {
		return "", false
	}
	return best, true
}
