package searchacceptance

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/frozensource"
)

// FrozenSourceEnvironment configures a provider's frozen physical data root.
type FrozenSourceEnvironment = frozensource.Environment

// MapFrozenRecord changes only the read path after snapshot verification.
func MapFrozenRecord(snapshotRoot, originalHome string, record conversation.Record, manifestFiles map[string]bool) (conversation.Record, error) {
	mapped, err := frozensource.MapFrozenRecord(snapshotRoot, originalHome, record, manifestFiles)
	if err != nil {
		slog.Warn("frozen source mapping failed", "err", err)
		return mapped, fmt.Errorf("map frozen source: %w", err)
	}
	return mapped, nil
}

// FrozenProviderEnvironment returns the frozen provider root without changing
// the process environment.
func FrozenProviderEnvironment(snapshotRoot string, provider conversation.Provider) (FrozenSourceEnvironment, error) {
	environment, err := frozensource.ProviderEnvironment(snapshotRoot, provider)
	if err != nil {
		slog.Warn("frozen provider environment failed", "err", err)
		return environment, fmt.Errorf("resolve frozen provider environment: %w", err)
	}
	return environment, nil
}

// FrozenSourceEnvironments binds both Cursor source formats and Zed to the snapshot.
func FrozenSourceEnvironments(snapshotRoot string) ([]FrozenSourceEnvironment, error) {
	environments, err := frozensource.Environments(snapshotRoot)
	if err != nil {
		slog.Warn("frozen source environments failed", "err", err)
		return nil, fmt.Errorf("resolve frozen source environments: %w", err)
	}
	return environments, nil
}

func cleanAbsoluteRoot(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

func frozenManifestPath(snapshotRoot, relative string, manifest map[string]bool) (string, error) {
	path, err := frozensource.ManifestPath(snapshotRoot, relative, manifest)
	if err != nil {
		slog.Warn("frozen manifest path resolution failed", "err", err)
		return "", fmt.Errorf("resolve frozen manifest path: %w", err)
	}
	return path, nil
}
