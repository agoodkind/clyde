package claudepath

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// WriteModelOverrideSettings writes a one-shot temporary settings.json that
// copies rawSettings and swaps the "model" field to the provided value. It
// returns the path of the temp file. Callers are responsible for removing the
// file (typically with [os.Remove]) once the launched process exits.
//
// The helper is intentionally generic: it carries no knowledge of the value
// being assigned and is reused wherever Clyde needs to launch claude with a
// transient per-launch model override without mutating the persisted session
// settings.
func WriteModelOverrideSettings(rawSettings map[string]json.RawMessage, model string) (string, error) {
	if rawSettings == nil {
		rawSettings = map[string]json.RawMessage{}
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		slog.Warn("claude.settings_overlay.model_json_failed", "component", "claude.claudepath", "err", err)
		return "", fmt.Errorf("marshal override model: %w", err)
	}
	rawSettings["model"] = modelJSON
	content, err := json.MarshalIndent(rawSettings, "", "  ")
	if err != nil {
		slog.Warn("claude.settings_overlay.marshal_failed", "component", "claude.claudepath", "err", err)
		return "", fmt.Errorf("marshal override settings: %w", err)
	}
	tmpFile, err := os.CreateTemp("", "clyde-claude-settings-*.json")
	if err != nil {
		slog.Warn("claude.settings_overlay.temp_create_failed", "component", "claude.claudepath", "err", err)
		return "", fmt.Errorf("create override settings temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(content); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		slog.Warn("claude.settings_overlay.temp_write_failed", "component", "claude.claudepath", "path", tmpPath, "err", err)
		return "", fmt.Errorf("write override settings temp file %q: %w", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		slog.Warn("claude.settings_overlay.temp_close_failed", "component", "claude.claudepath", "path", tmpPath, "err", err)
		return "", fmt.Errorf("close override settings temp file %q: %w", tmpPath, err)
	}
	return tmpPath, nil
}
