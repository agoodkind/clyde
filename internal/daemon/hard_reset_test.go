package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/mitm/capture"
)

func resetTestRoots(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "clyde-reset-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	for key, directory := range map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config & custom", "XDG_STATE_HOME": "state", "XDG_CACHE_HOME": "cache", "XDG_RUNTIME_DIR": "run", "CODEX_HOME": "codex", "CLAUDE_CONFIG_DIR": "claude", "CLYDE_CURSOR_DATA_DIRS": "cursor", "CLYDE_ZED_DATA_DIRS": "zed"} {
		t.Setenv(key, filepath.Join(root, directory))
	}
	for _, key := range []string{"LAUNCHD_LABEL", "LAUNCHD_DOMAIN", "LAUNCHD_PLIST", "SYSTEMD_UNIT", "SYSTEMD_USER_UNIT", "CLYDE_DAEMON_SUPERVISOR_SOCKET", "CLYDE_SLOG_PATH", "CLYDE_CODEX_LOG_PATH", "CLYDE_ANTHROPIC_LOG_PATH"} {
		t.Setenv(key, "")
	}
	return root
}

func writeResetFixture(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHardResetRejectsProtectedTargetsBeforeServiceRemoval(t *testing.T) {
	root := resetTestRoots(t)
	for _, kind := range []string{"outside", "ca", "config", "credentials", "log", "directory", "symlink", "parent_symlink"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(config.DefaultStateDir(), "capture.db")
			extra := ""
			switch kind {
			case "outside":
				path = filepath.Join(root, "sibling", "database.db")
			case "ca":
				extra = "\n[mitm.ca]\nkey_path = " + strconv.Quote(path) + "\n"
			case "config":
				path = config.GlobalConfigPath()
			case "credentials":
				extra = "\n[export]\nopenai_api_key_file = " + strconv.Quote(path) + "\n"
			case "log":
				extra = "\n[logging.paths]\ndaemon = " + strconv.Quote(path) + "\n"
			case "directory":
				path = filepath.Join(config.DefaultStateDir(), "directory")
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink", "parent_symlink":
				sentinel := filepath.Join(root, kind, "provider.db")
				writeResetFixture(t, sentinel, []byte("protected provider bytes"))
				link := filepath.Join(config.DefaultStateDir(), kind)
				if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
					t.Fatal(err)
				}
				target := sentinel
				path = link
				if kind == "parent_symlink" {
					target = filepath.Dir(sentinel)
					path = filepath.Join(link, "provider.db")
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			body := []byte("[mitm.capture_store]\ndb_path = " + strconv.Quote(path) + "\n" + extra)
			writeResetFixture(t, config.GlobalConfigPath(), body)
			var output bytes.Buffer
			if err := HardReset(t.Context(), &output); err == nil {
				t.Fatal("protected target was accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("service removal ran before rejection: %s", &output)
			}
			preserved, err := os.ReadFile(config.GlobalConfigPath())
			if err != nil || !bytes.Equal(body, preserved) {
				t.Fatalf("config changed: %v", err)
			}
		})
	}
}

func TestHardResetInventoryPreservesConfigWithSharedRoots(t *testing.T) {
	resetTestRoots(t)
	t.Setenv("XDG_CONFIG_HOME", os.Getenv("XDG_STATE_HOME"))
	t.Setenv("XDG_CACHE_HOME", os.Getenv("XDG_STATE_HOME"))
	body := []byte("# shared parent, preserved bytes\n")
	writeResetFixture(t, config.GlobalConfigPath(), body)
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := hardResetTargets(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.Path == config.GlobalConfigPath() {
			t.Fatal("config joined deletion inventory")
		}
	}
	got, err := os.ReadFile(config.GlobalConfigPath())
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("shared-root config changed: %v", err)
	}
}

