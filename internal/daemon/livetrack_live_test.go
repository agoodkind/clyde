package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	codex "goodkind.io/clyde/internal/providers/codex/lifecycle"
	"goodkind.io/clyde/internal/session"
)

// TestLiveWorkerRegistryRegisterReleaseOnCodexStop verifies that starting and
// stopping a Codex live session increments and decrements the liveWorkers
// registry count so the drain loop sees a clean state after the session ends.
func TestLiveWorkerRegistryRegisterReleaseOnCodexStop(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	runtime := &fakeLiveRuntime{
		startSession: &codex.LiveSession{ThreadID: "codex-thread", WorkDir: tmp, Model: "gpt-test"},
	}
	oldFactory := newCodexLiveRuntime
	newCodexLiveRuntime = func(codex.LiveRuntimeOptions) codex.LiveRuntime {
		return runtime
	}
	defer func() { newCodexLiveRuntime = oldFactory }()

	srv := newTestServer(t)

	if got := srv.liveWorkers.Count(); got != 0 {
		t.Fatalf("initial count = %d, want 0", got)
	}

	resp, err := srv.StartLiveSession(context.Background(), &clydev1.StartLiveSessionRequest{
		Provider: string(session.ProviderCodex),
		Basedir:  tmp,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sessionID := resp.GetSession().GetSessionId()

	if got := srv.liveWorkers.Count(); got != 1 {
		t.Fatalf("after start count = %d, want 1", got)
	}

	if _, err := srv.StopLiveSession(context.Background(), &clydev1.StopLiveSessionRequest{
		SessionId: sessionID,
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := srv.liveWorkers.Count(); got != 0 {
		t.Fatalf("after stop count = %d, want 0", got)
	}
}

// TestLiveWorkerRegistryCodexRegisterRelease uses the gRPC methods directly
// via the test server to confirm the livetrack registry count goes 0->1->0.
func TestLiveWorkerRegistryCodexRegisterRelease(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	runtime := &fakeLiveRuntime{
		startSession: &codex.LiveSession{ThreadID: "codex-t2", WorkDir: tmp, Model: "gpt-test"},
	}
	oldFactory := newCodexLiveRuntime
	newCodexLiveRuntime = func(codex.LiveRuntimeOptions) codex.LiveRuntime { return runtime }
	defer func() { newCodexLiveRuntime = oldFactory }()

	srv := newTestServer(t)

	srv.liveSessions["codex-t2"] = &liveRuntimeSession{
		provider:         session.ProviderCodex,
		name:             "codex-chat-t2",
		id:               "codex-t2",
		basedir:          tmp,
		model:            "gpt-test",
		status:           "idle",
		startedAt:        daemonNow(),
		lastTurnID:       "",
		codexRuntime:     runtime,
		livetrackSession: nil,
	}
	srv.registerCodexLiveSession(context.Background(), srv.liveSessions["codex-t2"], runtime)

	if got := srv.liveWorkers.Count(); got != 1 {
		t.Fatalf("after register count = %d, want 1", got)
	}

	srv.remoteMu.Lock()
	lsess := srv.liveSessions["codex-t2"].livetrackSession
	srv.remoteMu.Unlock()
	if lsess == nil {
		t.Fatal("livetrackSession is nil after registerCodexLiveSession")
	}

	srv.liveWorkers.Release(context.Background(), lsess, "test.release")

	if got := srv.liveWorkers.Count(); got != 0 {
		t.Fatalf("after release count = %d, want 0", got)
	}
}

// TestLiveWorkerRegistryForegroundLeasePreservedWithLivetrack confirms the
// foreground lease suspend/restore flow still works correctly when the
// livetrack registry is active: the Codex runtime is registered on start,
// released on suspend (foreground acquire), and re-registered on restore.
func TestLiveWorkerRegistryForegroundLeasePreservedWithLivetrack(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("codex-chat", "codex-thread")
	setTestProviderIdentity(sess, session.ProviderCodex, "codex-thread")
	sess.Metadata.WorkDir = filepath.Join(tmp, "work")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	startRuntime := &fakeLiveRuntime{}
	restoreRuntime := &fakeLiveRuntime{
		attachSession: &codex.LiveSession{
			ThreadID: "codex-thread",
			WorkDir:  sess.Metadata.WorkDir,
			Model:    "gpt-test",
		},
	}
	oldFactory := newCodexLiveRuntime
	newCodexLiveRuntime = func(codex.LiveRuntimeOptions) codex.LiveRuntime {
		return restoreRuntime
	}
	defer func() { newCodexLiveRuntime = oldFactory }()

	srv := newTestServer(t)

	// Manually insert the session record and register it with livetrack so
	// the test reflects the production flow (startCodexLiveSession does both).
	record := &liveRuntimeSession{
		provider:         session.ProviderCodex,
		name:             "codex-chat",
		id:               "codex-thread",
		basedir:          sess.Metadata.WorkDir,
		model:            "gpt-test",
		status:           "idle",
		startedAt:        daemonNow(),
		lastTurnID:       "",
		codexRuntime:     startRuntime,
		livetrackSession: nil,
	}
	srv.remoteMu.Lock()
	srv.liveSessions["codex-thread"] = record
	srv.remoteMu.Unlock()
	srv.registerCodexLiveSession(context.Background(), record, startRuntime)

	if got := srv.liveWorkers.Count(); got != 1 {
		t.Fatalf("before acquire count = %d, want 1", got)
	}

	acquired, err := srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "codex-chat",
		Provider:    string(session.ProviderCodex),
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !acquired.GetShouldRestore() {
		t.Fatal("should_restore = false, want true")
	}
	if got := srv.liveWorkers.Count(); got != 0 {
		t.Fatalf("after acquire count = %d, want 0", got)
	}

	released, err := srv.ReleaseForegroundSession(context.Background(), &clydev1.ReleaseForegroundSessionRequest{
		LeaseToken: acquired.GetLeaseToken(),
		ExitState:  "ok",
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !released.GetRestored() {
		t.Fatal("restored = false, want true")
	}
	if got := srv.liveWorkers.Count(); got != 1 {
		t.Fatalf("after restore count = %d, want 1", got)
	}
}

// TestLiveWorkerForceCloseKillsWedgedCodexRuntime verifies that the
// livetrack registry's force-close path calls Close on a Codex runtime that
// does not exit before the drain deadline.
func TestLiveWorkerForceCloseKillsWedgedCodexRuntime(t *testing.T) {
	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(srv.Close)

	blocked := make(chan struct{})
	closed := make(chan struct{})
	blockingRuntime := &blockingCloserRuntime{
		blocked: blocked,
		closed:  closed,
	}

	record := &liveRuntimeSession{
		provider:         session.ProviderCodex,
		name:             "wedged-session",
		id:               "wedged-thread",
		basedir:          t.TempDir(),
		model:            "gpt-test",
		status:           "idle",
		startedAt:        daemonNow(),
		lastTurnID:       "",
		codexRuntime:     &fakeLiveRuntime{},
		livetrackSession: nil,
	}
	srv.registerCodexLiveSession(context.Background(), record, blockingRuntime)

	if got := srv.liveWorkers.Count(); got != 1 {
		t.Fatalf("before drain count = %d, want 1", got)
	}

	// Drain with a very short deadline so force-close runs immediately.
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result := srv.liveWorkers.Drain(drainCtx, "test.force_close")

	if result.ForceClosed != 1 {
		t.Fatalf("force_closed = %d, want 1", result.ForceClosed)
	}

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("wedged runtime was not closed by force-close")
	}
}

// TestLiveWorkerForceCloseKillsWedgedClaudeProcess verifies that the
// claudeWorkerCloser sends SIGKILL when the process ignores SIGINT.
func TestLiveWorkerForceCloseKillsWedgedClaudeProcess(t *testing.T) {
	tmp := t.TempDir()

	// A script that installs SIG_IGN for SIGINT and writes a ready marker,
	// then sleeps so the SIGINT from the closer has no effect. The closer
	// must escalate to SIGKILL.
	readyMarker := filepath.Join(tmp, "ready")
	script := filepath.Join(tmp, "worker.py")
	scriptBody := "#!/usr/bin/env python3\n" +
		"import signal, time, pathlib\n" +
		"signal.signal(signal.SIGINT, signal.SIG_IGN)\n" +
		"pathlib.Path(" + `"` + readyMarker + `"` + ").write_text('ready')\n" +
		"while True:\n" +
		"    time.sleep(30)\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command(script)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyMarker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("script did not install SIG_IGN before deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}

	closer := &claudeWorkerCloser{
		proc:           cmd.Process,
		done:           done,
		interruptGrace: 100 * time.Millisecond,
	}
	err := closer.Close("test.force_close")
	if err != nil {
		// A non-nil error from Close means the kill itself failed, which is
		// unexpected on a running process.
		t.Fatalf("close returned unexpected error: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after close")
	}
	if cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatal("process exited successfully, want signal termination")
	}
}

// blockingCloserRuntime is a Codex live runtime whose Close blocks until the
// blocked channel is closed. It signals the closed channel once Close runs.
type blockingCloserRuntime struct {
	blocked <-chan struct{}
	closed  chan<- struct{}
}

func (b *blockingCloserRuntime) Close() error {
	select {
	case <-b.blocked:
	case <-time.After(100 * time.Millisecond):
	}
	close(b.closed)
	return nil
}

// blockingCloserRuntime also satisfies codex.LiveRuntime for use in
// registerCodexLiveSession.
func (b *blockingCloserRuntime) Start(context.Context, codex.LiveStartRequest) (*codex.LiveSession, error) {
	return nil, errors.New("blockingCloserRuntime.Start not implemented")
}

func (b *blockingCloserRuntime) Attach(context.Context, codex.LiveAttachRequest) (*codex.LiveSession, error) {
	return nil, errors.New("blockingCloserRuntime.Attach not implemented")
}

func (b *blockingCloserRuntime) Send(context.Context, codex.LiveSendRequest) (*codex.LiveTurn, error) {
	return nil, errors.New("blockingCloserRuntime.Send not implemented")
}

func (b *blockingCloserRuntime) Stream(context.Context, codex.LiveStreamRequest) (<-chan codex.LiveEvent, error) {
	return nil, errors.New("blockingCloserRuntime.Stream not implemented")
}

func (b *blockingCloserRuntime) Stop(context.Context, codex.LiveStopRequest) error {
	return errors.New("blockingCloserRuntime.Stop not implemented")
}
