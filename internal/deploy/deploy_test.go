package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigFromEnvUsesDefaults(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	config, err := loadConfigFromEnv(platformDarwin, func(key string) (string, bool) {
		if key == "HOME" {
			return homeDir, true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	if config.InstallBin == "" {
		t.Fatal("InstallBin is empty")
	}
	if config.LaunchdLabel != defaultLaunchdLabel {
		t.Fatalf("LaunchdLabel = %q, want %q", config.LaunchdLabel, defaultLaunchdLabel)
	}
	wantPlist := filepath.Join(homeDir, "Library", "LaunchAgents", defaultLaunchdLabel+".plist")
	if config.LaunchdPlist != wantPlist {
		t.Fatalf("LaunchdPlist = %q, want %q", config.LaunchdPlist, wantPlist)
	}
}

func TestRunDarwinConfigMismatchInstallsService(t *testing.T) {
	t.Parallel()

	fixture := newDeployFixture(t, platformDarwin)
	fixture.fs.exists[fixture.config.LaunchdPlist] = true
	fixture.fs.files[fixture.config.LaunchdPlist] = []byte("mismatch")
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"bootout", fixture.config.LaunchdDomain, fixture.config.LaunchdPlist}}, "")
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"bootstrap", fixture.config.LaunchdDomain, fixture.config.LaunchdPlist}}, "")
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"print", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "loaded")

	if err := run(context.Background(), fixture.config, fixture.deps()); err != nil {
		t.Fatalf("Run() error = %v\nlogs:\n%s", err, fixture.logs.String())
	}

	fixture.assertLogContains(t,
		`deploy: event=branch.darwin.changed_service_config`,
		`deploy: event=action.install_service.selected reason="changed_service_config" cause="config_mismatch_or_missing"`,
		`deploy: event=main.done platform="Darwin" action="install_service" reason="changed_service_config" cause="config_mismatch_or_missing"`,
	)
	if !fixture.fs.wrotePath(fixture.config.LaunchdPlist) {
		t.Fatalf("launchd plist %q was not written", fixture.config.LaunchdPlist)
	}
}

func TestRunDarwinFingerprintFailureRestartsService(t *testing.T) {
	t.Parallel()

	fixture := newDeployFixture(t, platformDarwin)
	rendered := fixture.mustRenderTemplate(t, darwinTemplate, fixture.config.LaunchdLabel)
	fixture.fs.exists[fixture.config.LaunchdPlist] = true
	fixture.fs.files[fixture.config.LaunchdPlist] = rendered
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"print", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "loaded")
	fixture.compiledFingerprint = func() string { return "built-123" }
	fixture.runningFingerprint = func(context.Context) (string, error) { return "", errors.New("running failed") }
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"kickstart", "-k", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "")
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"print", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "running")

	if err := run(context.Background(), fixture.config, fixture.deps()); err != nil {
		t.Fatalf("Run() error = %v\nlogs:\n%s", err, fixture.logs.String())
	}

	fixture.assertLogContains(t,
		`deploy: event=branch.stale_root_parent cause="running_fingerprint_failed"`,
		`deploy: event=action.restart_service.selected reason="stale_root_parent" cause="running_fingerprint_failed"`,
		`deploy: event=main.done platform="Darwin" action="restart_service" reason="stale_root_parent" cause="running_fingerprint_failed"`,
	)
}

func TestRunDarwinMatchingFingerprintsReloadsDaemon(t *testing.T) {
	t.Parallel()

	fixture := newDeployFixture(t, platformDarwin)
	rendered := fixture.mustRenderTemplate(t, darwinTemplate, fixture.config.LaunchdLabel)
	fixture.fs.exists[fixture.config.LaunchdPlist] = true
	fixture.fs.files[fixture.config.LaunchdPlist] = rendered
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"print", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "loaded")
	fixture.compiledFingerprint = func() string { return "same-fingerprint" }
	fixture.runningFingerprint = func(context.Context) (string, error) { return "same-fingerprint", nil }
	fixture.runner.setSuccess(command{name: fixture.config.InstallBin, args: []string{"daemon", "reload"}}, "reload ok")
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"print", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "running")

	if err := run(context.Background(), fixture.config, fixture.deps()); err != nil {
		t.Fatalf("Run() error = %v\nlogs:\n%s", err, fixture.logs.String())
	}

	fixture.assertLogContains(t,
		`deploy: event=branch.worker_reload built_supervisor="same-fingerprint" running_supervisor="same-fingerprint"`,
		`deploy: event=action.reload_daemon.selected reason="worker_reload" built_supervisor="same-fingerprint" running_supervisor="same-fingerprint"`,
		`deploy: event=main.done platform="Darwin" action="reload_daemon" reason="worker_reload" cause=""`,
	)
}

