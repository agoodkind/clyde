package mitm

import (
	"path/filepath"

	"goodkind.io/clyde/internal/config"
)

// DefaultHookStagingRoot returns the user-local directory that holds
// the per-request temp directories the MITM hook dispatcher creates
// for the JSON envelopes and body files it exchanges with hook
// subprocesses. The directory is auto-created on first use and each
// per-request subdirectory is removed after the hook returns.
func DefaultHookStagingRoot() string {
	return filepath.Join(config.DefaultStateDir(), "mitm-hooks")
}
