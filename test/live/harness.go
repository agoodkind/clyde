//go:build live

// Package live holds build-tagged live-daemon validation for the lifecycle
// group, quiet-wait reload, and in-process config apply. It boots the worktree
// daemon binary in fully isolated temp XDG roots on fake ports so a run can
// never touch the operator's production daemon. Run with: go test -tags live
// ./test/live/.
package live

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// fakePorts holds the throwaway ports the live daemon binds. Every value differs
// from a production default so a live run can never collide with the operator's
// daemon: adapter 11434 to 21434, cursor ingress 11435 to 21435, MITM 487xx to
// 587xx. pprof is left off.
type fakePorts struct {
	MITMPort      int // 58723 (production cli.claude-code is 48723)
	AdapterPort   int // 21434 (production 11434)
	CursorPort    int // 21435 (production 11435)
	TopologyPort  int // 21436, the moved-to adapter port for the topology test
	MovedMITMPort int // 58724, the moved-to MITM port for the reload topology test
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
	extraEnv     []string
	requireToken string
	cmd          *exec.Cmd
}

const (
	fakeMITMPort       = 58723
	fakeAdapterPort    = 21434
	fakeCursorPort     = 21435
	fakeTopologyPort   = 21436
	daemonReadyMarker  = "daemon.worker.ready"
	hotApplyMarker     = "daemon.config.applied_in_process"
	reloadTriggeredKey = "daemon.config_watch.reload_triggered"
	classifiedMarker   = "daemon.config_watch.classified"
	workerStartedKey   = "daemon.supervisor.worker_started"
)

// resolveFakePorts reads each throwaway port from its CLYDE_TEST_*_PORT env var,
// falling back to the package default when the var is unset. Each worktree or
// terminal can export a distinct set so several isolated daemons run at once
// without colliding, mirroring lmd's LMD_TEST_PORT. The moved MITM port defaults to
// one above the resolved MITM port so an overridden base still gets a paired port,
// stepping down when the MITM port is already at the maximum so the default stays a
// valid port.
func resolveFakePorts() fakePorts {
	mitm := envPortOr("CLYDE_TEST_MITM_PORT", fakeMITMPort)
	movedDefault := mitm + 1
	if movedDefault > maxTCPPort {
		movedDefault = mitm - 1
	}
	return fakePorts{
		MITMPort:      mitm,
		AdapterPort:   envPortOr("CLYDE_TEST_ADAPTER_PORT", fakeAdapterPort),
		CursorPort:    envPortOr("CLYDE_TEST_CURSOR_PORT", fakeCursorPort),
		TopologyPort:  envPortOr("CLYDE_TEST_TOPOLOGY_PORT", fakeTopologyPort),
		MovedMITMPort: envPortOr("CLYDE_TEST_MOVED_MITM_PORT", movedDefault),
	}
}

// maxTCPPort is the highest valid TCP port, used to reject out-of-range overrides.
const maxTCPPort = 65535

// envPortOr returns the integer value of the env var named name, or def when the
// var is unset, blank, or does not parse as a port in the range 1 to 65535. It
// trims surrounding whitespace so an accidental newline in the override does not
// silently drop back to the default.
func envPortOr(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	val, err := strconv.Atoi(raw)
	if err != nil || val < 1 || val > maxTCPPort {
		return def
	}
	return val
}

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
		cfg:         resolveFakePorts(),
		extraEnv:    nil,
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
	seen := map[int]string{}
	for _, p := range []struct {
		name string
		port int
	}{
		{"MITM", h.cfg.MITMPort},
		{"adapter", h.cfg.AdapterPort},
		{"cursor", h.cfg.CursorPort},
		{"topology", h.cfg.TopologyPort},
		{"moved MITM", h.cfg.MovedMITMPort},
	} {
		if other, dup := seen[p.port]; dup {
			return fmt.Errorf("preflight: %s port %d duplicates the %s port; set distinct CLYDE_TEST_*_PORT values", p.name, p.port, other)
		}
		seen[p.port] = p.name
		if portListening(p.port) {
			return fmt.Errorf("preflight: fake %s port %d already listening; refusing to run", p.name, p.port)
		}
	}
	for _, root := range []string{h.stateRoot, h.configRoot, h.runtimeRoot} {
		if !underTempRoot(root) {
			return fmt.Errorf("preflight: root %q is not a temp dir; refusing to run", root)
		}
	}
	return nil
}

