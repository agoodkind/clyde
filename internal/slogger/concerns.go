package slogger

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/gklog"
)

// concernPaths maps each concern key to its relative JSONL path under the
// concern root. Subsystem files populate this map via registerConcernPaths
// during package init.
var concernPaths = make(map[string]string)

// eventConcernRules maps event message prefixes to concern keys for the legacy
// event-based router. Subsystem files append their rules via
// registerEventConcernRules during package init.
//
// Rule ordering matters: more specific prefixes must be registered before
// broader ones for the same subsystem. Each subsystem file preserves its
// internal ordering; cross-subsystem ordering is irrelevant because prefixes
// do not overlap across subsystems.
var eventConcernRules []eventConcernRule

// eventConcernRule is kept as a migration reference for older event names. The
// production concern router no longer depends on it; records must carry the
// explicit concern attr from slogger.For/WithConcern or a narrow
// import-cycle-safe equivalent.
type eventConcernRule struct {
	prefix  string
	concern string
}

// registerConcernPaths merges entries from a subsystem-specific path map into
// the package-level concernPaths registry. Called from each subsystem init.
func registerConcernPaths(paths map[string]string) {
	maps.Copy(concernPaths, paths)
}

// registerEventConcernRules appends subsystem rules to the package-level
// eventConcernRules slice. Called from each subsystem init.
func registerEventConcernRules(rules []eventConcernRule) {
	eventConcernRules = append(eventConcernRules, rules...)
}

// IsKnownConcern reports whether concern is registered by the concern file
// router.
func IsKnownConcern(concern string) bool {
	_, ok := concernPaths[strings.TrimSpace(concern)]
	return ok
}

// ConcernRelPath returns the relative path under the concern root for the
// named concern. Callers join the result with [DefaultConcernRoot] to obtain
// the absolute on-disk JSONL path. The returned path is empty when the concern
// is unknown.
func ConcernRelPath(concern string) string {
	return concernPaths[strings.TrimSpace(concern)]
}

func concernForEvent(message string) string {
	for _, rule := range eventConcernRules {
		if strings.HasPrefix(message, rule.prefix) {
			return rule.concern
		}
	}
	return ""
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
