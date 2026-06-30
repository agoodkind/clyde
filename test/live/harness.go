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
	"net/http"
	"net/http/httptest"
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
// daemon: adapter 11434 to 21434, cursor ingress 11435 to 21435, MITM 487xx to
// 587xx. pprof is left off.
type fakePorts struct {
	MITMPort     int // 58723 (production cli.claude-code is 48723)
	AdapterPort  int // 21434 (production 11434)
	CursorPort   int // 21435 (production 11435)
	TopologyPort int // 21436, the moved-to adapter port for the topology test
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
		cfg:         fakePorts{MITMPort: fakeMITMPort, AdapterPort: fakeAdapterPort, CursorPort: fakeCursorPort, TopologyPort: fakeTopologyPort},
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
	for _, port := range []int{h.cfg.MITMPort, h.cfg.AdapterPort, h.cfg.CursorPort, h.cfg.TopologyPort} {
		if portListening(port) {
			return fmt.Errorf("preflight: fake port %d already listening; refusing to run", port)
		}
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
	caDir := filepath.Join(h.stateRoot, "ca")
	captureDir := filepath.Join(h.stateRoot, "mitm")
	for _, dir := range []string{caDir, captureDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	extra := ""
	for _, name := range extraModels {
		extra += fmt.Sprintf("\n[adapter.models.%s]\nbackend = \"passthrough_override\"\npassthrough_override = \"local\"\ncontext = 8000\nefforts = [\"medium\"]\n", name)
	}
	requireToken := ""
	if h.requireToken != "" {
		requireToken = fmt.Sprintf("\nrequire_token = %q", h.requireToken)
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
default_model = "local-test"%s

[adapter.openai_compat_passthrough]
base_url = %q

[adapter.passthrough_overrides.local]
base_url = %q

[adapter.models.local-test]
backend = "passthrough_override"
passthrough_override = "local"
context = 8000
efforts = ["medium"]

[adapter.client_identity]
beta_header = "test-beta"
user_agent = "clyde-live-test/0.0"
system_prompt_prefix = "test-prefix"
stainless_package_version = "0.0.0"
stainless_runtime = "node"
stainless_runtime_version = "v0.0.0"
cc_version = "0.0.0"
cc_entrypoint = "test"

[adapter.families.testfam]
model = "claude-test"
supports_tools = true
supports_vision = false
efforts = ["medium"]
thinking_modes = ["disabled"]
max_output_tokens = 8000
contexts = [{ tokens = 8000, alias_suffix = "", wire_suffix = "" }]
%s
[mitm]
enabled_default = false
`, adapterPort, h.cfg.CursorPort, requireToken, passthroughURL, passthroughURL, extra)
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

var _ = context.Background
