package properlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAcquireAndRelease(t *testing.T) {
	target := filepath.Join(t.TempDir(), "thing")
	release, err := Acquire(context.Background(), target, Options{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	lockPath := LockPath(target)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock dir at %s: %v", lockPath, err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock dir still present after release: stat err=%v", err)
	}
}

func TestContention(t *testing.T) {
	target := filepath.Join(t.TempDir(), "thing")
	first, err := Acquire(context.Background(), target, Options{
		AcquireTimeout: 5 * time.Second,
		RetryWait:      5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	var (
		secondReleased chan struct{}
		secondErr      error
	)
	secondReleased = make(chan struct{})
	go func() {
		defer close(secondReleased)
		release, err := Acquire(context.Background(), target, Options{
			AcquireTimeout: 5 * time.Second,
			RetryWait:      5 * time.Millisecond,
		})
		if err != nil {
			secondErr = err
			return
		}
		secondErr = release()
	}()

	// The second caller must block while the first holds the lock.
	select {
	case <-secondReleased:
		t.Fatal("second Acquire returned before first released the lock")
	case <-time.After(150 * time.Millisecond):
	}

	if err := first(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	select {
	case <-secondReleased:
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire did not return after first released")
	}
	if secondErr != nil {
		t.Fatalf("second Acquire: %v", secondErr)
	}
}

func TestStaleLockRecovery(t *testing.T) {
	target := filepath.Join(t.TempDir(), "thing")
	lockPath := LockPath(target)
	if err := os.Mkdir(lockPath, lockDirMode); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	release, err := Acquire(context.Background(), target, Options{
		StaleThreshold: 10 * time.Second,
		AcquireTimeout: 2 * time.Second,
		RetryWait:      10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Acquire after stale: %v", err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat after stale steal: %v", err)
	}
	// The fresh lock should have a recent mtime (steal recreated the dir).
	if time.Since(info.ModTime()) > 5*time.Second {
		t.Fatalf("post-steal mtime %v is older than expected", info.ModTime())
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestAcquireTimeout(t *testing.T) {
	target := filepath.Join(t.TempDir(), "thing")
	holder, err := Acquire(context.Background(), target, Options{
		StaleThreshold:       10 * time.Second,
		AcquireTimeout:       5 * time.Second,
		MtimeRefreshInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	t.Cleanup(func() { _ = holder() })

	_, err = Acquire(context.Background(), target, Options{
		StaleThreshold: 10 * time.Second,
		AcquireTimeout: 200 * time.Millisecond,
		RetryWait:      20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("second Acquire: want timeout error, got nil")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("second Acquire: want ErrTimeout, got %v", err)
	}
}

func TestMtimeRefresh(t *testing.T) {
	target := filepath.Join(t.TempDir(), "thing")
	release, err := Acquire(context.Background(), target, Options{
		StaleThreshold:       250 * time.Millisecond,
		MtimeRefreshInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = release() }()

	lockPath := LockPath(target)
	firstInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Wait longer than the stale threshold and confirm the mtime was bumped
	// at least once so the lock would still be considered live by a peer.
	time.Sleep(400 * time.Millisecond)
	secondInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat 2: %v", err)
	}
	if !secondInfo.ModTime().After(firstInfo.ModTime()) {
		t.Fatalf("mtime did not advance: first=%v second=%v", firstInfo.ModTime(), secondInfo.ModTime())
	}
}

// TestReleaseIsIdempotent confirms calling release twice does not return an
// error on the second call. The on-disk lock is removed by the first call;
// the second is a no-op.
func TestReleaseIsIdempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "thing")
	release, err := Acquire(context.Background(), target, Options{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

// TestConcurrentAcquireSerializes ensures multiple goroutines racing for the
// same target serialize through the lock: at any moment at most one holds it.
func TestConcurrentAcquireSerializes(t *testing.T) {
	target := filepath.Join(t.TempDir(), "thing")
	var (
		inside   int
		maxSeen  int
		mu       sync.Mutex
		wg       sync.WaitGroup
		workers  = 8
		holdTime = 20 * time.Millisecond
	)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			release, err := Acquire(context.Background(), target, Options{
				AcquireTimeout: 10 * time.Second,
				RetryWait:      5 * time.Millisecond,
			})
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			mu.Lock()
			inside++
			if inside > maxSeen {
				maxSeen = inside
			}
			current := inside
			mu.Unlock()
			if current > 1 {
				t.Errorf("concurrent holders: %d", current)
			}
			time.Sleep(holdTime)
			mu.Lock()
			inside--
			mu.Unlock()
			if err := release(); err != nil {
				t.Errorf("release: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxSeen != 1 {
		t.Fatalf("maxSeen = %d, want 1", maxSeen)
	}
}
