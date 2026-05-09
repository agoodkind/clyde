package codexstore

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	sessionsSubdir         = "sessions"
	archivedSessionsSubdir = "archived_sessions"
	sessionIndexFilename   = "session_index.jsonl"
)

// StorePaths mirrors the durable Codex rollout store layout from
// clyde-research/codex/codex-rs/rollout/src/lib.rs and core config.
type StorePaths struct {
	CodexHome           string
	SQLiteHome          string
	SessionsDir         string
	ArchivedSessionsDir string
	SessionIndexPath    string
}

// ResolveStorePaths resolves the Codex data roots Clyde needs for local
// rollout discovery and cleanup.
func ResolveStorePaths(codexHome, sqliteHome string) (StorePaths, error) {
	home, err := resolveCodexHome(codexHome)
	if err != nil {
		return StorePaths{}, err
	}
	sqlite := strings.TrimSpace(sqliteHome)
	if sqlite == "" {
		sqlite = strings.TrimSpace(os.Getenv("CODEX_SQLITE_HOME"))
	}
	if sqlite == "" {
		sqlite = home
	}
	if abs, err := filepath.Abs(sqlite); err == nil {
		sqlite = abs
	}
	return StorePaths{
		CodexHome:           home,
		SQLiteHome:          filepath.Clean(sqlite),
		SessionsDir:         filepath.Join(home, sessionsSubdir),
		ArchivedSessionsDir: filepath.Join(home, archivedSessionsSubdir),
		SessionIndexPath:    filepath.Join(home, sessionIndexFilename),
	}, nil
}

// ResolveStorePathsFromEnv uses CODEX_HOME when set and otherwise falls back
// to ~/.codex, matching Codex's documented home resolution.
func ResolveStorePathsFromEnv() (StorePaths, error) {
	return ResolveStorePaths(os.Getenv("CODEX_HOME"), os.Getenv("CODEX_SQLITE_HOME"))
}

func resolveCodexHome(value string) (string, error) {
	home := strings.TrimSpace(value)
	if home == "" {
		home = strings.TrimSpace(os.Getenv("CODEX_HOME"))
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			// TODO: thread ctx for correlation
			slog.Error("codex.store.home_resolve_failed", "err", err)
			return "", fmt.Errorf("resolve codex home: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	if strings.HasPrefix(home, "~") {
		userHome, err := os.UserHomeDir()
		if err != nil {
			// TODO: thread ctx for correlation
			slog.Error("codex.store.home_expand_failed", "err", err)
			return "", fmt.Errorf("expand codex home: %w", err)
		}
		switch {
		case home == "~":
			home = userHome
		case strings.HasPrefix(home, "~/"):
			home = filepath.Join(userHome, strings.TrimPrefix(home, "~/"))
		}
	}
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	return filepath.Clean(home), nil
}

func rolloutRoots(paths StorePaths) []rolloutRoot {
	return []rolloutRoot{
		{Path: paths.SessionsDir, Archived: false},
		{Path: paths.ArchivedSessionsDir, Archived: true},
	}
}

type rolloutRoot struct {
	Path     string
	Archived bool
}
