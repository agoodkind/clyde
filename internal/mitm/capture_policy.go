package mitm

import (
	"errors"
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

type captureWriterOwner struct {
	writer *lumberjack.Logger
	lock   *os.File
}

// ErrCaptureSinkClosed is returned by captureWriterCache.acquire and
// any function that depends on it once the cache has been closed.
// CLYDE-299: late shutdown-racing tunnel goroutines used to re-create
// a fresh lumberjack writer plus a fresh flock on the same lock file
// after the old proxy released it, blocking the new proxy generation
// from acquiring its own LOCK_EX. Callers must observe this error and
// stop attempting capture writes through the closed proxy.
var ErrCaptureSinkClosed = errors.New("mitm capture writer cache closed")

// captureWriterCache owns the per-proxy lumberjack rotated writer
// pool plus the flock files that serialize concurrent index writes.
// One cache lives on each *Proxy and is closed exactly once during
// Proxy.Shutdown. After closing, acquire returns ErrCaptureSinkClosed
// so late tunnel goroutines do not re-create writers and do not
// re-acquire the flock.
type captureWriterCache struct {
	mu      sync.Mutex
	writers map[captureWriterKey]*captureWriterOwner
	closed  bool
	log     *slog.Logger
}

func newCaptureWriterCache(log *slog.Logger) *captureWriterCache {
	if log == nil {
		log = slog.Default()
	}
	return &captureWriterCache{
		mu:      sync.Mutex{},
		writers: make(map[captureWriterKey]*captureWriterOwner),
		closed:  false,
		log:     log,
	}
}

// acquire returns the cached writer for path+rotation, creating it on
// first use. Returns ErrCaptureSinkClosed once close has run; callers
// must propagate the error rather than fall back to a new writer.
func (c *captureWriterCache) acquire(path string, rotation gklog.RotationConfig) (*captureWriterOwner, error) {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrCaptureSinkClosed
	}
	if owner, ok := c.writers[key]; ok {
		return owner, nil
	}
	lock, err := lockCaptureIndex(path)
	if err != nil {
		return nil, err
	}
	writer := gklog.NewLumberjackWriterWithConfig(path, rotation)
	owner := &captureWriterOwner{writer: writer, lock: lock}
	c.writers[key] = owner
	return owner, nil
}

// writeLine appends one JSONL capture record. For rotated policies it
// goes through the cache; for non-rotated policies it locks per-write
// and never touches the cache. After close, rotated writes return
// ErrCaptureSinkClosed; non-rotated writes are unaffected because
// they own their flock for the duration of the call.
func (c *captureWriterCache) writeLine(dir string, line []byte, policy CaptureFilePolicy) error {
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.log.Warn("mitm.capture.mkdir_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"capture_dir", dir,
			"err", err,
		)
		return fmt.Errorf("create capture dir: %w", err)
	}
	path := filepath.Join(dir, captureIndexFilename)
	if policy.RotationEnabled {
		owner, err := c.acquire(path, policy.Rotation)
		if err != nil {
			if errors.Is(err, ErrCaptureSinkClosed) {
				return err
			}
			c.log.Warn("mitm.capture.lock_failed",
				"component", "mitm",
				"concern", "providers.mitm.wire",
				"capture_path", path,
				"err", err,
			)
			return fmt.Errorf("lock rotated capture index: %w", err)
		}
		if _, err := owner.writer.Write(append(line, '\n')); err != nil {
			c.log.Warn("mitm.capture.rotated_write_failed",
				"component", "mitm",
				"concern", "providers.mitm.wire",
				"capture_path", path,
				"err", err,
			)
			return fmt.Errorf("write rotated capture event: %w", err)
		}
		return nil
	}
	lock, err := lockCaptureIndex(path)
	if err != nil {
		c.log.Warn("mitm.capture.lock_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"capture_path", path,
			"err", err,
		)
		return fmt.Errorf("lock capture index: %w", err)
	}
	defer unlockCaptureIndex(lock)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		c.log.Warn("mitm.capture.open_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"capture_path", path,
			"err", err,
		)
		return fmt.Errorf("open capture index: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(line, '\n')); err != nil {
		c.log.Warn("mitm.capture.write_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"capture_path", path,
			"err", err,
		)
		return fmt.Errorf("write capture event: %w", err)
	}
	return nil
}

// close drains every cached writer, releases every flock, and marks
// the cache closed so any subsequent acquire returns
// ErrCaptureSinkClosed. Idempotent: a second close is a no-op.
func (c *captureWriterCache) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	owners := c.writers
	c.writers = make(map[captureWriterKey]*captureWriterOwner)
	c.mu.Unlock()
	for key, owner := range owners {
		if owner == nil {
			continue
		}
		if owner.writer != nil {
			if err := owner.writer.Close(); err != nil {
				c.log.Warn("mitm.capture.release_writer_failed",
					"component", "mitm",
					"concern", "providers.mitm.lifecycle",
					"capture_path", key.path,
					"err", err,
				)
			}
		}
		if owner.lock != nil {
			unlockCaptureIndex(owner.lock)
		}
	}
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

func lockCaptureIndex(path string) (*os.File, error) {
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		slog.Warn("mitm.capture.lock_open_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("open capture index lock: %w", err)
	}
	if err := flockCaptureFile(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		slog.Warn("mitm.capture.lock_acquire_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("acquire capture index lock: %w", err)
	}
	return file, nil
}

func unlockCaptureIndex(file *os.File) {
	if file == nil {
		return
	}
	if err := flockCaptureFile(file, syscall.LOCK_UN); err != nil {
		slog.Warn("mitm.capture.unlock_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"path", file.Name(),
			"err", err,
		)
	}
	if err := file.Close(); err != nil {
		slog.Warn("mitm.capture.lock_close_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"path", file.Name(),
			"err", err,
		)
	}
}

func flockCaptureFile(file *os.File, how int) error {
	fd, err := strconv.Atoi(strconv.FormatUint(uint64(file.Fd()), 10))
	if err != nil {
		slog.Warn("mitm.capture.lock_fd_convert_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"path", file.Name(),
			"err", err,
		)
		return fmt.Errorf("convert capture lock file descriptor: %w", err)
	}
	if err := syscall.Flock(fd, how); err != nil {
		slog.Warn("mitm.capture.flock_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"path", file.Name(),
			"err", err,
		)
		return fmt.Errorf("flock capture lock file: %w", err)
	}
	return nil
}
