package deploy

import (
	"bytes"
	"encoding/xml"
	"path/filepath"
	"strconv"
	"strings"
)

func systemdUnitPath(cfg config, unit string) string {
	root := filepath.Join(cfg.Home, ".config")
	for _, variable := range cfg.Environment {
		if variable.Key == "XDG_CONFIG_HOME" && strings.TrimSpace(variable.Value) != "" {
			root = strings.TrimSpace(variable.Value)
			if relative, found := strings.CutPrefix(root, "~/"); found {
				root = filepath.Join(cfg.Home, relative)
			}
		}
	}
	return filepath.Join(root, "systemd", "user", unit)
}

// rootEnvironment preserves the path overrides interpreted by the daemon and
// provider readers. Values are data, never service-manager syntax.
func rootEnvironment(lookup func(string) (string, bool)) []field {
	var result []field
	if lookup == nil {
		return result
	}
	for _, key := range []string{
		"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
		"TMPDIR", "CODEX_HOME", "CODEX_SQLITE_HOME", "CLAUDE_CONFIG_DIR",
		"CLYDE_CURSOR_DATA_DIRS", "CLYDE_ZED_DATA_DIRS",
		"CLYDE_ANTHROPIC_LOG_PATH", "CLYDE_CODEX_LOG_PATH", "CLYDE_SLOG_PATH",
	} {
		if value, ok := lookup(key); ok {
			result = append(result, field{Key: key, Value: value})
		}
	}
	return result
}

func (executor *executor) renderRootEnvironment() string {
	var result strings.Builder
	for _, variable := range executor.config.Environment {
		if executor.config.Platform == platformDarwin {
			var value bytes.Buffer
			_ = xml.EscapeText(&value, []byte(variable.Value))
			result.WriteString("        <key>" + variable.Key + "</key>\n        <string>" + value.String() + "</string>\n")
		} else {
			value := strings.ReplaceAll(variable.Value, "%", "%%")
			result.WriteString("Environment=" + strconv.Quote(variable.Key+"="+value) + "\n")
		}
	}
	return strings.TrimSuffix(result.String(), "\n")
}
