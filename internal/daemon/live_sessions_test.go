package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	codex "goodkind.io/clyde/internal/providers/codex/lifecycle"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/webapp"
)

func TestLiveSessionLaunchBasedirRequiresExplicitOrStoredDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	explicit := filepath.Join(tmp, "explicit")
	got, err := liveSessionLaunchBasedir("", explicit)
	if err != nil {
		t.Fatalf("explicit basedir: %v", err)
	}
	if got != explicit {
		t.Fatalf("explicit basedir = %q want %q", got, explicit)
	}

	if _, err := liveSessionLaunchBasedir("", ""); err == nil {
		t.Fatalf("missing basedir returned nil error")
	} else if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing basedir code = %v", status.Code(err))
	}

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	stored := session.NewSession("stored-chat", "session-id")
	stored.Metadata.WorkDir = filepath.Join(tmp, "stored-workdir")
	if err := store.Create(stored); err != nil {
		t.Fatalf("create stored session: %v", err)
	}
	got, err = liveSessionLaunchBasedir("stored-chat", "")
	if err != nil {
		t.Fatalf("stored basedir: %v", err)
	}
	if got != stored.Metadata.WorkDir {
		t.Fatalf("stored basedir = %q want %q", got, stored.Metadata.WorkDir)
	}
}

func TestForegroundLeaseReturnsInternalWhenUUIDAllocationFails(t *testing.T) {
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
	srv := newTestServer(t)
	uuid.DisableRandPool()
	uuid.SetRand(strings.NewReader(""))
	t.Cleanup(func() {
		uuid.SetRand(nil)
		uuid.DisableRandPool()
	})

	_, err = srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "codex-chat",
		Provider:    string(session.ProviderCodex),
	})
	if err == nil {
		t.Fatal("acquire returned nil error, want UUID allocation error")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("acquire code = %v, want %v", status.Code(err), codes.Internal)
	}
	if !strings.Contains(err.Error(), "generate lease token") {
		t.Fatalf("acquire error = %q, want lease token allocation error", err.Error())
	}
}