// underTempRoot reports whether path lives under a temp root. The live harness
// puts every sandbox dir and its built binary under a temp path, so this
// distinguishes a live-test artifact from a production one at a stable location
// such as ~/.local/bin/clyde. It matches only on a path-segment boundary against
// the OS temp dir and the explicit /tmp and /private roots macOS resolves it
// through, so a production path that merely contains "tmp" as a substring, or one
// under an unrelated /var directory, is not misclassified as a temp daemon.
func underTempRoot(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range tempRoots() {
		if clean == root || strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// tempRoots returns the directories the live harness treats as temp: the OS temp
// dir plus the explicit /tmp and /private roots macOS resolves it through. The OS
// temp dir covers t.TempDir sandboxes and the built test binary; the /tmp roots
// cover the short runtime dir the harness makes with os.MkdirTemp("/tmp", ...).
func tempRoots() []string {
	roots := []string{"/tmp", "/private/tmp", "/private/var/folders"}
	if osTemp := filepath.Clean(os.TempDir()); osTemp != "" && osTemp != "." {
		roots = append(roots, osTemp)
	}
	return roots
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
	base := append(os.Environ(),
		"XDG_STATE_HOME="+h.stateRoot,
		"XDG_CONFIG_HOME="+h.configRoot,
		"XDG_RUNTIME_DIR="+h.runtimeRoot,
	)
	return append(base, h.extraEnv...)
}

// writeAdapterConfig writes a bootable adapter-enabled fake config on the fake
// adapter/cursor ports with MITM disabled. adapterPort lets the topology test
// move the listener. The passthroughURL is the OpenAI-compatible upstream the
// "local-test" model routes to (a slow local server for the in-flight tests).
// extraModels are extra [adapter.models.<name>] passthrough aliases appended so a
// hot apply can add a model and assert it serves.
func (h *harness) writeAdapterConfig(t *testing.T, adapterPort int, passthroughURL string, extraModels []string) {
	t.Helper()
	h.writeAdapterConfigWithCapture(t, adapterPort, passthroughURL, extraModels, false)
}

// writeCombinedAdapterMITMConfig enables adapter ingress capture and the MITM
// listener against the same isolated capture store used by the daemon.
func (h *harness) writeCombinedAdapterMITMConfig(t *testing.T, adapterPort int, passthroughURL string) {
	t.Helper()
	h.writeAdapterConfigWithCapture(t, adapterPort, passthroughURL, nil, true)
}

func (h *harness) writeAdapterConfigWithCapture(t *testing.T, adapterPort int, passthroughURL string, extraModels []string, combinedCapture bool) {
	t.Helper()
	caDir := filepath.Join(h.stateRoot, "ca")
	captureDir := filepath.Join(h.stateRoot, "mitm")
	for _, dir := range []string{caDir, captureDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	extra := ""
	for _, name := range extraModels {
		extra += fmt.Sprintf("\n[adapter.models.%q]\nprovider = \"passthrough_override\"\nwire_model = %q\nprofile = \"local-test\"\npassthrough_override = \"local\"\nadvertise = true\n", name, name)
	}
	requireToken := ""
	if h.requireToken != "" {
		requireToken = fmt.Sprintf("\nrequire_token = %q", h.requireToken)
	}
	captureIngress := ""
	mitmConfig := "[mitm]\nenabled_default = false\n"
	if combinedCapture {
		captureIngress = "\ncapture_ingress = true"
		mitmConfig = fmt.Sprintf(`[mitm]
enabled_default = true
capture_dir = %q
providers = ["anthropic"]

[mitm.ca]
cert_path = %q
key_path = %q

[mitm.capture_store]
db_path = %q

[mitm.cli.claude-code]
host = "localhost"
port = %d
`, captureDir, filepath.Join(caDir, "ca.crt"), filepath.Join(caDir, "ca.key"), filepath.Join(captureDir, "capture.db"), h.cfg.MITMPort)
	}
	content := fmt.Sprintf(`[logging]
level = "debug"

[conversation.semantic]
enabled = false

[adapter]
enabled = true
direct_oauth = false
host = "[::1]"
port = %d
cursor_ingress_port = %d
default_model = "local-test"%s%s

[adapter.openai_compat_passthrough]
base_url = %q

[adapter.passthrough_overrides.local]
base_url = %q

[adapter.model_profiles.local-test]
contexts = [{ name = "standard", tokens = 8000 }]
max_output_tokens = 8000
reasoning_efforts = ["medium"]
default_effort = "medium"
supports_tools = true
supports_vision = false

[adapter.models."local-test"]
provider = "passthrough_override"
wire_model = "local-test"
profile = "local-test"
passthrough_override = "local"
advertise = true

[adapter.client_identity]
beta_header = "test-beta"
user_agent = "clyde-live-test/0.0"
system_prompt_prefix = "test-prefix"
stainless_package_version = "0.0.0"
stainless_runtime = "node"
stainless_runtime_version = "v0.0.0"
cc_version = "0.0.0"
cc_entrypoint = "test"

%s
%s`, adapterPort, h.cfg.CursorPort, requireToken, captureIngress, passthroughURL, passthroughURL, extra, mitmConfig)
	if err := os.WriteFile(h.configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write fake adapter config: %v", err)
	}
}

// writeReloadEdit rewrites the adapter config with a reload-routed change set
// (require_token, which is not in the hot set), so the watcher classifies the
// edit as reload and the quiet-wait gates it.
func (h *harness) writeReloadEdit(t *testing.T, passthroughURL string) {
	t.Helper()
	h.requireToken = "live-reload-token"
	h.writeAdapterConfig(t, h.cfg.AdapterPort, passthroughURL, nil)
}

// writeCombinedReloadEdit preserves combined capture while changing the
// reload-routed adapter token field.
func (h *harness) writeCombinedReloadEdit(t *testing.T, passthroughURL string) {
	t.Helper()
	h.requireToken = "live-reload-token"
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, passthroughURL)
}

// latestWorkerPid parses the most recent daemon.supervisor.worker_started pid
// from the daemon log. It is the current worker pid: a hot apply leaves it
// unchanged, a reload or rebind advances it.
func (h *harness) latestWorkerPid() int {
	pid := 0
	_ = filepath.WalkDir(h.stateRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.Contains(line, workerStartedKey) {
				continue
			}
			if p := extractJSONInt(line, "pid"); p > 0 {
				pid = p
			}
		}
		return nil
	})
	return pid
}

