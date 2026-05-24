package mitm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// DefaultBaselineRoot returns the user-local directory that stores golden
// MITM wire baselines. Baselines are machine-local because they are generated
// from each user's real upstream clients and should not be committed.
func DefaultBaselineRoot() string {
	return filepath.Join(config.DefaultStateDir(), "mitm-baselines")
}

// DefaultHookStagingRoot returns the user-local directory that holds
// the per-request temp directories the MITM hook dispatcher creates
// for the JSON envelopes and body files it exchanges with hook
// subprocesses. The directory is auto-created on first use and each
// per-request subdirectory is removed after the hook returns.
func DefaultHookStagingRoot() string {
	return filepath.Join(config.DefaultStateDir(), "mitm-hooks")
}

func DefaultDriftLogDir() string {
	return filepath.Join(config.DefaultStateDir(), "mitm-drift")
}

func BaselineReferencePath(root, upstream string, useV2 bool) string {
	filename := "reference.toml"
	if useV2 {
		filename = "reference-v2.toml"
	}
	return filepath.Join(root, strings.TrimSpace(upstream), filename)
}

func FindBaselineReference(root, upstream string) (string, error) {
	name := strings.TrimSpace(upstream)
	if name == "" {
		return "", fmt.Errorf("baseline reference: upstream is required")
	}
	for _, useV2 := range []bool{true, false} {
		path := BaselineReferencePath(root, name, useV2)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("no local MITM baseline for %q under %s; wait for the daemon-owned MITM refresher to create one from live captures or pass --reference", name, filepath.Join(root, name))
}

func BaselineSourceLabel(path string) string {
	root, rootErr := filepath.Abs(DefaultBaselineRoot())
	candidate, candErr := filepath.Abs(path)
	if rootErr == nil && candErr == nil {
		if rel, err := filepath.Rel(root, candidate); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			return filepath.Join("XDG_STATE_HOME", "clyde", "mitm-baselines", rel)
		}
	}
	return path
}
