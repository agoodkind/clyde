package daemon

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

func TestStopAdapterProcessReturnsImmediatelyDuringReloadDrain(t *testing.T) {
	cancelCalled := false
	proc := &adapterProcess{
		cancel:         func() { cancelCalled = true },
		done:           make(chan struct{}),
		reloadDraining: atomic.Bool{},
	}
	proc.reloadDraining.Store(true)

	start := time.Now()
	stopAdapterProcess(proc, 200*time.Millisecond)
	elapsed := time.Since(start)

	if !cancelCalled {
		t.Fatal("stopAdapterProcess did not call cancel")
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("stopAdapterProcess took %s during reload drain, want immediate return", elapsed)
	}
}

func TestStopAdapterProcessWaitsForTimeoutOutsideReloadDrain(t *testing.T) {
	cancelCalled := false
	proc := &adapterProcess{
		cancel:         func() { cancelCalled = true },
		done:           make(chan struct{}),
		reloadDraining: atomic.Bool{},
	}

	timeout := 30 * time.Millisecond
	start := time.Now()
	stopAdapterProcess(proc, timeout)
	elapsed := time.Since(start)

	if !cancelCalled {
		t.Fatal("stopAdapterProcess did not call cancel")
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("stopAdapterProcess returned in %s, want it to wait near %s", elapsed, timeout)
	}
}

func TestStartLiveWorkerReloadDrainReturnsImmediatelyAndCompletesLater(t *testing.T) {
	registry := livetrack.New[LiveMeta](livetrack.Options[LiveMeta]{
		Component: "daemon.live",
		Concern:   "daemon.live",
		Log:       slog.Default(),
	})
	sessionHandle, err := registry.Register(context.Background(), "codex.live", LiveMeta{
		Provider:      "codex",
		LiveSessionID: "thread-id",
		WorkerPID:     0,
		Lease:         "background",
	}, &codexRuntimeCloser{runtime: nil})
	if err != nil {
		t.Fatalf("register live worker: %v", err)
	}
	srv := &Server{
		log:         slog.Default(),
		liveWorkers: registry,
	}

	start := time.Now()
	done := startLiveWorkerReloadDrain(context.Background(), slog.Default(), srv)
	elapsed := time.Since(start)
	if done == nil {
		t.Fatal("startLiveWorkerReloadDrain returned nil")
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("startLiveWorkerReloadDrain took %s, want immediate return", elapsed)
	}
	select {
	case <-done:
		t.Fatal("live worker reload drain completed before release")
	default:
	}
	registry.Release(context.Background(), sessionHandle, "test.release")
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("live worker reload drain did not finish")
	}
}