// pidOnPort returns the pid listening on the given loopback TCP port, or 0 if
// none. It uses lsof so the OS, not a log, is the source of truth for which
// process owns the listener: the basis for the pid-changed/port-rebound
// assertion.
func pidOnPort(port int) int {
	out, err := exec.Command("lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Fields(string(out)) {
		var pid int
		if _, scanErr := fmt.Sscanf(line, "%d", &pid); scanErr == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// waitForPidOnPort polls until a process listens on port (returning its pid) or
// timeout (returning 0).
func waitForPidOnPort(port int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid := pidOnPort(port); pid > 0 {
			return pid
		}
		time.Sleep(150 * time.Millisecond)
	}
	return 0
}

// extractJSONInt pulls an integer field value out of a JSON log line by key.
func extractJSONInt(line, key string) int {
	marker := fmt.Sprintf("%q:", key)
	idx := strings.Index(line, marker)
	if idx < 0 {
		return 0
	}
	rest := line[idx+len(marker):]
	val := 0
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			val = val*10 + int(r-'0')
			continue
		}
		if val > 0 {
			break
		}
		if r == ' ' {
			continue
		}
		break
	}
	return val
}

// adapterGet performs a GET against the live adapter on the fake port and
// returns the body, or fails the test. It dials [::1] on adapterPort.
func (h *harness) adapterGet(t *testing.T, adapterPort int, path string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("[::1]:%d", adapterPort), 2*time.Second)
	if err != nil {
		t.Fatalf("dial adapter: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", path)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write adapter request: %v", err)
	}
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 4096)
	for {
		n, readErr := conn.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if readErr != nil {
			break
		}
	}
	return string(buf)
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
	// A reload- or rebind-spawned worker re-parents to PID 1 and escapes the
	// supervisor's process group, so kill anything still running the test binary
	// by its unique temp path. This only ever matches this test's daemon, never
	// the production binary at ~/.local/bin/clyde.
	_ = exec.Command("pkill", "-f", h.binPath).Run()
	time.Sleep(200 * time.Millisecond)
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

