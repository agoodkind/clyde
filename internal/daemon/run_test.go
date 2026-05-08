package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsAdapterConfigEvent(t *testing.T) {
	cfgDir := t.TempDir()
	tomlPath := filepath.Join(cfgDir, "config.toml")

	if !isAdapterConfigEvent(fsnotify.Event{Name: tomlPath, Op: fsnotify.Write}, tomlPath) {
		t.Fatalf("toml write should trigger reload")
	}
	if isAdapterConfigEvent(fsnotify.Event{Name: filepath.Join(cfgDir, "config.json"), Op: fsnotify.Create}, tomlPath) {
		t.Fatalf("json create should not trigger reload")
	}
	if isAdapterConfigEvent(fsnotify.Event{Name: filepath.Join(cfgDir, "notes.txt"), Op: fsnotify.Write}, tomlPath) {
		t.Fatalf("unrelated file should not trigger reload")
	}
	if isAdapterConfigEvent(fsnotify.Event{Name: tomlPath, Op: fsnotify.Op(0)}, tomlPath) {
		t.Fatalf("non-mutating event should not trigger reload")
	}
}

func TestAdapterControllerApplyNoopDoesNotStopProcess(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stopped := false
	proc := &adapterProcess{
		cancel: func() { stopped = true },
		done:   make(chan struct{}),
	}
	close(proc.done)

	ctrl := &adapterController{
		log: log,
		current: adapterLaunchConfig{
			Enabled: false,
		},
		proc: proc,
	}

	err := ctrl.apply(context.Background(), adapterLaunchConfig{Enabled: false}, false, nil)
	if err != nil {
		t.Fatalf("apply returned error: %v", err)
	}
	if stopped {
		t.Fatalf("no-op apply should not stop existing process")
	}
}

func TestAdapterControllerApplyBodyLoggingChangeDoesNotStopProcess(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stopped := false
	proc := &adapterProcess{
		cancel: func() { stopped = true },
		done:   make(chan struct{}),
	}
	close(proc.done)
	runtimeLogging := adapter.NewRuntimeLogging(config.LoggingConfig{
		Body: config.LoggingBody{Mode: "summary", MaxKB: 32},
	})

	ctrl := &adapterController{
		log:            log,
		runtimeLogging: runtimeLogging,
		current: adapterLaunchConfig{
			Enabled: true,
			Adapter: config.AdapterConfig{Port: 11434},
			Logging: config.LoggingConfig{Body: config.LoggingBody{Mode: "summary", MaxKB: 32}},
		},
		proc: proc,
	}

	err := ctrl.apply(context.Background(), adapterLaunchConfig{
		Enabled: true,
		Adapter: config.AdapterConfig{Port: 11434},
		Logging: config.LoggingConfig{Body: config.LoggingBody{Mode: "raw", MaxKB: 256}},
	}, false, nil)
	if err != nil {
		t.Fatalf("apply returned error: %v", err)
	}
	if stopped {
		t.Fatalf("body-only apply should not stop existing process")
	}
	body := runtimeLogging.Body()
	if body.Mode != "raw" || body.MaxKB != 256 {
		t.Fatalf("runtime body logging = %+v, want raw/256", body)
	}
}

func TestAdapterControllerApplyDisableStopsProcess(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stopped := false
	proc := &adapterProcess{
		cancel: func() { stopped = true },
		done:   make(chan struct{}),
	}
	close(proc.done)

	ctrl := &adapterController{
		log: log,
		current: adapterLaunchConfig{
			Enabled: true,
			Adapter: config.AdapterConfig{Port: 11434},
		},
		proc: proc,
	}

	err := ctrl.apply(context.Background(), adapterLaunchConfig{Enabled: false}, false, nil)
	if err != nil {
		t.Fatalf("apply returned error: %v", err)
	}
	if !stopped {
		t.Fatalf("disable apply should stop existing process")
	}
	if ctrl.proc != nil {
		t.Fatalf("process should be cleared after disable")
	}
	if ctrl.current.Enabled {
		t.Fatalf("current config should be disabled")
	}
}

func TestStopAdapterProcessWaitsForDone(t *testing.T) {
	done := make(chan struct{})
	canceled := false
	proc := &adapterProcess{
		cancel: func() {
			canceled = true
			close(done)
		},
		done: done,
	}
	stopAdapterProcess(proc, 100*time.Millisecond)
	if !canceled {
		t.Fatalf("expected cancel to be called")
	}
}

