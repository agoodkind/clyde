package compact

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"goodkind.io/clyde/internal/config"
)

// Calibration captures the static_overhead measurement for one
// session. static_overhead is everything in the live context total
// that does NOT come from the transcript tail: system prompt, tools,
// agents, memory files, and any reactive context blocks the CLI
// injects. It is treated as constant per session.
type Calibration struct {
	StaticOverhead int       `json:"static_overhead"`
	CapturedAt     time.Time `json:"captured_at"`
	Model          string    `json:"model,omitempty"`
}

// SessionStateDir returns the per-session state directory under XDG
// state home: $XDG_STATE_HOME/clyde/sessions/<sessionID>/.
// The directory is created on first call.
func SessionStateDir(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session id required")
	}
	dir := filepath.Join(config.DefaultStateDir(), "sessions", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("compact.calibration.mkdir_failed", "component", "compact", "session_id", sessionID, "dir", dir, "err", err)
		return "", fmt.Errorf("mkdir session state: %w", err)
	}
	return dir, nil
}

func calibrationPath(sessionID string) (string, error) {
	dir, err := SessionStateDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "calibration.json"), nil
}

// LoadCalibration reads the per-session calibration file. ok is false
// when the file does not exist; this is the "not yet calibrated"
// signal callers use to refuse a target.
func LoadCalibration(sessionID string) (Calibration, bool, error) {
	path, err := calibrationPath(sessionID)
	if err != nil {
		return Calibration{StaticOverhead: 0, CapturedAt: time.Time{}, Model: ""}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Calibration{StaticOverhead: 0, CapturedAt: time.Time{}, Model: ""}, false, nil
		}
		slog.Error("compact.calibration.read_failed", "component", "compact", "session_id", sessionID, "path", path, "err", err)
		return Calibration{}, false, fmt.Errorf("read calibration: %w", err)
	}
	var cal Calibration
	if err := json.Unmarshal(data, &cal); err != nil {
		slog.Error("compact.calibration.parse_failed", "component", "compact", "session_id", sessionID, "path", path, "err", err)
		return Calibration{StaticOverhead: 0, CapturedAt: time.Time{}, Model: ""}, false, fmt.Errorf("parse calibration: %w", err)
	}
	return cal, true, nil
}

// SaveCalibration writes the calibration file atomically. Overwrites
// any prior value so re-running --calibrate=N updates in place.
func SaveCalibration(sessionID string, cal Calibration) error {
	if cal.StaticOverhead < 0 {
		return fmt.Errorf("static_overhead must be >= 0")
	}
	if cal.CapturedAt.IsZero() {
		cal.CapturedAt = compactClock.Now().UTC()
	}
	path, err := calibrationPath(sessionID)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(cal, "", "  ")
	if err != nil {
		slog.Error("compact.calibration.encode_failed", "component", "compact", "session_id", sessionID, "err", err)
		return fmt.Errorf("encode calibration: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		slog.Error("compact.calibration.write_failed", "component", "compact", "session_id", sessionID, "path", tmp, "err", err)
		return fmt.Errorf("write calibration: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		slog.Error("compact.calibration.rename_failed", "component", "compact", "session_id", sessionID, "tmp", tmp, "path", path, "err", err)
		return fmt.Errorf("rename calibration: %w", err)
	}
	return nil
}
