package mitm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"goodkind.io/gklog"

	"goodkind.io/clyde/internal/config"
)

// TestCaptureWriterCacheCloseUnlocksFlock confirms the per-proxy
// cached lumberjack flock is released when close runs. Without the
// release, a new daemon generation could not acquire LOCK_EX on the
// same path because the old generation's writer would still hold the
// lock.
func TestCaptureWriterCacheCloseUnlocksFlock(t *testing.T) {
	cache := newCaptureWriterCache(slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if err := cache.writeLine(dir, []byte(`{"probe":"first"}`), policy); err != nil {
		t.Fatalf("write capture line: %v", err)
	}

	// While the writer cache still holds the lock, a fresh LOCK_EX on
	// the lock file must fail (non-blocking). This proves the cache
	// held the lock.
	lockPath := path + ".lock"
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock probe: %v", err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatalf("expected probe LOCK_EX_NB to fail while writer cache holds lock")
	}
	_ = probe.Close()

	cache.close()

	freshLock, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open fresh lock: %v", err)
	}
	defer func() { _ = freshLock.Close() }()
	if err := syscall.Flock(int(freshLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("expected fresh LOCK_EX after close; got %v", err)
	}
	if err := syscall.Flock(int(freshLock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock fresh: %v", err)
	}
}

// TestCaptureWriterCacheCloseIdempotent calls close twice; the second
// call against an already-closed cache must be a no-op.
func TestCaptureWriterCacheCloseIdempotent(t *testing.T) {
	cache := newCaptureWriterCache(slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache.close()
	cache.close()
}

// TestCaptureWriterCacheAcquireAfterCloseReturnsSentinel proves that
// late tunnel goroutines racing Proxy.Shutdown observe
// ErrCaptureSinkClosed instead of silently re-creating a writer and
// re-acquiring the flock (CLYDE-299).
func TestCaptureWriterCacheAcquireAfterCloseReturnsSentinel(t *testing.T) {
	cache := newCaptureWriterCache(slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if err := cache.writeLine(dir, []byte(`{"probe":"first"}`), policy); err != nil {
		t.Fatalf("write capture line: %v", err)
	}
	cache.close()
	if !captureWriterCacheClosed(cache) {
		t.Fatalf("cache.closed = false, want true after close")
	}
	_, err := cache.acquire(path, policy.Rotation)
	if !errors.Is(err, ErrCaptureSinkClosed) {
		t.Fatalf("acquire after close err = %v, want ErrCaptureSinkClosed", err)
	}
	err = cache.writeLine(dir, []byte(`{"probe":"after-close"}`), policy)
	if !errors.Is(err, ErrCaptureSinkClosed) {
		t.Fatalf("writeLine after close err = %v, want ErrCaptureSinkClosed", err)
	}
	// And the flock the cache used to hold is in fact released, so
	// the next proxy generation can take LOCK_EX from this same
	// process.
	probe, err := os.OpenFile(path+".lock", os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock probe: %v", err)
	}
	defer func() { _ = probe.Close() }()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("expected fresh LOCK_EX after close; got %v", err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock fresh: %v", err)
	}
}

// TestProxyShutdownLockoutRace exercises the CLYDE-299 race: many
// goroutines call into the writer-acquire path concurrently while
// Shutdown closes the cache. After Shutdown returns, no goroutine
// must end up holding the flock and no goroutine can have re-created
// a writer.
func TestProxyShutdownLockoutRace(t *testing.T) {
	captureDir := t.TempDir()
	policy := CaptureFilePolicy{
		RotationEnabled: true,
		Rotation: gklog.RotationConfig{
			MaxSizeMB:  1,
			MaxBackups: 1,
			MaxAgeDays: 1,
			Compress:   pointerToBool(false),
		},
	}
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	caDir := t.TempDir()
	cfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "ca.crt"),
			KeyPath:  filepath.Join(caDir, "ca.key"),
		},
		CaptureDir: captureDir,
		BodyMode:   "summary",
	}
	proxy, err := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), listener)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	// Prime one writer so we know the cache holds a flock at the start
	// of the race.
	if err := proxy.captureWriters.writeLine(captureDir, []byte(`{"primer":true}`), policy); err != nil {
		t.Fatalf("primer writeLine: %v", err)
	}

	// Two phase race. Phase 1: a fleet of goroutines writes captures
	// in parallel until the test signals shutdown; their job is to
	// keep the writer cache busy across close so close() exercises
	// the contended path. Phase 2: a second fleet starts on a barrier
	// that opens AFTER Shutdown returns; every goroutine in phase 2
	// must observe ErrCaptureSinkClosed. Phase 2 is the strict
	// CLYDE-299 invariant: any writer-acquire after Shutdown must NOT
	// re-create a writer or re-acquire the flock.
	const phase1Goroutines = 32
	const phase2Goroutines = 16
	var (
		wg          sync.WaitGroup
		preShutdown = make(chan struct{})
		postClose   = make(chan struct{})
		sentinelHit atomic.Int64
		otherErrs   atomic.Int64
		successHit  atomic.Int64
		phase2Late  atomic.Int64
	)
	for i := 0; i < phase1Goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-preShutdown
			for {
				err := proxy.captureWriters.writeLine(captureDir, []byte(`{"race":true}`), policy)
				if err == nil {
					successHit.Add(1)
					continue
				}
				if errors.Is(err, ErrCaptureSinkClosed) {
					sentinelHit.Add(1)
					return
				}
				otherErrs.Add(1)
				return
			}
		}()
	}
	for i := 0; i < phase2Goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-postClose
			err := proxy.captureWriters.writeLine(captureDir, []byte(`{"phase2":true}`), policy)
			if !errors.Is(err, ErrCaptureSinkClosed) {
				phase2Late.Add(1)
				return
			}
			sentinelHit.Add(1)
		}()
	}
	close(preShutdown)
	// Wait for phase 1 goroutines to land at least one successful
	// write, proving they actually started racing the cache.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && successHit.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := proxy.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	close(postClose)

	wg.Wait()

	if phase2Late.Load() != 0 {
		t.Fatalf("phase 2 goroutines saw a non-sentinel result after Shutdown: %d", phase2Late.Load())
	}

	if !captureWriterCacheClosed(proxy.captureWriters) {
		t.Fatalf("captureWriters not closed after Shutdown")
	}
	if otherErrs.Load() != 0 {
		t.Fatalf("unexpected non-sentinel errors: %d", otherErrs.Load())
	}
	if sentinelHit.Load() == 0 {
		t.Fatalf("no goroutine observed ErrCaptureSinkClosed; race did not exercise close")
	}
	t.Logf("race: success=%d sentinel=%d", successHit.Load(), sentinelHit.Load())

	// The flock must be available to a fresh opener in this same
	// process after Shutdown returned.
	lockPath := filepath.Join(captureDir, captureIndexFilename) + ".lock"
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock probe: %v", err)
	}
	defer func() { _ = probe.Close() }()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("expected LOCK_EX after Shutdown; got %v", err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock probe: %v", err)
	}

	// And the cache held no leftover writers.
	proxy.captureWriters.mu.Lock()
	leftover := len(proxy.captureWriters.writers)
	proxy.captureWriters.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("captureWriters has %d leftover writers after close, want 0", leftover)
	}
}

