package zedstore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/homedir"
)

const (
	zedDataDirsEnvVar    = "CLYDE_ZED_DATA_DIRS"
	zedThreadsDBRelative = "threads/threads.db"
)

var zedMetadataChannels = [...]string{"0-stable", "0-preview", "0-nightly", "0-dev"}

// DataRoot names one local Zed data root and its known thread database paths.
type DataRoot struct {
	RootDir         string
	ThreadsDBPath   string
	MetadataDBPaths []string
}

// ResolveDataRoots resolves the configured or default Zed data roots Clyde
// should inspect locally.
func ResolveDataRoots(ctx context.Context, dataDirs string) ([]DataRoot, error) {
	roots, err := resolveDataDirList(ctx, dataDirs)
	if err != nil {
		return nil, err
	}
	out := make([]DataRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, DataRoot{
			RootDir:         root,
			ThreadsDBPath:   filepath.Join(root, zedThreadsDBRelative),
			MetadataDBPaths: metadataDBPaths(root),
		})
	}
	return out, nil
}

// ResolveDataRootsFromEnv resolves the Zed data roots from
// `CLYDE_ZED_DATA_DIRS`.
func ResolveDataRootsFromEnv(ctx context.Context) ([]DataRoot, error) {
	return ResolveDataRoots(ctx, os.Getenv(zedDataDirsEnvVar))
}

func resolveDataDirList(ctx context.Context, dataDirs string) ([]string, error) {
	raw := strings.TrimSpace(dataDirs)
	if raw == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			slog.ErrorContext(ctx, "zed.store.home_resolve_failed", "concern", "providers.zed.store", "err", err)
			return nil, fmt.Errorf("resolve zed home: %w", err)
		}
		raw = filepath.Join(userHome, "Library", "Application Support", "Zed")
	}

	seen := make(map[string]struct{})
	out := make([]string, 0, 1)
	for _, entry := range filepath.SplitList(raw) {
		root := strings.TrimSpace(entry)
		if root == "" {
			continue
		}
		if strings.HasPrefix(root, "~") {
			root = homedir.Expand(root)
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	return out, nil
}

func metadataDBPaths(root string) []string {
	paths := make([]string, 0, len(zedMetadataChannels))
	for _, channel := range zedMetadataChannels {
		paths = append(paths, filepath.Join(root, "db", channel, "db.sqlite"))
	}
	return paths
}