func TestRunLinuxInactiveInstallsService(t *testing.T) {
	t.Parallel()

	fixture := newDeployFixture(t, platformLinux)
	rendered := fixture.mustRenderTemplate(t, linuxTemplate, strings.TrimSuffix(fixture.config.SystemdUnit, filepath.Ext(fixture.config.SystemdUnit)))
	fixture.fs.exists[fixture.config.SystemdUserUnit] = true
	fixture.fs.files[fixture.config.SystemdUserUnit] = rendered
	fixture.runner.setFailure(command{name: "systemctl", args: []string{"--user", "is-active", "--quiet", fixture.config.SystemdUnit}}, 3, "")
	fixture.runner.setSuccess(command{name: "systemctl", args: []string{"--user", "daemon-reload"}}, "")
	fixture.runner.setSuccess(command{name: "systemctl", args: []string{"--user", "enable", fixture.config.SystemdUnit}}, "")
	fixture.runner.setSuccess(command{name: "systemctl", args: []string{"--user", "restart", fixture.config.SystemdUnit}}, "")
	fixture.runner.setSuccess(command{name: "systemctl", args: []string{"--user", "status", fixture.config.SystemdUnit, "--no-pager"}}, "active")

	if err := run(context.Background(), fixture.config, fixture.deps()); err != nil {
		t.Fatalf("Run() error = %v\nlogs:\n%s", err, fixture.logs.String())
	}

	fixture.assertLogContains(t,
		`deploy: event=linux.service_active.false unit="clyde-daemon.service" status="3"`,
		`deploy: event=action.install_service.selected reason="inactive_service" cause="systemd_inactive"`,
		`deploy: event=main.done platform="Linux" action="install_service" reason="inactive_service" cause="systemd_inactive"`,
	)
	written := fixture.fs.written[fixture.config.SystemdUserUnit]
	wantExecStart := "ExecStart=" + fixture.config.InstallBin + " daemon run"
	if !strings.Contains(string(written), wantExecStart) {
		t.Fatalf("systemd unit missing %q:\n%s", wantExecStart, written)
	}
}

func TestRunReloadOnlyRefusesOnDarwinConfigMismatch(t *testing.T) {
	t.Parallel()

	fixture := newDeployFixture(t, platformDarwin)
	fixture.config.ReloadOnly = true
	fixture.fs.exists[fixture.config.LaunchdPlist] = true
	fixture.fs.files[fixture.config.LaunchdPlist] = []byte("mismatch")

	err := run(context.Background(), fixture.config, fixture.deps())
	if err == nil {
		t.Fatal("run() err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "make deploy") {
		t.Fatalf("err = %q, want make deploy hint", err.Error())
	}

	fixture.assertLogContains(t,
		`deploy: event=branch.darwin.changed_service_config`,
		`deploy: event=action.refuse.selected reason="deploy_required" cause="config_mismatch_or_missing"`,
		`deploy: event=main.failed platform="Darwin" action="refuse" reason="deploy_required" cause="config_mismatch_or_missing"`,
	)
}

func TestRunReloadOnlyReloadsWhenStateIsValid(t *testing.T) {
	t.Parallel()

	fixture := newDeployFixture(t, platformDarwin)
	fixture.config.ReloadOnly = true
	rendered := fixture.mustRenderTemplate(t, darwinTemplate, fixture.config.LaunchdLabel)
	fixture.fs.exists[fixture.config.LaunchdPlist] = true
	fixture.fs.files[fixture.config.LaunchdPlist] = rendered
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"print", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "loaded")
	fixture.compiledFingerprint = func() string { return "same-fingerprint" }
	fixture.runningFingerprint = func(context.Context) (string, error) { return "same-fingerprint", nil }
	fixture.runner.setSuccess(command{name: fixture.config.InstallBin, args: []string{"daemon", "reload"}}, "reload ok")
	fixture.runner.setSuccess(command{name: "launchctl", args: []string{"print", fixture.config.LaunchdDomain + "/" + fixture.config.LaunchdLabel}}, "running")

	if err := run(context.Background(), fixture.config, fixture.deps()); err != nil {
		t.Fatalf("Run() error = %v\nlogs:\n%s", err, fixture.logs.String())
	}

	fixture.assertLogContains(t,
		`deploy: event=main.mode reload_only="true"`,
		`deploy: event=branch.worker_reload built_supervisor="same-fingerprint" running_supervisor="same-fingerprint"`,
		`deploy: event=action.reload_daemon.selected reason="worker_reload" built_supervisor="same-fingerprint" running_supervisor="same-fingerprint"`,
		`deploy: event=main.done platform="Darwin" action="reload_daemon" reason="worker_reload" cause=""`,
	)
}