// TestProxyReloadSequenceLeavesNoAppendFailed simulates the daemon
// reload sequence: an old proxy serves a few captures, Shutdown
// returns, and a brand-new proxy on the same capture dir takes over.
// The new proxy's writer-cache acquire path must succeed without any
// flock contention. This is the CLYDE-270 incident reproduction in
// test form: the original symptom was a burst of append_failed events
// on the new worker because the old worker's writer cache held the
// flock past Shutdown.
func TestProxyReloadSequenceLeavesNoAppendFailed(t *testing.T) {
	captureDir := t.TempDir()
	caDir := t.TempDir()
	cfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "ca.crt"),
			KeyPath:  filepath.Join(caDir, "ca.key"),
		},
		CaptureDir: captureDir,
		BodyMode:   "summary",
	}
	policy := CaptureFilePolicy{
		RotationEnabled: true,
		Rotation: gklog.RotationConfig{
			MaxSizeMB:  1,
			MaxBackups: 1,
			MaxAgeDays: 1,
			Compress:   pointerToBool(false),
		},
	}

	oldListener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen old: %v", err)
	}
	oldProxy, err := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), oldListener)
	if err != nil {
		t.Fatalf("new old proxy: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := oldProxy.captureWriters.writeLine(captureDir, []byte(`{"old":true}`), policy); err != nil {
			t.Fatalf("old proxy write %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := oldProxy.Shutdown(ctx); err != nil {
		t.Fatalf("old proxy shutdown: %v", err)
	}

	newListener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen new: %v", err)
	}
	defer func() { _ = newListener.Close() }()
	newProxy, err := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), newListener)
	if err != nil {
		t.Fatalf("new new proxy: %v", err)
	}
	defer newProxy.closeCaptureWriters()
	for i := 0; i < 5; i++ {
		if err := newProxy.captureWriters.writeLine(captureDir, []byte(`{"new":true}`), policy); err != nil {
			t.Fatalf("new proxy write %d: %v (would surface as mitm.capture.append_failed)", i, err)
		}
	}
}

func pointerToBool(b bool) *bool {
	return &b
}

// captureWriterCacheClosed reads the cache's closed flag under its
// mutex. Used only by tests that want to assert post-shutdown state.
func captureWriterCacheClosed(c *captureWriterCache) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
