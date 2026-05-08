package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/session"
)

func TestStartRemoteSessionCreatesCanonicalSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	script := filepath.Join(tmp, "fake-worker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatalf("write fake worker: %v", err)
	}
	oldExec := remoteWorkerExecutable
	remoteWorkerExecutable = func() (string, error) { return script, nil }
	defer func() { remoteWorkerExecutable = oldExec }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(log)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	basedir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatalf("mkdir basedir: %v", err)
	}

	resp, err := srv.StartRemoteSession(context.Background(), &clydev1.StartRemoteSessionRequest{
		Basedir: basedir,
	})
	if err != nil {
		t.Fatalf("start remote session: %v", err)
	}
	if resp.GetSessionName() == "" {
		t.Fatalf("session name not set")
	}
	if resp.GetSessionId() == "" {
		t.Fatalf("session id not set")
	}
	if resp.GetLaunchState() != clydev1.StartRemoteSessionResponse_LAUNCH_STATE_LAUNCHING {
		t.Fatalf("launch state = %v", resp.GetLaunchState())
	}

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := store.Get(resp.GetSessionName())
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Metadata.ProviderSessionID() != resp.GetSessionId() {
		t.Fatalf("session id = %q want %q", sess.Metadata.ProviderSessionID(), resp.GetSessionId())
	}
	if sess.Metadata.WorkDir != basedir {
		t.Fatalf("workdir = %q want %q", sess.Metadata.WorkDir, basedir)
	}
	settings, err := store.LoadSettings(resp.GetSessionName())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if settings == nil || !settings.RemoteControl {
		t.Fatalf("remote control settings not persisted")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.remoteMu.Lock()
		_, ok := srv.remoteWorkers[resp.GetSessionId()]
		srv.remoteMu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("remote worker was not tracked")
}

func TestStartRemoteSessionRejectsExistingName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.Create(session.NewSession("chat-fixed", "uuid-1")); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(log)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	basedir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatalf("mkdir basedir: %v", err)
	}

	if _, err := srv.StartRemoteSession(context.Background(), &clydev1.StartRemoteSessionRequest{
		SessionName: "chat-fixed",
		Basedir:     basedir,
	}); err == nil {
		t.Fatalf("expected already exists error")
	}
}

func TestStartRemoteSessionHonorsIncognito(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	script := filepath.Join(tmp, "fake-worker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatalf("write fake worker: %v", err)
	}
	oldExec := remoteWorkerExecutable
	remoteWorkerExecutable = func() (string, error) { return script, nil }
	defer func() { remoteWorkerExecutable = oldExec }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(log)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	basedir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatalf("mkdir basedir: %v", err)
	}

	resp, err := srv.StartRemoteSession(context.Background(), &clydev1.StartRemoteSessionRequest{
		Basedir:   basedir,
		Incognito: true,
	})
	if err != nil {
		t.Fatalf("start remote session: %v", err)
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := store.Get(resp.GetSessionName())
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !sess.Metadata.IsIncognito {
		t.Fatalf("incognito flag not persisted")
	}
}

func TestStartRemoteSessionLaunchesWorkerWithSessionAndBasedir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	argsFile := filepath.Join(tmp, "worker-args.txt")
	pwdFile := filepath.Join(tmp, "worker-pwd.txt")
	script := filepath.Join(tmp, "fake-worker.sh")
	scriptBody := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\npwd > " + pwdFile + "\nsleep 1\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake worker: %v", err)
	}
	oldExec := remoteWorkerExecutable
	remoteWorkerExecutable = func() (string, error) { return script, nil }
	defer func() { remoteWorkerExecutable = oldExec }()

	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	basedir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatalf("mkdir basedir: %v", err)
	}
	resp, err := srv.StartRemoteSession(context.Background(), &clydev1.StartRemoteSessionRequest{
		SessionName: "Merry Swan",
		Basedir:     basedir,
		Incognito:   true,
	})
	if err != nil {
		t.Fatalf("start remote session: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, argsErr := os.Stat(argsFile)
		_, pwdErr := os.Stat(pwdFile)
		if argsErr == nil && pwdErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read worker args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	wantArgs := []string{
		"daemon",
		"launch-remote-worker",
		"--session-name",
		"Merry Swan",
		"--session-id",
		resp.GetSessionId(),
		"--basedir",
		basedir,
		"--incognito",
	}
	if strings.Join(args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("worker args = %#v want %#v", args, wantArgs)
	}
	pwdBytes, err := os.ReadFile(pwdFile)
	if err != nil {
		t.Fatalf("read worker pwd: %v", err)
	}
	if got := strings.TrimSpace(string(pwdBytes)); got != basedir {
		t.Fatalf("worker pwd = %q want %q", got, basedir)
	}
}

func TestStartRemoteSessionRejectsInvalidDisplayName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	basedir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatalf("mkdir basedir: %v", err)
	}
	_, err = srv.StartRemoteSession(context.Background(), &clydev1.StartRemoteSessionRequest{
		SessionName: " Merry Swan",
		Basedir:     basedir,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error code = %v want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestDetectRemoteWorkerTmuxStateIsExplicit(t *testing.T) {
	t.Setenv("TMUX", "")
	if got := detectRemoteWorkerTmuxState(); got.Detected || got.Status != "not_detected" {
		t.Fatalf("tmux state without env = %#v", got)
	}
	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")
	got := detectRemoteWorkerTmuxState()
	if !got.Detected {
		t.Fatalf("tmux detected = false, want true")
	}
	if got.Status != "unsupported" {
		t.Fatalf("tmux status = %q want unsupported", got.Status)
	}
	if got.Reason == "" {
		t.Fatalf("tmux unsupported reason is empty")
	}
}