func TestHardResetRejectsRootResolvingIntoProviderData(t *testing.T) {
	resetTestRoots(t)
	provider := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(provider, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(os.Getenv("XDG_STATE_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(provider, config.DefaultStateDir()); err != nil {
		t.Fatal(err)
	}
	writeResetFixture(t, config.GlobalConfigPath(), []byte("# preserve provider root\n"))
	var output bytes.Buffer
	if err := HardReset(t.Context(), &output); err == nil {
		t.Fatal("provider-backed Clyde root was accepted")
	}
	if output.Len() != 0 {
		t.Fatalf("service removal occurred: %s", &output)
	}
}

func TestHardResetCommandUsesNativeInstallerAndPreservesProtectedFiles(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native service platforms only")
	}
	// Build before HOME changes so Go's own caches stay independent of fixtures.
	repository, err := daemonTestRepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	buildRoot := t.TempDir()
	cliBinary := filepath.Join(buildRoot, "clyde")
	managerBinary := filepath.Join(buildRoot, "manager")
	for _, build := range []struct{ output, source string }{{cliBinary, "./cmd/clyde"}, {managerBinary, "./internal/daemon/testdata/reset-service-manager"}} {
		command := exec.CommandContext(t.Context(), "go", "build", "-o", build.output, build.source)
		command.Dir = repository
		started := time.Now()
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build fixture: %v\n%s", err, output)
		}
		info, err := os.Stat(build.output)
		if err != nil || info.ModTime().Before(started.Add(-time.Second)) {
			t.Fatalf("fixture binary not freshly built: %v", err)
		}
	}
	root := resetTestRoots(t)
	bin := filepath.Join(root, "clyde")
	body, err := os.ReadFile(cliBinary)
	if err != nil {
		t.Fatal(err)
	}
	writeResetFixture(t, bin, body)
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	managerDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(managerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"launchctl", "systemctl"} {
		if err := os.Symlink(managerBinary, filepath.Join(managerDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", managerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLYDE_RESET_TEST_ROOT", root)
	t.Setenv("INSTALL_BIN", filepath.Join(root, "stale-must-not-run"))
	t.Setenv("LOG_PATH", filepath.Join(root, "native.log"))
	configBody := []byte("# Preserve these exact bytes.\n[logging.cleanup]\nenabled = false\n[mitm.capture_store]\ndb_path = " + strconv.Quote(filepath.Join(config.DefaultStateDir(), "custom", "capture.db")) + "\n")
	writeResetFixture(t, config.GlobalConfigPath(), configBody)
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := hardResetTargets(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var inventory []string
	for _, target := range targets {
		if target.Path == config.DaemonSocketPath() || target.Path == daemonsupervisor.SocketPath(config.RuntimeDir()) {
			continue
		}
		writeResetFixture(t, target.Path, []byte("old incompatible store bytes"))
		inventory = append(inventory, target.Path)
	}
	writeResetFixture(t, filepath.Join(root, "inventory"), []byte(strings.Join(inventory, "\n")))
	protected := []string{config.GlobalConfigPath(), cfg.MITM.CA.CertPath, cfg.MITM.CA.KeyPath, filepath.Join(config.DefaultStateDir(), "logs", "retained.jsonl"), filepath.Join(config.DefaultStateDir(), "exports", "transcript.md"), filepath.Join(config.DefaultStateDir(), "sibling.db"), filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), filepath.Join(os.Getenv("CODEX_HOME"), "state_5.sqlite"), filepath.Join(os.Getenv("CLYDE_CURSOR_DATA_DIRS"), "User", "globalStorage", "state.vscdb"), filepath.Join(root, "lm-semantic-search", "collection.db"), filepath.Join(root, "sibling-repo", "database.db")}
	for _, path := range protected {
		if path != config.GlobalConfigPath() {
			writeResetFixture(t, path, []byte("protected sentinel: "+path))
		}
	}
	before := make(map[string][]byte)
	for _, path := range protected {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := capture.Open(t.Context(), capture.Config{DBPath: cfg.MITM.CaptureStore.DBPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		_ = store.Close(t.Context(), "reset test")
		t.Fatal("old fixture database unexpectedly decoded")
	}
	// SQLite can remove malformed sidecars during its failed open. Restore the
	// complete old-file inventory before testing the reset boundary.
	for _, path := range inventory {
		writeResetFixture(t, path, []byte("old incompatible store bytes"))
	}
	t.Cleanup(func() {
		processes, err := hardResetProcesses(context.Background(), bin)
		if err != nil {
			t.Error(err)
			return
		}
		if err := stopResetProcesses(context.Background(), processes); err != nil {
			t.Error(err)
		}
	})
	command := exec.CommandContext(t.Context(), bin, "daemon", "hard-reset")
	output, err := command.CombinedOutput()
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(root, "new-daemon.log"))
		t.Fatalf("hard reset: %v\n%s\nnew daemon:\n%s", err, output, log)
	}
	if !strings.Contains(string(output), "installation and status check succeeded") {
		t.Fatalf("missing installed status: %s", output)
	}
	for _, path := range inventory {
		body, err := os.ReadFile(path)
		if err == nil && bytes.Contains(body, []byte("old incompatible store bytes")) {
			t.Fatalf("old data survived: %s", path)
		}
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("protected bytes changed: %s: %v", path, err)
		}
	}
	store, err = capture.Open(t.Context(), capture.Config{DBPath: cfg.MITM.CaptureStore.DBPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("fresh store creation: %v", err)
	}
	if err := store.Close(t.Context(), "reset test"); err != nil {
		t.Fatal(err)
	}
	status := InspectStatus(t.Context())
	if !status.SupervisorResponding || !status.DaemonResponding {
		t.Fatalf("new daemon unavailable: %+v", status)
	}
	commands, err := os.ReadFile(filepath.Join(root, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "lm-semantic-search") || strings.Contains(string(commands), "hooks") {
		t.Fatalf("reset exceeded service scope: %s", commands)
	}
	// The started daemon must use the reset cache root, not the caller's default.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(conversation.CachePath()); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(conversation.CachePath()); err != nil {
		t.Fatal("new daemon did not populate the reset conversation cache")
	}
	oldProcesses, err := hardResetProcesses(t.Context(), bin)
	if err != nil || len(oldProcesses) < 2 {
		t.Fatalf("identify installed supervisor and worker: %v, %v", oldProcesses, err)
	}
	// Exercise failure after teardown while real old processes are still alive.
	writeResetFixture(t, filepath.Join(root, "inventory"), nil)
	t.Setenv("CLYDE_RESET_TEST_LATE_WORKER", "1")
	t.Setenv("CLYDE_RESET_TEST_FAIL", "bootstrap")
	if runtime.GOOS == "linux" {
		t.Setenv("CLYDE_RESET_TEST_FAIL", "restart")
	}
	command = exec.CommandContext(t.Context(), bin, "daemon", "hard-reset")
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "data reset; native installation failed") {
		t.Fatalf("installation failure was not reported: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "installation and status check succeeded") {
		t.Fatal("failed install claimed success")
	}
	for _, process := range oldProcesses {
		alive, err := sameResetProcess(t.Context(), process)
		if err != nil || alive {
			t.Fatalf("old process %d survived reset: %v", process.PID, err)
		}
	}
	latePIDBytes, err := os.ReadFile(filepath.Join(root, "late-pid"))
	if err != nil {
		t.Fatal(err)
	}
	latePID, err := strconv.Atoi(string(latePIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	if process, err := readResetProcess(t.Context(), latePID); err == nil {
		t.Fatalf("worker spawned during teardown survived: %d", process.PID)
	}
	if _, err := os.Stat(cfg.MITM.CaptureStore.DBPath); !os.IsNotExist(err) {
		t.Fatalf("data survived failed installation: %v", err)
	}
	t.Setenv("CLYDE_RESET_TEST_FAIL", "")
	t.Setenv("CLYDE_RESET_TEST_LATE_WORKER", "")
	t.Setenv("CLYDE_RESET_TEST_ABSENT", "1")
	if err := os.Remove(filepath.Join(root, "installed")); err != nil {
		t.Fatal(err)
	}
	command = exec.CommandContext(t.Context(), bin, "daemon", "hard-reset")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("rerun with absent data/registration: %v\n%s", err, output)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("rerun changed protected bytes: %s: %v", path, err)
		}
	}
}

func TestResetProcessIdentityRejectsSiblingRootsAndCommands(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	socket := "/tmp/reset-fixture/daemon.supervisor.sock"
	process := resetProcess{Executable: executable, Arguments: []string{executable, "daemon", "worker"}, SupervisorSocket: socket}
	if !resetProcessMatches(process, executable, socket, false) {
		t.Fatal("owned worker rejected")
	}
	if resetProcessMatches(process, executable, socket+"-sibling", false) {
		t.Fatal("sibling runtime accepted")
	}
	process.Arguments = []string{executable, "daemon", "hard-reset"}
	if resetProcessMatches(process, executable, socket, false) {
		t.Fatal("operator process accepted as worker")
	}
	process.Arguments = []string{executable, "daemon", "run"}
	if !resetProcessMatches(process, executable, socket, true) {
		t.Fatal("identified supervisor rejected")
	}
	if resetProcessMatches(process, executable+"-other", socket, true) {
		t.Fatal("different executable accepted")
	}
}

func TestResetReadsCurrentProcessIdentity(t *testing.T) {
	process, err := readResetProcess(t.Context(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if process.PID != os.Getpid() || process.Executable == "" || process.Started == "" {
		t.Fatalf("incomplete process identity: %+v", process)
	}
	alive, err := sameResetProcess(t.Context(), process)
	if err != nil || !alive {
		t.Fatalf("current process identity did not match: %v", err)
	}
	process.Started = "different start stamp"
	alive, err = sameResetProcess(t.Context(), process)
	if err != nil || alive {
		t.Fatalf("reused PID accepted: %v", err)
	}
}

func TestResetStopsOnlyTheIdentifiedFixtureProcess(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	process, err := readResetProcess(t.Context(), child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := stopResetProcesses(t.Context(), []resetProcess{process}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Fatalf("test process affected: %v", err)
	}
}
