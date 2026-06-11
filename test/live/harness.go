//go:build live

// Package live holds build-tagged live-daemon validation for the lifecycle
// group, quiet-wait reload, and in-process config apply. It boots the worktree
// daemon binary in fully isolated temp XDG roots on fake ports so a run can
// never touch the operator's production daemon. Run with: go test -tags live
// ./test/live/.
package live

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakePorts holds the throwaway ports the live daemon binds. Every value differs
// from a production default so a live run can never collide with the operator's
// daemon. The adapter is disabled in the fake config, so only the MITM listener
// port is bound.
type fakePorts struct {
	MITMPort int // 58723 (production cli.claude-code is 48723)
}

// harness owns the temp state/config/runtime roots, the fake config, and the
// booted daemon subprocess for one live test. All daemon I/O is redirected into
// temp dirs via the XDG env vars so nothing lands in the operator's real clyde
// state, and the daemon binds only the fake MITM port.
type harness struct {
	stateRoot    string
	configRoot   string
	runtimeRoot  string
	cfg          fakePorts
	binPath      string
	configPath   string
	daemonLog    string
	prodPidsPre  map[int]bool
	cmd          *exec.Cmd
}

const (
	fakeMITMPort       = 58723
	daemonReadyMarker  = "daemon.mitm.started"
	hotApplyMarker     = "daemon.config.applied_in_process"
	reloadTriggeredKey = "daemon.config_watch.reload_triggered"
	classifiedMarker   = "daemon.config_watch.classified"
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	// The runtime root holds the supervisor's Unix control socket, whose path
	// must fit macOS's ~104-char sun_path limit. t.TempDir() lives under a long
	// /var/folders path that overflows it, so the runtime root gets a short
	// /tmp dir instead; state and config can use the long temp dirs.
	runtimeRoot, err := os.MkdirTemp("/tmp", "clyde-rt-")
	if err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	h := &harness{
		stateRoot:   t.TempDir(),
		configRoot:  t.TempDir(),
		runtimeRoot: runtimeRoot,
		cfg:         fakePorts{MITMPort: fakeMITMPort},
		binPath:     buildWorktreeBinary(t),
		prodPidsPre: snapshotProductionPids(),
		cmd:         nil,
	}
	h.configPath = filepath.Join(h.configRoot, "clyde", "config.toml")
	h.daemonLog = filepath.Join(h.stateRoot, "daemon-run.out")
	if err := os.MkdirAll(filepath.Dir(h.configPath), 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	return h
}

// preflight fails when the fake MITM port is already listening or a temp root is
// missing, so the suite aborts before booting instead of risking a collision.
func (h *harness) preflight() error {
	if portListening(h.cfg.MITMPort) {
		return fmt.Errorf("preflight: fake MITM port %d already listening; refusing to run", h.cfg.MITMPort)
	}
	for _, root := range []string{h.stateRoot, h.configRoot, h.runtimeRoot} {
		isTemp := strings.Contains(root, os.TempDir()) ||
			strings.HasPrefix(root, "/var") ||
			strings.HasPrefix(root, "/private") ||
			strings.HasPrefix(root, "/tmp/")
		if !isTemp {
			return fmt.Errorf("preflight: root %q is not a temp dir; refusing to run", root)
		}
	}
	return nil
}

// occupyPort binds the fake port so a test can assert preflight rejects it.
func (h *harness) occupyPort(t *testing.T, port int) func() {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Fatalf("occupy port %d: %v", port, err)
	}
	return func() { _ = ln.Close() }
}

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("[::1]:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// writeConfig writes the fake MITM-only config. providers and the MITM port are
// parameterized so tests can edit a hot field (providers) or a topology field
// (port).
func (h *harness) writeConfig(t *testing.T, mitmPort int, providers []string) {
	t.Helper()
	providerList := ""
	for i, p := range providers {
		if i > 0 {
			providerList += ", "
		}
		providerList += fmt.Sprintf("%q", p)
	}
	caDir := filepath.Join(h.stateRoot, "ca")
	captureDir := filepath.Join(h.stateRoot, "mitm")
	// The capture store and CA loader do not create their parent dirs, so the
	// harness ensures they exist before the daemon opens them.
	for _, dir := range []string{caDir, captureDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	content := fmt.Sprintf(`[logging]
level = "debug"

[conversation.semantic]
enabled = false

[adapter]
enabled = false

[mitm]
enabled_default = true
capture_dir = %q
providers = [%s]

[mitm.ca]
cert_path = %q
key_path = %q

[mitm.capture_store]
db_path = %q

[mitm.cli.claude-code]
host = "localhost"
port = %d
`,
		captureDir, providerList,
		filepath.Join(caDir, "ca.crt"), filepath.Join(caDir, "ca.key"),
		filepath.Join(captureDir, "capture.db"), mitmPort)
	if err := os.WriteFile(h.configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write fake config: %v", err)
	}
}

// env returns the XDG overrides that point the daemon at temp roots, isolating
// its socket, capture db, and logs from production.
func (h *harness) env() []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+h.stateRoot,
		"XDG_CONFIG_HOME="+h.configRoot,
		"XDG_RUNTIME_DIR="+h.runtimeRoot,
	)
}

