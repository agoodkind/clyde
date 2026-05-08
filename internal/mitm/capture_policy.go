package mitm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"goodkind.io/gklog"
	"gopkg.in/natefinch/lumberjack.v2"

	"goodkind.io/clyde/internal/config"
)

const captureIndexFilename = "capture.jsonl"

// CaptureFilePolicy is the MITM-local policy for capture index files.
// It deliberately speaks in file rotation terms instead of importing the
// broader process logging setup.
type CaptureFilePolicy struct {
	RotationEnabled bool
	Rotation        gklog.RotationConfig
}

type captureWriterKey struct {
	path       string
	maxSizeMB  int
	maxBackups int
	maxAgeDays int
	compress   bool
}

var captureWriterCache = struct {
	mu      sync.Mutex
	writers map[captureWriterKey]*lumberjack.Logger
}{
	mu:      sync.Mutex{},
	writers: make(map[captureWriterKey]*lumberjack.Logger),
}

func captureFilePolicyFromConfig(cfg config.MITMConfig) CaptureFilePolicy {
	rot := cfg.Capture.Rotation
	enabled := true
	if rot.Enabled != nil {
		enabled = *rot.Enabled
	}
	compress := rot.Compress
	if compress == nil {
		value := false
		compress = &value
	}
	return CaptureFilePolicy{
		RotationEnabled: enabled,
		Rotation: gklog.RotationConfig{
			MaxSizeMB:  capturePositiveInt(rot.MaxSizeMB, 8),
			MaxBackups: capturePositiveInt(rot.MaxBackups, 3),
			MaxAgeDays: capturePositiveInt(rot.MaxAgeDays, 2),
			Compress:   compress,
		},
	}
}

func capturePositiveInt(value int, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

// WriteCaptureLine appends one JSONL capture record using the configured file
// policy.
func WriteCaptureLine(dir string, line []byte, policy CaptureFilePolicy) error {
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("mitm.capture.mkdir_failed", "capture_dir", dir, "err", err)
		return fmt.Errorf("create capture dir: %w", err)
	}
	path := filepath.Join(dir, captureIndexFilename)
	if policy.RotationEnabled {
		writer := captureRotatedWriter(path, policy.Rotation)
		if _, err := writer.Write(append(line, '\n')); err != nil {
			slog.Warn("mitm.capture.rotated_write_failed", "capture_path", path, "err", err)
			return fmt.Errorf("write rotated capture event: %w", err)
		}
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("mitm.capture.open_failed", "capture_path", path, "err", err)
		return fmt.Errorf("open capture index: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(line, '\n')); err != nil {
		slog.Warn("mitm.capture.write_failed", "capture_path", path, "err", err)
		return fmt.Errorf("write capture event: %w", err)
	}
	return nil
}

func captureRotatedWriter(path string, rotation gklog.RotationConfig) *lumberjack.Logger {
	compress := false
	if rotation.Compress != nil {
		compress = *rotation.Compress
	}
	key := captureWriterKey{
		path:       path,
		maxSizeMB:  rotation.MaxSizeMB,
		maxBackups: rotation.MaxBackups,
		maxAgeDays: rotation.MaxAgeDays,
		compress:   compress,
	}
	captureWriterCache.mu.Lock()
	defer captureWriterCache.mu.Unlock()
	writer, ok := captureWriterCache.writers[key]
	if ok {
		return writer
	}
	writer = gklog.NewLumberjackWriterWithConfig(path, rotation)
	captureWriterCache.writers[key] = writer
	return writer
}