func TestAdapterReloadDrainSkipsShutdownWhenNoActiveRequests(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	drained := false
	forceClosed := false
	listenerClosed := false
	proc := &adapterProcess{
		drain: func(context.Context) error {
			drained = true
			return nil
		},
		waitIdle: func(context.Context) int {
			return 0
		},
		forceClose: func() error {
			forceClosed = true
			return nil
		},
		closeListener: func() error {
			listenerClosed = true
			return nil
		},
	}
	ctrl := &adapterController{log: log, proc: proc}

	ctrl.drainReloadedProcess(time.Second)

	if drained {
		t.Fatalf("idle reload drain should not wait on http.Server.Shutdown")
	}
	if !forceClosed {
		t.Fatalf("idle reload drain should force close idle keepalive connections")
	}
	if !listenerClosed {
		t.Fatalf("reload drain should close old listener")
	}
}

func TestAdapterReloadDrainUsesShutdownForActiveRequests(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	drained := false
	forceClosed := false
	proc := &adapterProcess{
		drain: func(context.Context) error {
			drained = true
			return nil
		},
		waitIdle: func(context.Context) int {
			return 1
		},
		forceClose: func() error {
			forceClosed = true
			return nil
		},
		closeListener: func() error { return nil },
	}
	ctrl := &adapterController{log: log, proc: proc}

	ctrl.drainReloadedProcess(time.Second)

	if !drained {
		t.Fatalf("active reload drain should wait on http.Server.Shutdown")
	}
	if !forceClosed {
		t.Fatalf("active reload drain should force close after bounded drain")
	}
}

func TestAdapterReloadDrainBoundsWaitIdle(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const timeout = 20 * time.Millisecond
	drained := false
	forceClosed := false
	proc := &adapterProcess{
		drain: func(context.Context) error {
			drained = true
			return nil
		},
		waitIdle: func(ctx context.Context) int {
			<-ctx.Done()
			return 1
		},
		forceClose: func() error {
			forceClosed = true
			return nil
		},
		closeListener: func() error { return nil },
	}
	ctrl := &adapterController{log: log, proc: proc}
	started := time.Now()

	ctrl.drainReloadedProcess(timeout)

	elapsed := time.Since(started)
	if elapsed < timeout {
		t.Fatalf("reload drain elapsed=%s, want at least %s", elapsed, timeout)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("reload drain elapsed=%s, want bounded wait", elapsed)
	}
	if !drained {
		t.Fatalf("active reload drain should call shutdown after idle wait times out")
	}
	if !forceClosed {
		t.Fatalf("active reload drain should force close after bounded drain")
	}
}

func TestReloadHTTPDrainWaitFitsReloadCommandDeadline(t *testing.T) {
	const reloadCommandDeadline = 45 * time.Second
	if reloadHTTPDrainWait >= reloadCommandDeadline {
		t.Fatalf("reloadHTTPDrainWait=%s must be less than reload command deadline %s", reloadHTTPDrainWait, reloadCommandDeadline)
	}
}

func TestReloadDaemonCallsReloadFunc(t *testing.T) {
	called := false
	srv := &Server{
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions:       map[string]*wrapperSession{"wrapper-reload-1": {wrapperID: "wrapper-reload-1", sessionName: "chat-1"}},
		globalSettings: map[string]json.RawMessage{},
	}
	srv.SetReloadFunc(func(_ context.Context) (reloadReport, error) {
		called = true
		return reloadReport{BinaryReloaded: true, NewPID: 1234}, nil
	})

	resp, err := srv.ReloadDaemon(context.Background(), &clydev1.ReloadDaemonRequest{})
	if err != nil {
		t.Fatalf("reload daemon: %v", err)
	}
	if !called {
		t.Fatalf("reload func was not called")
	}
	if resp.GetActiveSessions() != 1 {
		t.Fatalf("active sessions=%d want 1", resp.GetActiveSessions())
	}
	if !resp.GetBinaryReloaded() {
		t.Fatalf("binary reload flag was not propagated")
	}
	if resp.GetNewPid() != 1234 {
		t.Fatalf("new pid=%d want 1234", resp.GetNewPid())
	}
}