// snapshotProductionPids returns the set of running production clyde daemon pids,
// meaning those whose binary lives at a stable install path rather than a temp
// root. It lists every "clyde daemon" pid and keeps only the ones ps confirms are
// production. Keeping only confirmed production pids keeps assertProductionUntouched
// correct when several isolated instances run at once: a concurrent test daemon's
// shutdown no longer looks like a production pid disappearing.
func snapshotProductionPids() map[int]bool {
	out, err := exec.Command("pgrep", "-f", "clyde daemon").Output()
	pids := map[int]bool{}
	if err != nil {
		return pids
	}
	for _, line := range strings.Fields(string(out)) {
		var pid int
		if _, scanErr := fmt.Sscanf(line, "%d", &pid); scanErr != nil || pid <= 0 {
			continue
		}
		if isProductionDaemon(pid) {
			pids[pid] = true
		}
	}
	return pids
}

// isProductionDaemon reports whether pid is the operator's installed production
// daemon rather than a live-test daemon. It requires ps to resolve the command AND
// that command's executable path to sit outside every temp root. A ps failure or
// empty output is treated as not-production so a temp daemon caught exiting mid
// shutdown is skipped instead of landing in the production set, where it would
// later trip assertProductionUntouched as a false isolation breach.
func isProductionDaemon(pid int) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	command := strings.TrimSpace(string(out))
	if command == "" {
		return false
	}
	execPath := command
	if idx := strings.IndexByte(command, ' '); idx >= 0 {
		execPath = command[:idx]
	}
	return !underTempRoot(execPath)
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

// slowUpstream is a local OpenAI-compatible upstream the adapter passthrough
// routes to. Each request blocks until released, so a chat request through the
// adapter holds its egress session in flight while the test edits the config.
type slowUpstream struct {
	server   *httptest.Server
	released chan struct{}
	gotReq   chan struct{}
}

// startSlowUpstream starts the upstream on an ephemeral port. requests block
// until release() is called, then return a minimal chat completion.
func startSlowUpstream(t *testing.T) *slowUpstream {
	t.Helper()
	u := &slowUpstream{
		server:   nil,
		released: make(chan struct{}),
		gotReq:   make(chan struct{}, 8),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case u.gotReq <- struct{}{}:
		default:
		}
		<-u.released
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":0,"model":"local-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	u.server = httptest.NewServer(handler)
	t.Cleanup(func() {
		u.releaseAll()
		u.server.Close()
	})
	return u
}

// baseURL returns the upstream base URL with the /v1 suffix the adapter
// passthrough expects.
func (u *slowUpstream) baseURL() string { return u.server.URL + "/v1" }

// releaseAll unblocks every current and future upstream request.
func (u *slowUpstream) releaseAll() {
	select {
	case <-u.released:
	default:
		close(u.released)
	}
}

// waitForRequest blocks until the upstream has received at least one request, so
// a test knows the adapter egress session is in flight.
func (u *slowUpstream) waitForRequest(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-u.gotReq:
	case <-time.After(timeout):
		t.Fatalf("upstream received no request within %s", timeout)
	}
}

