package mitm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

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
	writers map[captureWriterKey]*captureWriterOwner
}{
	mu:      sync.Mutex{},
	writers: make(map[captureWriterKey]*captureWriterOwner),
}

type captureWriterOwner struct {
	writer *lumberjack.Logger
	lock   *os.File
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
		owner, err := captureRotatedWriter(path, policy.Rotation)
		if err != nil {
			slog.Warn("mitm.capture.lock_failed", "capture_path", path, "err", err)
			return fmt.Errorf("lock rotated capture index: %w", err)
		}
		if _, err := owner.writer.Write(append(line, '\n')); err != nil {
			slog.Warn("mitm.capture.rotated_write_failed", "capture_path", path, "err", err)
			return fmt.Errorf("write rotated capture event: %w", err)
		}
		return nil
	}
	lock, err := lockCaptureIndex(path)
	if err != nil {
		slog.Warn("mitm.capture.lock_failed", "capture_path", path, "err", err)
		return fmt.Errorf("lock capture index: %w", err)
	}
	defer unlockCaptureIndex(lock)
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

func lockCaptureIndex(path string) (*os.File, error) {
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		slog.Warn("mitm.capture.lock_open_failed", "lock_path", lockPath, "err", err)
		return nil, fmt.Errorf("open capture index lock: %w", err)
	}
	if err := flockCaptureFile(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		slog.Warn("mitm.capture.lock_acquire_failed", "lock_path", lockPath, "err", err)
		return nil, fmt.Errorf("acquire capture index lock: %w", err)
	}
	return file, nil
}

func unlockCaptureIndex(file *os.File) {
	if file == nil {
		return
	}
	if err := flockCaptureFile(file, syscall.LOCK_UN); err != nil {
		slog.Warn("mitm.capture.unlock_failed", "path", file.Name(), "err", err)
	}
	if err := file.Close(); err != nil {
		slog.Warn("mitm.capture.lock_close_failed", "path", file.Name(), "err", err)
	}
}

func flockCaptureFile(file *os.File, how int) error {
	fd, err := strconv.Atoi(strconv.FormatUint(uint64(file.Fd()), 10))
	if err != nil {
		slog.Warn("mitm.capture.lock_fd_convert_failed", "path", file.Name(), "err", err)
		return fmt.Errorf("convert capture lock file descriptor: %w", err)
	}
	if err := syscall.Flock(fd, how); err != nil {
		slog.Warn("mitm.capture.flock_failed", "path", file.Name(), "err", err)
		return fmt.Errorf("flock capture lock file: %w", err)
	}
	return nil
}

func captureRotatedWriter(path string, rotation gklog.RotationConfig) (*captureWriterOwner, error) {
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
	owner, ok := captureWriterCache.writers[key]
	if ok {
		return owner, nil
	}
	lock, err := lockCaptureIndex(path)
	if err != nil {
		return nil, err
	}
	writer := gklog.NewLumberjackWriterWithConfig(path, rotation)
	owner = &captureWriterOwner{writer: writer, lock: lock}
	captureWriterCache.writers[key] = owner
	return owner, nil
}