func TestForegroundLeaseSuspendsAndRestoresCodexLiveRuntime(t *testing.T) {
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
	srv := newTestServer(t)
	startRuntime := &fakeLiveRuntime{}
	restoreRuntime := &fakeLiveRuntime{attachSession: &codex.LiveSession{ThreadID: "codex-thread", WorkDir: sess.Metadata.WorkDir, Model: "gpt-test"}}
	oldFactory := newCodexLiveRuntime
	factoryCalls := 0
	newCodexLiveRuntime = func(codex.LiveRuntimeOptions) codex.LiveRuntime {
		factoryCalls++
		return restoreRuntime
	}
	defer func() { newCodexLiveRuntime = oldFactory }()

	srv.liveSessions["codex-thread"] = &liveRuntimeSession{
		provider:     session.ProviderCodex,
		name:         "codex-chat",
		id:           "codex-thread",
		basedir:      sess.Metadata.WorkDir,
		model:        "gpt-test",
		status:       "idle",
		startedAt:    daemonNow(),
		codexRuntime: startRuntime,
	}

	acquired, err := srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "codex-chat",
		Provider:    string(session.ProviderCodex),
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !acquired.GetShouldRestore() {
		t.Fatalf("should_restore = false, want true")
	}
	if !startRuntime.closed {
		t.Fatalf("start runtime was not closed")
	}
	if _, ok := srv.liveSessions["codex-thread"]; ok {
		t.Fatalf("codex live session still present after acquire")
	}

	released, err := srv.ReleaseForegroundSession(context.Background(), &clydev1.ReleaseForegroundSessionRequest{
		LeaseToken: acquired.GetLeaseToken(),
		ExitState:  "ok",
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !released.GetRestored() {
		t.Fatalf("restored = false, want true")
	}
	if factoryCalls != 1 {
		t.Fatalf("factory calls = %d, want 1", factoryCalls)
	}
	if restoreRuntime.attachedThread != "codex-thread" {
		t.Fatalf("attached thread = %q, want codex-thread", restoreRuntime.attachedThread)
	}
	if _, ok := srv.liveSessions["codex-thread"]; !ok {
		t.Fatalf("codex live session not restored")
	}
}

func TestSendLiveSessionDeliversToClaudeInjectSocket(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	shortRun := shortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", shortRun)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("claude-chat", "claude-session")
	setTestProviderIdentity(sess, session.ProviderClaude, "claude-session")
	sess.Metadata.WorkDir = filepath.Join(tmp, "work")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	socketPath := injectSocketPath("claude-session")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatalf("mkdir inject dir: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen inject socket: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	received := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		payload, _ := io.ReadAll(conn)
		received <- string(payload)
	}()

	srv := newTestServer(t)
	resp, err := srv.SendLiveSession(context.Background(), &clydev1.SendLiveSessionRequest{
		SessionId: "claude-session",
		Text:      "hello claude",
	})
	if err != nil {
		t.Fatalf("send live session: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("accepted = false, want true")
	}
	select {
	case got := <-received:
		if got != "hello claude\n" {
			t.Fatalf("injected payload = %q want %q", got, "hello claude\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for injected payload")
	}
}

func TestListLiveSessionsIncludesClaudeRemoteWorkers(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	srv := newTestServer(t)
	srv.remoteMu.Lock()
	srv.remoteWorkers["claude-session"] = &remoteWorker{
		sessionName: "claude-chat",
		sessionID:   "claude-session",
		basedir:     filepath.Join(tmp, "work"),
		incognito:   true,
	}
	srv.remoteMu.Unlock()

	resp, err := srv.ListLiveSessions(context.Background(), &clydev1.ListLiveSessionsRequest{})
	if err != nil {
		t.Fatalf("list live sessions: %v", err)
	}
	if len(resp.GetSessions()) != 1 {
		t.Fatalf("live session count = %d want 1", len(resp.GetSessions()))
	}
	got := resp.GetSessions()[0]
	if got.GetProvider() != string(session.ProviderClaude) || got.GetSessionName() != "claude-chat" || got.GetSessionId() != "claude-session" {
		t.Fatalf("live session = %#v", got)
	}
	if !got.GetSupportsSend() || !got.GetSupportsStream() || got.GetSupportsStop() {
		t.Fatalf("unexpected live capabilities: send=%v stream=%v stop=%v", got.GetSupportsSend(), got.GetSupportsStream(), got.GetSupportsStop())
	}
}

func TestCodexLiveStreamFansOutOneRuntimeStream(t *testing.T) {
	srv := newTestServer(t)
	runtime := &fakeLiveRuntime{
		sendTurn: &codex.LiveTurn{
			ThreadID: "codex-thread",
			TurnID:   "turn-1",
			Status:   codex.LiveTurnStatusInProgress,
		},
		streamEvents: []codex.LiveEvent{
			{Kind: codex.LiveEventDelta, ThreadID: "codex-thread", TurnID: "turn-1", Delta: "hello"},
			{Kind: codex.LiveEventCompleted, ThreadID: "codex-thread", TurnID: "turn-1", Status: codex.LiveTurnStatusCompleted},
		},
	}
	srv.liveSessions["codex-thread"] = &liveRuntimeSession{
		provider:     session.ProviderCodex,
		name:         "codex-chat",
		id:           "codex-thread",
		codexRuntime: runtime,
	}

	if _, err := srv.SendLiveSession(context.Background(), &clydev1.SendLiveSessionRequest{
		SessionId: "codex-thread",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("send live session: %v", err)
	}

	first, err := srv.streamLiveSessionEvents(context.Background(), "codex-thread")
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	second, err := srv.streamLiveSessionEvents(context.Background(), "codex-thread")
	if err != nil {
		t.Fatalf("second stream: %v", err)
	}
	gotFirst := collectLiveEvents(t, first, 2)
	gotSecond := collectLiveEvents(t, second, 2)
	if runtime.streamCalls != 1 {
		t.Fatalf("stream calls = %d want 1", runtime.streamCalls)
	}
	if gotFirst[0].Text != "hello" || gotSecond[0].Text != "hello" {
		t.Fatalf("fanout delta mismatch: first=%#v second=%#v", gotFirst, gotSecond)
	}
}

func TestStartCodexLiveSessionStoresTypedEffort(t *testing.T) {
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
	resp, err := srv.StartLiveSession(context.Background(), &clydev1.StartLiveSessionRequest{
		Provider: string(session.ProviderCodex),
		Basedir:  tmp,
		Effort:   "xhigh",
	})
	if err != nil {
		t.Fatalf("start live session: %v", err)
	}
	record := srv.liveSessions[resp.GetSession().GetSessionId()]
	if record == nil {
		t.Fatalf("live session record missing")
	}
	state := liveRuntimeState(record.id)
	if state.effort != codex.ReasoningEffort("xhigh") {
		t.Fatalf("effort = %q want xhigh", state.effort)
	}
}

func TestStartCodexLiveSessionRejectsNonLiveEffort(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	srv := newTestServer(t)
	_, err := srv.StartLiveSession(context.Background(), &clydev1.StartLiveSessionRequest{
		Provider: string(session.ProviderCodex),
		Basedir:  tmp,
		Effort:   "minimal",
	})
	if err == nil {
		t.Fatalf("start live session returned nil error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error code = %v want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestStartClaudeLiveSessionRejectsCodexOnlyEffort(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	srv := newTestServer(t)
	_, err := srv.StartLiveSession(context.Background(), &clydev1.StartLiveSessionRequest{
		Provider: string(session.ProviderClaude),
		Basedir:  tmp,
		Effort:   "xhigh",
	})
	if err == nil {
		t.Fatalf("start live session returned nil error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error code = %v want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestListLiveSessionsReacquiresClaudeSocketWithoutWorkerMap(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	shortRun := shortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", shortRun)

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	basedir := filepath.Join(tmp, "work")
	sess := session.NewSession("claude-chat", "claude-session")
	setTestProviderIdentity(sess, session.ProviderClaude, "claude-session")
	sess.Metadata.WorkDir = basedir
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	socketPath := injectSocketPath("claude-session")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatalf("mkdir inject dir: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen inject socket: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()

	srv := newTestServer(t)
	resp, err := srv.ListLiveSessions(context.Background(), &clydev1.ListLiveSessionsRequest{})
	if err != nil {
		t.Fatalf("list live sessions: %v", err)
	}
	if len(resp.GetSessions()) != 1 {
		t.Fatalf("live session count = %d want 1", len(resp.GetSessions()))
	}
	got := resp.GetSessions()[0]
	if got.GetProvider() != string(session.ProviderClaude) || got.GetSessionId() != "claude-session" {
		t.Fatalf("live session = %#v", got)
	}
	if got.GetStatus() != "reattachable" {
		t.Fatalf("status = %q want reattachable", got.GetStatus())
	}
	if got.GetBasedir() != basedir {
		t.Fatalf("basedir = %q want %q", got.GetBasedir(), basedir)
	}
}

func TestForegroundLeaseSuspendsClaudeRemoteByInjectSocketAfterReload(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	shortRun := shortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", shortRun)

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	basedir := filepath.Join(tmp, "work")
	sess := session.NewSession("claude-chat", "claude-session")
	setTestProviderIdentity(sess, session.ProviderClaude, "claude-session")
	sess.Metadata.WorkDir = basedir
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	socketPath := injectSocketPath("claude-session")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatalf("mkdir inject dir: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen inject socket: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		payload, _ := io.ReadAll(conn)
		received <- payload
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()

	srv := newTestServer(t)
	acquired, err := srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "claude-chat",
		Provider:    string(session.ProviderClaude),
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !acquired.GetShouldRestore() {
		t.Fatalf("should_restore = false, want true")
	}
	if acquired.GetRestoreReason() != "claude_remote_socket" {
		t.Fatalf("restore_reason = %q want claude_remote_socket", acquired.GetRestoreReason())
	}
	select {
	case got := <-received:
		if len(got) != 1 || got[0] != 0x03 {
			t.Fatalf("inject payload = %#v want ctrl-c", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for ctrl-c payload")
	}
}

func TestForegroundLeaseNoopsWhenNoLiveRuntimeExists(t *testing.T) {
	setupDaemonTestHome(t)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("codex-chat", "codex-thread")
	setTestProviderIdentity(sess, session.ProviderCodex, "codex-thread")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	srv := newTestServer(t)

	acquired, err := srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "codex-chat",
		Provider:    string(session.ProviderCodex),
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if acquired.GetShouldRestore() {
		t.Fatalf("should_restore = true, want false")
	}
	released, err := srv.ReleaseForegroundSession(context.Background(), &clydev1.ReleaseForegroundSessionRequest{LeaseToken: acquired.GetLeaseToken()})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.GetRestored() {
		t.Fatalf("restored = true, want false")
	}
}

func TestForegroundLeaseSuspendsAndRestoresClaudeRemoteWorker(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	basedir := filepath.Join(tmp, "work")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatalf("mkdir basedir: %v", err)
	}
	sess := session.NewSession("claude-chat", "claude-session")
	setTestProviderIdentity(sess, session.ProviderClaude, "claude-session")
	sess.Metadata.WorkDir = basedir
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	srv := newTestServer(t)
	srv.remoteMu.Lock()
	srv.remoteWorkers["claude-session"] = &remoteWorker{
		sessionName: "claude-chat",
		sessionID:   "claude-session",
		incognito:   true,
	}
	srv.remoteMu.Unlock()
	script := filepath.Join(tmp, "fake-worker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake worker: %v", err)
	}
	oldExec := remoteWorkerExecutable
	remoteWorkerExecutable = func() (string, error) { return script, nil }
	defer func() { remoteWorkerExecutable = oldExec }()

	acquired, err := srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "claude-chat",
		Provider:    string(session.ProviderClaude),
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !acquired.GetShouldRestore() {
		t.Fatalf("should_restore = false, want true")
	}
	srv.remoteMu.Lock()
	_, stillPresent := srv.remoteWorkers["claude-session"]
	srv.remoteMu.Unlock()
	if stillPresent {
		t.Fatalf("remote worker still present after acquire")
	}
	if err := store.Rename("claude-chat", "claude-renamed"); err != nil {
		t.Fatalf("rename session before restore: %v", err)
	}

	released, err := srv.ReleaseForegroundSession(context.Background(), &clydev1.ReleaseForegroundSessionRequest{
		LeaseToken: acquired.GetLeaseToken(),
		ExitState:  "ok",
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !released.GetRestored() {
		t.Fatalf("restored = false, want true")
	}
	srv.remoteMu.Lock()
	restored := srv.remoteWorkers["claude-session"]
	srv.remoteMu.Unlock()
	if restored == nil || restored.sessionID != "claude-session" {
		t.Fatalf("remote worker not restored: %#v", restored)
	}
	if restored.sessionName != "claude-renamed" {
		t.Fatalf("restored session name = %q, want claude-renamed", restored.sessionName)
	}
	if released.GetLiveSession().GetSessionName() != "claude-renamed" {
		t.Fatalf("released live session name = %q, want claude-renamed", released.GetLiveSession().GetSessionName())
	}
	if !restored.incognito {
		t.Fatalf("restored incognito = false, want true")
	}
	if restored.cmd != nil && restored.cmd.Process != nil {
		_ = restored.cmd.Process.Kill()
	}
	if restored.done != nil {
		<-restored.done
	}
}

func TestForegroundLeaseStopsActiveCodexTurnBeforeClose(t *testing.T) {
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
	srv := newTestServer(t)
	startRuntime := &fakeLiveRuntime{}
	restoreRuntime := &fakeLiveRuntime{}
	oldFactory := newCodexLiveRuntime
	newCodexLiveRuntime = func(codex.LiveRuntimeOptions) codex.LiveRuntime {
		return restoreRuntime
	}
	defer func() { newCodexLiveRuntime = oldFactory }()
	t.Cleanup(func() { deleteLiveRuntimeState("codex-thread") })

	srv.liveSessions["codex-thread"] = &liveRuntimeSession{
		provider:     session.ProviderCodex,
		name:         "codex-chat",
		id:           "codex-thread",
		basedir:      sess.Metadata.WorkDir,
		model:        "gpt-test",
		status:       "in_progress",
		startedAt:    daemonNow(),
		lastTurnID:   "turn-active",
		codexRuntime: startRuntime,
	}

	acquired, err := srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "codex-chat",
		Provider:    string(session.ProviderCodex),
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !acquired.GetShouldRestore() {
		t.Fatalf("should_restore = false, want true")
	}
	if !startRuntime.closed {
		t.Fatalf("start runtime was not closed after acquire")
	}
	if _, ok := srv.liveSessions["codex-thread"]; ok {
		t.Fatalf("codex live session still present after acquire")
	}
	if got := len(startRuntime.calls); got != 2 {
		t.Fatalf("call count = %d, want 2 (stop, close); calls=%v", got, startRuntime.calls)
	}
	if startRuntime.calls[0].Kind != fakeLiveRuntimeCallStop {
		t.Fatalf("first call = %q, want %q", startRuntime.calls[0].Kind, fakeLiveRuntimeCallStop)
	}
	if startRuntime.calls[0].TurnID != "turn-active" {
		t.Fatalf("stop turn_id = %q, want turn-active", startRuntime.calls[0].TurnID)
	}
	if startRuntime.calls[1].Kind != fakeLiveRuntimeCallClose {
		t.Fatalf("second call = %q, want %q", startRuntime.calls[1].Kind, fakeLiveRuntimeCallClose)
	}
}

func TestForegroundLeaseInterruptsAndKillsClaudeRemoteWorkerOnTimeout(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	basedir := filepath.Join(tmp, "work")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatalf("mkdir basedir: %v", err)
	}
	sess := session.NewSession("claude-chat", "claude-session")
	setTestProviderIdentity(sess, session.ProviderClaude, "claude-session")
	sess.Metadata.WorkDir = basedir
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Worker installs SIG_IGN for SIGINT, signals readiness through a
	// marker file, and then sleeps. The daemon's Interrupt path therefore
	// has no effect, forcing the timeout-and-Kill branch which delivers
	// SIGKILL through the real OS signal pipeline.
	readyMarker := filepath.Join(tmp, "trap-worker-ready")
	script := filepath.Join(tmp, "interrupt-trap-worker.sh")
	scriptBody := fmt.Sprintf("#!/usr/bin/env python3\n"+
		"import signal, time, pathlib\n"+
		"signal.signal(signal.SIGINT, signal.SIG_IGN)\n"+
		"pathlib.Path(%q).write_text('ready')\n"+
		"while True:\n"+
		"    time.sleep(30)\n", readyMarker)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write trap worker: %v", err)
	}

	cmd := exec.Command(script)
	cmd.Dir = basedir
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start trap worker: %v", err)
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

	// Wait for SIG_IGN to be installed before sending the interrupt;
	// otherwise SIGINT arrives during Python startup with the default
	// terminate disposition still active.
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(readyMarker); statErr == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatalf("trap worker did not install SIG_IGN before deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}

	oldTimeout := claudeRemoteSuspendTimeout
	claudeRemoteSuspendTimeout = 100 * time.Millisecond
	t.Cleanup(func() { claudeRemoteSuspendTimeout = oldTimeout })

	// Block restoreClaudeRemoteAfterForeground from forking a real
	// worker after suspend. Restore is not under test here.
	oldExec := remoteWorkerExecutable
	remoteWorkerExecutable = func() (string, error) { return "", fmt.Errorf("restore disabled in test") }
	t.Cleanup(func() { remoteWorkerExecutable = oldExec })

	srv := newTestServer(t)
	worker := &remoteWorker{
		sessionName: "claude-chat",
		sessionID:   "claude-session",
		basedir:     basedir,
		incognito:   false,
		cmd:         cmd,
		done:        done,
	}
	srv.remoteMu.Lock()
	srv.remoteWorkers["claude-chat"] = worker
	srv.remoteMu.Unlock()

	startedAt := time.Now()
	acquired, err := srv.AcquireForegroundSession(context.Background(), &clydev1.AcquireForegroundSessionRequest{
		SessionName: "claude-chat",
		Provider:    string(session.ProviderClaude),
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	elapsed := time.Since(startedAt)
	if !acquired.GetShouldRestore() {
		t.Fatalf("should_restore = false, want true")
	}
	if acquired.GetRestoreReason() != "claude_remote_worker" {
		t.Fatalf("restore_reason = %q want claude_remote_worker", acquired.GetRestoreReason())
	}
	// Acquire must have blocked at least one full timeout interval
	// because the trapped SIGINT is ignored, forcing the daemon to wait
	// until claudeRemoteSuspendTimeout before falling through to Kill.
	if elapsed < claudeRemoteSuspendTimeout {
		t.Fatalf("acquire elapsed = %v, want >= %v (timeout path required)", elapsed, claudeRemoteSuspendTimeout)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("acquire elapsed = %v, want < 3s (timeout+drain bound)", elapsed)
	}

	srv.remoteMu.Lock()
	_, stillPresent := srv.remoteWorkers["claude-chat"]
	srv.remoteMu.Unlock()
	if stillPresent {
		t.Fatalf("remote worker still registered after acquire")
	}

	// Worker must be reaped because Process.Kill delivered SIGKILL.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("worker process did not exit after kill")
	}
	state := cmd.ProcessState
	if state == nil {
		t.Fatalf("process state nil after wait")
	}
	if state.Success() {
		t.Fatalf("worker exited successfully, want signal-induced termination")
	}
	waitStatus, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("wait status type = %T, want syscall.WaitStatus", state.Sys())
	}
	if !waitStatus.Signaled() {
		t.Fatalf("worker exit status = %v, want signaled", waitStatus)
	}
	if waitStatus.Signal() != syscall.SIGKILL {
		t.Fatalf("worker termination signal = %v, want SIGKILL", waitStatus.Signal())
	}
}

func TestReleaseForegroundSessionIsIdempotent(t *testing.T) {
	setupDaemonTestHome(t)
	srv := newTestServer(t)
	resp, err := srv.ReleaseForegroundSession(context.Background(), &clydev1.ReleaseForegroundSessionRequest{
		LeaseToken: "missing",
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if resp.GetRestored() {
		t.Fatalf("restored = true, want false")
	}
}

func setupDaemonTestHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))
	return tmp
}

func shortRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("/tmp", fmt.Sprintf("clyde-%d-%d", os.Getpid(), time.Now().UnixNano()))
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir short runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func setTestProviderIdentity(sess *session.Session, provider session.ProviderID, id string) {
	sess.Metadata.Provider = provider
	sess.Metadata.SessionID = id
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: provider, ID: id},
	}
	sess.Metadata.NormalizeProviderState()
}

// fakeLiveRuntimeCallKind enumerates fakeLiveRuntime method invocations for
// the typed call log used by ordering assertions in tests.
type fakeLiveRuntimeCallKind string

const (
	fakeLiveRuntimeCallStart  fakeLiveRuntimeCallKind = "start"
	fakeLiveRuntimeCallAttach fakeLiveRuntimeCallKind = "attach"
	fakeLiveRuntimeCallSend   fakeLiveRuntimeCallKind = "send"
	fakeLiveRuntimeCallStream fakeLiveRuntimeCallKind = "stream"
	fakeLiveRuntimeCallStop   fakeLiveRuntimeCallKind = "stop"
	fakeLiveRuntimeCallClose  fakeLiveRuntimeCallKind = "close"
)

// fakeLiveRuntimeCall records one invocation against fakeLiveRuntime. Stop
// captures TurnID so ordering tests can assert which turn was interrupted
// before Close ran.
type fakeLiveRuntimeCall struct {
	Kind   fakeLiveRuntimeCallKind
	TurnID string
}

type fakeLiveRuntime struct {
	closed         bool
	attachedThread string
	attachSession  *codex.LiveSession
	startSession   *codex.LiveSession
	sendRequests   []codex.LiveSendRequest
	sendTurn       *codex.LiveTurn
	streamEvents   []codex.LiveEvent
	streamCalls    int
	calls          []fakeLiveRuntimeCall
}

func (f *fakeLiveRuntime) Start(context.Context, codex.LiveStartRequest) (*codex.LiveSession, error) {
	f.calls = append(f.calls, fakeLiveRuntimeCall{Kind: fakeLiveRuntimeCallStart})
	if f.startSession != nil {
		return f.startSession, nil
	}
	return &codex.LiveSession{ThreadID: "codex-thread"}, nil
}

func (f *fakeLiveRuntime) Attach(_ context.Context, req codex.LiveAttachRequest) (*codex.LiveSession, error) {
	f.attachedThread = req.ThreadID
	f.calls = append(f.calls, fakeLiveRuntimeCall{Kind: fakeLiveRuntimeCallAttach})
	if f.attachSession != nil {
		return f.attachSession, nil
	}
	return &codex.LiveSession{ThreadID: req.ThreadID}, nil
}

func (f *fakeLiveRuntime) Send(_ context.Context, req codex.LiveSendRequest) (*codex.LiveTurn, error) {
	f.sendRequests = append(f.sendRequests, req)
	f.calls = append(f.calls, fakeLiveRuntimeCall{Kind: fakeLiveRuntimeCallSend})
	if f.sendTurn != nil {
		return f.sendTurn, nil
	}
	return &codex.LiveTurn{ThreadID: req.ThreadID, TurnID: "turn-1", Status: codex.LiveTurnStatusInProgress}, nil
}

func (f *fakeLiveRuntime) Stream(context.Context, codex.LiveStreamRequest) (<-chan codex.LiveEvent, error) {
	f.streamCalls++
	f.calls = append(f.calls, fakeLiveRuntimeCall{Kind: fakeLiveRuntimeCallStream})
	out := make(chan codex.LiveEvent, len(f.streamEvents))
	for _, event := range f.streamEvents {
		out <- event
	}
	close(out)
	return out, nil
}

func (f *fakeLiveRuntime) Stop(_ context.Context, req codex.LiveStopRequest) error {
	f.calls = append(f.calls, fakeLiveRuntimeCall{Kind: fakeLiveRuntimeCallStop, TurnID: req.TurnID})
	return nil
}

func (f *fakeLiveRuntime) Close() error {
	f.closed = true
	f.calls = append(f.calls, fakeLiveRuntimeCall{Kind: fakeLiveRuntimeCallClose})
	return nil
}

func collectLiveEvents(t *testing.T, ch <-chan webapp.LiveSessionEvent, count int) []webapp.LiveSessionEvent {
	t.Helper()
	out := make([]webapp.LiveSessionEvent, 0, count)
	for len(out) < count {
		select {
		case event, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed after %d events, want %d", len(out), count)
			}
			out = append(out, event)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for live event %d", len(out)+1)
		}
	}
	return out
}
