package mitm

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"goodkind.io/gklog"
)

// TestReleaseCaptureWritersUnlocksFlock confirms the cached
// rotated-writer flock is released when releaseCaptureWriters runs.
// Without the release, a new daemon generation could not acquire
// LOCK_EX on the same path because the old generation's writer
// would still hold the lock through the cache.
func TestReleaseCaptureWritersUnlocksFlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, captureIndexFilename)
	policy := CaptureFilePolicy{
		RotationEnabled: true,
		Rotation: gklog.RotationConfig{
			MaxSizeMB:  1,
			MaxBackups: 1,
			MaxAgeDays: 1,
			Compress:   pointerToBool(false),
		},
	}
	if err := WriteCaptureLine(dir, []byte(`{"probe":"first"}`), policy); err != nil {
		t.Fatalf("write capture line: %v", err)
	}

	// While the writer cache still holds the lock, a fresh LOCK_EX
	// on the lock file must fail (non-blocking). This proves the
	// cache held the lock.
	lockPath := path + ".lock"
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock probe: %v", err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatalf("expected probe LOCK_EX_NB to fail while writer cache holds lock")
	}
	_ = probe.Close()

	releaseCaptureWriters()

	freshLock, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open fresh lock: %v", err)
	}
	defer func() { _ = freshLock.Close() }()
	if err := syscall.Flock(int(freshLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("expected fresh LOCK_EX after release; got %v", err)
	}
	if err := syscall.Flock(int(freshLock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock fresh: %v", err)
	}
}

// TestReleaseCaptureWritersIdempotent calls release twice; the
// second call against an empty cache must be a no-op.
func TestReleaseCaptureWritersIdempotent(t *testing.T) {
	releaseCaptureWriters()
	releaseCaptureWriters()
}

func pointerToBool(b bool) *bool {
	return &b
}