type deployFixture struct {
	t                   *testing.T
	config              config
	fs                  *fakeFileSystem
	runner              *fakeRunner
	logs                bytes.Buffer
	stdout              bytes.Buffer
	stderr              bytes.Buffer
	compiledFingerprint func() string
	runningFingerprint  func(ctx context.Context) (string, error)
}

func newDeployFixture(t *testing.T, targetPlatform platform) *deployFixture {
	t.Helper()

	homeDir := t.TempDir()
	installBin := filepath.Join(homeDir, "bin", "clyde")
	config := config{
		Platform:        targetPlatform,
		Home:            homeDir,
		InstallBin:      installBin,
		LogPath:         filepath.Join(homeDir, "logs", "clyde-daemon.log"),
		LaunchdLabel:    "",
		LaunchdPlist:    "",
		LaunchdDomain:   "",
		SystemdUnit:     "",
		SystemdUserUnit: "",
	}
	switch targetPlatform {
	case platformDarwin:
		config.LaunchdLabel = defaultLaunchdLabel
		config.LaunchdDomain = "gui/501"
		config.LaunchdPlist = filepath.Join(homeDir, "Library", "LaunchAgents", defaultLaunchdLabel+".plist")
	case platformLinux:
		config.SystemdUnit = defaultSystemdUnit
		config.SystemdUserUnit = filepath.Join(homeDir, ".config", "systemd", "user", defaultSystemdUnit)
	}
	return &deployFixture{
		t:                   t,
		config:              config,
		fs:                  newFakeFileSystem(),
		runner:              newFakeRunner(),
		compiledFingerprint: func() string { return "built-default" },
		runningFingerprint:  func(context.Context) (string, error) { return "built-default", nil },
	}
}

func (fixture *deployFixture) deps() dependencies {
	return dependencies{
		fileSystem:          fixture.fs,
		runner:              fixture.runner,
		logWriter:           &fixture.logs,
		stdout:              &fixture.stdout,
		stderr:              &fixture.stderr,
		compiledFingerprint: fixture.compiledFingerprint,
		runningFingerprint:  fixture.runningFingerprint,
	}
}

func (fixture *deployFixture) assertLogContains(t *testing.T, entries ...string) {
	t.Helper()
	body := fixture.logs.String()
	for _, entry := range entries {
		if !strings.Contains(body, entry) {
			t.Fatalf("logs missing %q:\n%s", entry, body)
		}
	}
}

func (fixture *deployFixture) mustRenderTemplate(t *testing.T, template string, label string) []byte {
	t.Helper()
	current := executor{
		config:  fixture.config,
		files:   fixture.fs,
		runner:  fixture.runner,
		logger:  newLogger(ioDiscard{}),
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
		outcome: outcome{action: actionUnset, reason: "", cause: causeUnset},
	}
	return current.renderTemplate(template, label)
}

type fakeFileSystem struct {
	files   map[string][]byte
	exists  map[string]bool
	written map[string][]byte
}

func newFakeFileSystem() *fakeFileSystem {
	return &fakeFileSystem{
		files:   map[string][]byte{},
		exists:  map[string]bool{},
		written: map[string][]byte{},
	}
}

func (fs *fakeFileSystem) readFile(path string) ([]byte, error) {
	body, ok := fs.files[path]
	if !ok {
		return nil, fmt.Errorf("read missing file %s", path)
	}
	return append([]byte(nil), body...), nil
}

func (fs *fakeFileSystem) writeFile(path string, body []byte, perm os.FileMode) error {
	_ = perm
	fs.exists[path] = true
	fs.files[path] = append([]byte(nil), body...)
	fs.written[path] = append([]byte(nil), body...)
	return nil
}

func (fs *fakeFileSystem) mkdirAll(path string, perm os.FileMode) error {
	_ = perm
	fs.exists[path] = true
	return nil
}

func (fs *fakeFileSystem) pathExists(path string) (bool, error) {
	return fs.exists[path], nil
}

func (fs *fakeFileSystem) wrotePath(path string) bool {
	_, ok := fs.written[path]
	return ok
}

type fakeRunner struct {
	results map[string]commandResult
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{results: map[string]commandResult{}}
}

func (runner *fakeRunner) run(ctx context.Context, current command) commandResult {
	_ = ctx
	result, ok := runner.results[current.display()]
	if !ok {
		return commandResult{output: "", exitCode: 127, err: fmt.Errorf("unexpected command %s", current.display())}
	}
	return result
}

func (runner *fakeRunner) setSuccess(current command, output string) {
	runner.results[current.display()] = commandResult{output: output, exitCode: 0, err: nil}
}

func (runner *fakeRunner) setFailure(current command, exitCode int, output string) {
	runner.results[current.display()] = commandResult{output: output, exitCode: exitCode, err: fmt.Errorf("exit status %d", exitCode)}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