// postChat sends a non-blocking chat completion to the adapter routed to the
// passthrough model, returning a channel that receives the raw response when the
// request completes. The request holds an adapter egress session until the
// upstream is released.
func (h *harness) postChat(t *testing.T, adapterPort int) <-chan string {
	t.Helper()
	done := make(chan string, 1)
	body := `{"model":"local-test","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
	go func() {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("[::1]:%d", adapterPort), 2*time.Second)
		if err != nil {
			done <- "dial-error: " + err.Error()
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
		req := fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		if _, err := conn.Write([]byte(req)); err != nil {
			done <- "write-error: " + err.Error()
			return
		}
		buf := make([]byte, 0, 16*1024)
		tmp := make([]byte, 4096)
		for {
			n, readErr := conn.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if readErr != nil {
				break
			}
		}
		done <- string(buf)
	}()
	return done
}

type responsesUpstreamRequest struct {
	path string
	body string
}

type responsesRequestEnvelope struct {
	Input         string                      `json:"input"`
	Stream        bool                        `json:"stream"`
	CompatUnknown responsesCompatibilityProbe `json:"compat_unknown"`
}

type responsesCompatibilityProbe struct {
	Probe string `json:"probe"`
}

type responsesUpstream struct {
	server        *httptest.Server
	requests      chan responsesUpstreamRequest
	streamRelease chan struct{}
}

func startResponsesUpstream(t *testing.T) *responsesUpstream {
	t.Helper()
	upstream := &responsesUpstream{
		server:        nil,
		requests:      make(chan responsesUpstreamRequest, 8),
		streamRelease: make(chan struct{}),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		var envelope responsesRequestEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}
		upstream.requests <- responsesUpstreamRequest{path: request.URL.Path, body: string(body)}
		probeID := envelope.CompatUnknown.Probe
		w.Header().Set("X-Upstream-Marker", "responses-live")
		w.Header().Set("X-Request-Id", "upstream-"+probeID)
		if !envelope.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"resp-%s","object":"response","status":"completed","model":"local-test","output":[{"type":"message","id":"msg-%s","status":"completed","role":"assistant","content":[{"type":"output_text","text":"probe %s"}]}]}`, probeID, probeID, probeID)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-%s\",\"status\":\"in_progress\"}}\n\n", probeID)
		_, _ = fmt.Fprintf(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg-%s\",\"type\":\"message\"}}\n\n", probeID)
		_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"probe %s %s\"}\n\n", probeID, strings.Repeat("x", 16*1024))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-upstream.streamRelease
		_, _ = fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg-%s\",\"type\":\"message\",\"status\":\"completed\"}}\n\n", probeID)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-%s\",\"status\":\"completed\",\"output_text\":\"probe %s\"}}\n\n", probeID, probeID)
	})
	upstream.server = httptest.NewServer(handler)
	t.Cleanup(func() {
		upstream.releaseStreams()
		upstream.server.Close()
	})
	return upstream
}

func (u *responsesUpstream) baseURL() string {
	return u.server.URL + "/v1"
}

func (u *responsesUpstream) waitForRequest(t *testing.T, timeout time.Duration) responsesUpstreamRequest {
	t.Helper()
	select {
	case request := <-u.requests:
		return request
	case <-time.After(timeout):
		t.Fatalf("Responses upstream received no request within %s", timeout)
		return responsesUpstreamRequest{}
	}
}

func (u *responsesUpstream) releaseStreams() {
	select {
	case <-u.streamRelease:
	default:
		close(u.streamRelease)
	}
}

type responsesHTTPResponse struct {
	statusCode int
	body       string
}

func (h *harness) postResponses(t *testing.T, adapterPort int, body string) responsesHTTPResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, fmt.Sprintf("http://[::1]:%d/v1/responses", adapterPort), strings.NewReader(body))
	if err != nil {
		t.Fatalf("create Responses request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post Responses request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Responses response: %v", err)
	}
	return responsesHTTPResponse{statusCode: response.StatusCode, body: string(responseBody)}
}