func TestReloadDaemonRequiresProcessLock(t *testing.T) {
	var lockHeld atomic.Bool
	called := false
	srv := &Server{
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		globalSettings: map[string]json.RawMessage{},
	}
	setReloadFuncWhenProcessOwner(srv, &lockHeld, func(_ context.Context) (reloadReport, error) {
		called = true
		return reloadReport{BinaryReloaded: true, NewPID: 4321}, nil
	})

	_, err := srv.ReloadDaemon(context.Background(), &clydev1.ReloadDaemonRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reload before lock code=%v err=%v, want FailedPrecondition", status.Code(err), err)
	}
	if called {
		t.Fatalf("reload func should not be called before process lock is held")
	}

	lockHeld.Store(true)
	resp, err := srv.ReloadDaemon(context.Background(), &clydev1.ReloadDaemonRequest{})
	if err != nil {
		t.Fatalf("reload after lock: %v", err)
	}
	if !called {
		t.Fatalf("reload func should be called after process lock is held")
	}
	if resp.GetNewPid() != 4321 {
		t.Fatalf("new pid=%d want 4321", resp.GetNewPid())
	}
}

func TestReloadDaemonRejectsBadReplacementBinaryBeforeStart(t *testing.T) {
	badBinary := filepath.Join(t.TempDir(), "clyde")
	script := "#!/usr/bin/env bash\nprintf 'not-clyde\\n'\n"
	if err := os.WriteFile(badBinary, []byte(script), 0o755); err != nil {
		t.Fatalf("write bad binary: %v", err)
	}

	oldExecutablePath := daemonExecutablePath
	oldReplacementCommand := daemonReplacementCommand
	t.Cleanup(func() {
		daemonExecutablePath = oldExecutablePath
		daemonReplacementCommand = oldReplacementCommand
	})

	startCalled := false
	daemonExecutablePath = func() (string, error) {
		return badBinary, nil
	}
	daemonReplacementCommand = func(string, ...string) *exec.Cmd {
		startCalled = true
		return exec.Command("false")
	}

	_, err := reloadDaemonBinary(
		context.Background(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		&daemonRuntime{},
		&Server{},
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "validate replacement daemon binary") {
		t.Fatalf("reloadDaemonBinary error=%v, want validation failure", err)
	}
	if startCalled {
		t.Fatalf("replacement command was created for invalid binary")
	}
}

func TestInheritedListenerFilesIncludesDaemonAdapterAndWebapp(t *testing.T) {
	// Unix socket paths are short on macOS, and t.TempDir can exceed the limit.
	socketDir, err := os.MkdirTemp("/tmp", "clyde-daemon-*") //nolint:usetesting
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(socketDir)
	})
	daemonLis, err := net.Listen("unix", filepath.Join(socketDir, "d.sock"))
	if err != nil {
		t.Fatalf("listen daemon: %v", err)
	}
	defer daemonLis.Close()
	adapterLis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen adapter: %v", err)
	}
	defer adapterLis.Close()
	webLis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen webapp: %v", err)
	}
	defer webLis.Close()
	mitmLis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen mitm: %v", err)
	}
	defer mitmLis.Close()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	_, adapterPort, err := net.SplitHostPort(adapterLis.Addr().String())
	if err != nil {
		t.Fatalf("split adapter addr: %v", err)
	}
	_, webPort, err := net.SplitHostPort(webLis.Addr().String())
	if err != nil {
		t.Fatalf("split web addr: %v", err)
	}
	_, mitmPort, err := net.SplitHostPort(mitmLis.Addr().String())
	if err != nil {
		t.Fatalf("split mitm addr: %v", err)
	}
	toml := "[adapter]\n" +
		"enabled = true\n" +
		"host = \"[::1]\"\n" +
		"port = " + adapterPort + "\n" +
		"[web_app]\n" +
		"enabled = true\n" +
		"host = \"[::1]\"\n" +
		"port = " + webPort + "\n" +
		"[mitm.listen]\n" +
		"host = \"[::1]\"\n" +
		"port = " + mitmPort + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	rt := &daemonRuntime{
		listener: daemonLis,
		adapter:  &adapterController{proc: &adapterProcess{lis: adapterLis}},
		webapp:   &webAppProcess{lis: webLis},
		mitm:     &mitmProcess{lis: mitmLis},
	}
	files, specs, cleanup, err := inheritedListenerFiles(rt)
	if err != nil {
		t.Fatalf("inherited files: %v", err)
	}
	defer cleanup()
	if len(files) != 4 || len(specs) != 4 {
		t.Fatalf("got %d files and %d specs, want 4 each", len(files), len(specs))
	}
	gotNames := []string{specs[0].Name, specs[1].Name, specs[2].Name, specs[3].Name}
	if strings.Join(gotNames, ",") != "daemon,adapter,webapp,mitm" {
		t.Fatalf("listener names = %v", gotNames)
	}
	for i, spec := range specs {
		if spec.FD != 3+i {
			t.Fatalf("spec %s fd=%d want %d", spec.Name, spec.FD, 3+i)
		}
	}
}
