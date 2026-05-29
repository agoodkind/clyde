package slogger

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/gklog"
)

// concernPaths maps each concern key to its relative JSONL path
// under the concern root. Subsystem files populate this map via
// registerConcernPaths during package init.
var concernPaths = make(map[string]string)

// registerConcernPaths merges entries from a subsystem-specific path
// map into the package-level concernPaths registry. Called from each
// subsystem init.
func registerConcernPaths(paths map[string]string) {
	maps.Copy(concernPaths, paths)
}

// IsKnownConcern reports whether concern is registered by the
// concern file router.
func IsKnownConcern(concern string) bool {
	_, ok := concernPaths[strings.TrimSpace(concern)]
	return ok
}

// ConcernRelPath returns the relative path under the concern root
// for the named concern. Callers join the result with
// [DefaultConcernRoot] to obtain the absolute on-disk JSONL path.
// The returned path is empty when the concern is unknown.
func ConcernRelPath(concern string) string {
	return concernPaths[strings.TrimSpace(concern)]
}

func concernHandlers(root string, level slog.Level, rotation RotationPolicy, policies map[string]ConcernPolicy) []slog.Handler {
	handlers := make([]slog.Handler, 0, len(concernPaths))
	for concern, rel := range concernPaths {
		handlerLevel := level
		rot := gklog.RotationConfig{}
		if rotation.Enabled {
			rot = rotationConfig(rotation)
		}
		policy, ok := policies[concern]
		if ok && policy.Enabled != nil && !*policy.Enabled {
			continue
		}
		if ok && policy.Level != nil {
			handlerLevel = *policy.Level
		}
		if ok && policy.Rotation != nil {
			rot = rotationConfigForConcern(*policy.Rotation)
		}
		path := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		handlers = append(handlers, newConcernFilterHandler(concern, gklog.FileJSON(path, handlerLevel, rot)))
	}
	return handlers
}

func rotationConfigForConcern(policy RotationPolicy) gklog.RotationConfig {
	if !policy.Enabled {
		return gklog.RotationConfig{}
	}
	return rotationConfig(policy)
}