type responsesStream struct {
	first chan string
	done  chan string
}

func (h *harness) startResponsesStream(t *testing.T, adapterPort int, body string) *responsesStream {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, fmt.Sprintf("http://[::1]:%d/v1/responses", adapterPort), strings.NewReader(body))
	if err != nil {
		t.Fatalf("create streaming Responses request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post streaming Responses request: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("streaming Responses status = %d, body = %s", response.StatusCode, responseBody)
	}
	stream := &responsesStream{first: make(chan string, 1), done: make(chan string, 1)}
	go func() {
		defer func() { _ = response.Body.Close() }()
		var all bytes.Buffer
		buffer := make([]byte, 32*1024)
		firstSent := false
		for {
			count, readErr := response.Body.Read(buffer)
			if count > 0 {
				chunk := string(buffer[:count])
				_, _ = all.Write(buffer[:count])
				if !firstSent {
					stream.first <- chunk
					firstSent = true
				}
			}
			if readErr != nil {
				if !firstSent {
					stream.first <- "read-error: " + readErr.Error()
				}
				stream.done <- all.String()
				return
			}
		}
	}()
	return stream
}

func (s *responsesStream) waitForFirstFrame(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case first := <-s.first:
		return first
	case <-time.After(timeout):
		t.Fatalf("Responses stream did not deliver an incremental frame within %s", timeout)
		return ""
	}
}

func (s *responsesStream) waitForCompletion(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case body := <-s.done:
		return body
	case <-time.After(timeout):
		t.Fatalf("Responses stream did not complete within %s", timeout)
		return ""
	}
}

type responsesCapture struct {
	client          string
	path            string
	status          int
	responseHeaders string
	requestBody     string
	responseBody    string
}

func (h *harness) waitForResponsesCaptures(t *testing.T, probeID string, timeout time.Duration) []responsesCapture {
	t.Helper()
	databasePath := filepath.Join(h.stateRoot, "mitm", "capture.db")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		captures, err := readResponsesCaptures(databasePath, probeID)
		if err == nil && len(captures) == 2 {
			return captures
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("capture store did not record ingress and passthrough rows for %q within %s", probeID, timeout)
	return nil
}

func readResponsesCaptures(databasePath string, probeID string) ([]responsesCapture, error) {
	database, err := sql.Open("sqlite3", "file:"+databasePath+"?_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open capture database: %w", err)
	}
	defer func() { _ = database.Close() }()
	pattern := "%" + probeID + "%"
	rows, err := database.Query(`SELECT r.client, r.path, r.status, r.resp_headers,
COALESCE(CAST(request_body.data AS TEXT), ''), COALESCE(CAST(response_body.data AS TEXT), '')
FROM requests r
LEFT JOIN bodies request_body ON request_body.request_row_id = r.id AND request_body.which = 'request'
LEFT JOIN bodies response_body ON response_body.request_row_id = r.id AND response_body.which = 'response'
WHERE r.client IN ('adapter.ingress', 'adapter.passthrough')
AND (CAST(request_body.data AS TEXT) LIKE ? OR CAST(response_body.data AS TEXT) LIKE ?)
ORDER BY r.id`, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("query capture database: %w", err)
	}
	defer func() { _ = rows.Close() }()
	captures := make([]responsesCapture, 0, 2)
	for rows.Next() {
		var capture responsesCapture
		if err := rows.Scan(&capture.client, &capture.path, &capture.status, &capture.responseHeaders, &capture.requestBody, &capture.responseBody); err != nil {
			return nil, fmt.Errorf("scan capture database: %w", err)
		}
		captures = append(captures, capture)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read capture database: %w", err)
	}
	return captures, nil
}

func uniqueProbeID(t *testing.T, scenario string) string {
	t.Helper()
	testName := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("%s-%s-%d", testName, scenario, time.Now().UnixNano())
}