// boot starts `clyde daemon run` in its own process group with the temp env, and
// waits until the MITM listener is up. The process group lets teardown kill the
// supervisor and its worker child together.
func (h *harness) boot(t *testing.T) {
	t.Helper()
	if err := h.preflight(); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	out, err := os.Create(h.daemonLog)
	if err != nil {
		t.Fatalf("create daemon log: %v", err)
	}
	cmd := exec.Command(h.binPath, "daemon", "run")
	cmd.Env = h.env()
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	h.cmd = cmd
	t.Cleanup(func() { h.teardown(t) })
	if !h.waitForDaemonLog(daemonReadyMarker, 15*time.Second) {
		dump := h.dumpLogsOnFailure(t)
		t.Fatalf("daemon did not become ready (no %q); logs dumped to %s", daemonReadyMarker, dump)
	}
}

// dumpLogsOnFailure copies the daemon run output and every state JSONL to a
// persistent /tmp dir so a boot failure can be inspected after t.TempDir
// cleanup. Returns the dump dir.
func (h *harness) dumpLogsOnFailure(t *testing.T) string {
	t.Helper()
	dump, err := os.MkdirTemp("/tmp", "clyde-live-fail-")
	if err != nil {
		return "(mkdir failed: " + err.Error() + ")"
	}
	if data, readErr := os.ReadFile(h.daemonLog); readErr == nil {
		_ = os.WriteFile(filepath.Join(dump, "run.out"), data, 0o600)
	}
	idx := 0
	_ = filepath.WalkDir(h.stateRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if data, readErr := os.ReadFile(path); readErr == nil {
			_ = os.WriteFile(filepath.Join(dump, fmt.Sprintf("log-%d.jsonl", idx)), data, 0o600)
			idx++
		}
		return nil
	})
	return dump
}

// waitForDaemonLog polls the daemon stdout/stderr file plus the concern logs for
// marker until timeout. Returns true if found.
func (h *harness) waitForDaemonLog(marker string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.logContains(marker) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// logContains reports whether marker appears in the daemon run output file or
// any JSONL log under the temp state root (the daemon writes its process log and
// concern logs in a clyde/ subtree whose exact layout is discovered, not
// assumed).
func (h *harness) logContains(marker string) bool {
	if fileContains(h.daemonLog, marker) {
		return true
	}
	found := false
	_ = filepath.WalkDir(h.stateRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return nil
		}
		if strings.HasSuffix(path, ".jsonl") && fileContains(path, marker) {
			found = true
		}
		return nil
	})
	return found
}

func fileContains(path, marker string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), marker) {
			return true
		}
	}
	return false
}

// teardown kills the daemon process group, removes temp dirs (via t.TempDir
// cleanup), and asserts the production daemon pids are unchanged.
func (h *harness) teardown(t *testing.T) {
	t.Helper()
	if h.cmd != nil && h.cmd.Process != nil {
		_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = h.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
		}
		h.cmd = nil
	}
	h.assertProductionUntouched(t)
}

// assertProductionUntouched confirms the production daemon process set is
// unchanged across the live run, proving isolation held.
func (h *harness) assertProductionUntouched(t *testing.T) {
	t.Helper()
	post := snapshotProductionPids()
	for pid := range h.prodPidsPre {
		if !post[pid] {
			t.Errorf("production daemon pid %d disappeared during live run; isolation breached", pid)
		}
	}
}

// snapshotProductionPids returns the set of running production clyde daemon
// pids (those NOT under a temp XDG root). It shells to pgrep and filters by the
// absence of a temp runtime dir in the environment is not feasible here, so it
// matches the production binary path; the test binary lives under a temp dir.
func snapshotProductionPids() map[int]bool {
	out, err := exec.Command("pgrep", "-f", "clyde daemon").Output()
	pids := map[int]bool{}
	if err != nil {
		return pids
	}
	for _, line := range strings.Fields(string(out)) {
		var pid int
		if _, scanErr := fmt.Sscanf(line, "%d", &pid); scanErr == nil && pid > 0 {
			pids[pid] = true
		}
	}
	return pids
}

func buildWorktreeBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "clyde-live")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/clyde")
	cmd.Dir = repoRoot(t)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build worktree binary: %v", err)
	}
	return out
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// touchReload nudges the daemon by editing the config and returns once the
// watcher has classified the change, so a test can then assert the route taken.
func (h *harness) waitForClassification(timeout time.Duration) bool {
	return h.waitForDaemonLog(classifiedMarker, timeout)
}

var _ = context.Background
