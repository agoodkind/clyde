package claudepath

import (
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// SessionSettingsFileByStorageKey returns the path to a session's
// per-session settings.json file when it exists on disk. The helper
// takes raw strings (clydeRoot, storageKey) so it can be called from
// packages that cannot import the session domain (the daemon, the
// cli probe command, the compact runtime), without re-introducing a
// claudepath -> session dependency.
//
// Callers compute the storage key from a session value via
// sess.StorageKey() and pass the result here. Returns the empty
// string when any of the inputs are missing or when the file does
// not exist on disk.
func SessionSettingsFileByStorageKey(clydeRoot, storageKey string) string {
	if strings.TrimSpace(clydeRoot) == "" || strings.TrimSpace(storageKey) == "" {
		return ""
	}
	settingsPath := filepath.Join(config.GetSessionDir(clydeRoot, storageKey), "settings.json")
	info, err := os.Stat(settingsPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return ""
	}
	return settingsPath
}
