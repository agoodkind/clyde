package daemonsupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWorkerCommandUsesHiddenWorkerRoleAndReadyFD(t *testing.T) {
	t.Setenv(EnvReloadChild, "1")
	t.Setenv(EnvInheritedListeners, "stale-listeners")

	readyFilePath := filepath.Join(t.TempDir(), "ready")
	readyFile, err := os.Create(readyFilePath)
	if err != nil {
		t.Fatalf("create ready file: %v", err)
	}
	defer readyFile.Close()

	cmd := workerCommand("/tmp/clyde", readyFile, 3, "/tmp/clyde-supervisor.sock")

	if strings.Join(cmd.Args, " ") != "/tmp/clyde daemon worker" {
		t.Fatalf("args = %q, want hidden daemon worker role", strings.Join(cmd.Args, " "))
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != readyFile {
		t.Fatalf("extra files = %v, want readiness writer", cmd.ExtraFiles)
	}
	if !slices.Contains(cmd.Env, EnvReadyFD+"=3") {
		t.Fatalf("env does not include %s=3", EnvReadyFD)
	}
	if !slices.Contains(cmd.Env, EnvSupervisorSocket+"=/tmp/clyde-supervisor.sock") {
		t.Fatalf("env does not include %s", EnvSupervisorSocket)
	}
	if slices.Contains(cmd.Env, EnvReloadChild+"=1") {
		t.Fatalf("env still includes stale reload child marker")
	}
	if slices.Contains(cmd.Env, EnvInheritedListeners+"=stale-listeners") {
		t.Fatalf("env still includes stale inherited listeners")
	}
}

func TestWaitForWorkerReadyAcceptsReadyLine(t *testing.T) {
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create readiness pipe: %v", err)
	}
	defer readyRead.Close()

	waitCh := make(chan error, 1)
	if _, err := readyWrite.WriteString("ready\n"); err != nil {
		t.Fatalf("write readiness: %v", err)
	}
	if err := readyWrite.Close(); err != nil {
		t.Fatalf("close readiness writer: %v", err)
	}

	err = waitForWorkerReady(context.Background(), readyRead, waitCh)
	if err != nil {
		t.Fatalf("wait ready: %v", err)
	}
}

func TestWaitForWorkerReadyFailsWhenWorkerExitsFirst(t *testing.T) {
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create readiness pipe: %v", err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()

	waitCh := make(chan error, 1)
	waitCh <- errors.New("boom")

	err = waitForWorkerReady(context.Background(), readyRead, waitCh)
	if err == nil || !strings.Contains(err.Error(), "exited before readiness") {
		t.Fatalf("wait ready error = %v, want worker exit before readiness", err)
	}
}

func TestRequestReplacementSendsRequestAndFiles(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "clyde-supervisor-*") //nolint:usetesting
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(socketDir)
	})
	socketPath := filepath.Join(socketDir, "supervisor.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen supervisor: %v", err)
	}
	defer listener.Close()

	received := make(chan controlRequest, 1)
	receivedFileCount := make(chan int, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()

		req, files, readErr := readControlRequest(conn)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		defer closeFiles(files)
		received <- req
		receivedFileCount <- len(files)
		_, writeErr := conn.Write([]byte(`{"pid":9876}` + "\n"))
		serverErr <- writeErr
	}()

	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer readFile.Close()
	defer writeFile.Close()

	pid, err := RequestReplacement(
		context.Background(),
		socketPath,
		"/bin/clyde",
		[]ListenerSpec{{Name: "daemon", Network: "unix", Addr: "/tmp/clyde.sock", FD: 3}},
		4,
		[]string{EnvReloadChild + "=stale", EnvReadyFD + "=3", EnvInheritedListeners + "=old"},
		[]*os.File{readFile, writeFile},
	)
	if err != nil {
		t.Fatalf("request supervisor replacement: %v", err)
	}
	if pid != 9876 {
		t.Fatalf("pid = %d, want 9876", pid)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("supervisor server: %v", err)
	}
	req := <-received
	if req.ExecutablePath != "/bin/clyde" {
		t.Fatalf("executable path = %q, want /bin/clyde", req.ExecutablePath)
	}
	if req.Operation != controlOperationReplace {
		t.Fatalf("operation = %q, want %q", req.Operation, controlOperationReplace)
	}
	if !envContains(req.Environment, EnvReloadChild+"=1") {
		t.Fatalf("environment does not include reload child marker")
	}
	if !envContains(req.Environment, EnvReadyFD+"=4") {
		t.Fatalf("environment does not include ready fd")
	}
	if !envContainsPrefix(req.Environment, EnvInheritedListeners+"=") {
		t.Fatalf("environment does not include inherited listener specs")
	}
	if envContains(req.Environment, EnvReadyFD+"=3") {
		t.Fatalf("environment still includes stale ready fd")
	}
	if count := <-receivedFileCount; count != 2 {
		t.Fatalf("received file count = %d, want 2", count)
	}
}

func TestRequestReplacementSendsLargeEnvironment(t *testing.T) {
	socketDir := filepath.Join("/tmp", fmt.Sprintf("clyde-supervisor-large-%d", os.Getpid()))
	// Clear any directory a previous interrupted run left behind so a stale
	// supervisor.sock cannot fail ListenUnix with "address already in use".
	if err := os.RemoveAll(socketDir); err != nil {
		t.Fatalf("clear stale socket dir: %v", err)
	}
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(socketDir)
	})
	socketPath := filepath.Join(socketDir, "supervisor.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen supervisor: %v", err)
	}
	defer listener.Close()

	received := make(chan controlRequest, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()

		req, files, readErr := readControlRequest(conn)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		defer closeFiles(files)
		received <- req
		_, writeErr := conn.Write([]byte(`{"pid":9876}` + "\n"))
		serverErr <- writeErr
	}()

	// A single large environment value pushes the request body past the roughly
	// 8 KiB stream-socket send buffer that used to short-write the reload.
	largeValue := strings.Repeat("x", 32*1024)
	environment := []string{
		EnvReloadChild + "=stale",
		EnvReadyFD + "=3",
		"CLYDE_LARGE_ENV=" + largeValue,
	}
	pid, err := RequestReplacement(
		context.Background(),
		socketPath,
		"/bin/clyde",
		[]ListenerSpec{{Name: "daemon", Network: "unix", Addr: "/tmp/clyde.sock", FD: 3}},
		4,
		environment,
		nil,
	)
	if err != nil {
		t.Fatalf("request supervisor replacement with large environment: %v", err)
	}
	if pid != 9876 {
		t.Fatalf("pid = %d, want 9876", pid)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("supervisor server: %v", err)
	}
	req := <-received
	if !envContains(req.Environment, "CLYDE_LARGE_ENV="+largeValue) {
		t.Fatalf("large environment value did not survive the framed round-trip")
	}
}

func TestRequestStatusReturnsFingerprint(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "clyde-supervisor-status-*") //nolint:usetesting
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(socketDir)
	})
	socketPath := filepath.Join(socketDir, "supervisor.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen supervisor: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()

		req, files, readErr := readControlRequest(conn)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		defer closeFiles(files)
		if req.Operation != controlOperationStatus {
			serverErr <- errors.New("wrong operation")
			return
		}
		_, writeErr := conn.Write([]byte(`{"pid":4321,"fingerprint":"new-fingerprint"}` + "\n"))
		serverErr <- writeErr
	}()

	status, err := RequestStatus(context.Background(), socketPath)
	if err != nil {
		t.Fatalf("request status: %v", err)
	}
	if status.PID != 4321 {
		t.Fatalf("pid = %d, want 4321", status.PID)
	}
	if status.Fingerprint != "new-fingerprint" {
		t.Fatalf("fingerprint = %q, want new-fingerprint", status.Fingerprint)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("supervisor server: %v", err)
	}
}

func TestReplacementWorkerEnvironmentOverridesFDHandoff(t *testing.T) {
	req := controlRequest{
		Environment: []string{
			"KEEP=value",
			EnvReadyFD + "=3",
			EnvInheritedListeners + "=stale",
			EnvSupervisorSocket + "=/tmp/old.sock",
		},
		Listeners: []ListenerSpec{
			{Name: "daemon", Network: "unix", Addr: "/tmp/clyde.sock", FD: 3},
		},
		ReadyFD: 4,
	}

	env, err := replacementWorkerEnvironment(req, "/tmp/new.sock")
	if err != nil {
		t.Fatalf("supervisor reload worker environment: %v", err)
	}
	if !envContains(env, "KEEP=value") {
		t.Fatalf("environment dropped unrelated variable")
	}
	if !envContains(env, EnvReadyFD+"=4") {
		t.Fatalf("environment does not include new ready fd")
	}
	if !envContains(env, EnvSupervisorSocket+"=/tmp/new.sock") {
		t.Fatalf("environment does not include new supervisor socket")
	}
	if envContains(env, EnvReadyFD+"=3") {
		t.Fatalf("environment still includes stale ready fd")
	}
	if envContains(env, EnvInheritedListeners+"=stale") {
		t.Fatalf("environment still includes stale listener specs")
	}
	if envContains(env, EnvSupervisorSocket+"=/tmp/old.sock") {
		t.Fatalf("environment still includes stale supervisor socket")
	}
}

func TestCompiledFingerprintDefaultsWhenEmpty(t *testing.T) {
	previous := BuildFingerprint
	BuildFingerprint = ""
	t.Cleanup(func() {
		BuildFingerprint = previous
	})
	if got := CompiledFingerprint(); got != buildFingerprintDefault {
		t.Fatalf("compiled fingerprint = %q, want %q", got, buildFingerprintDefault)
	}
}

// TestSupervisorStopsAbortedAndSupersededWorkers runs a supervisor with real
// worker processes and sends the control requests over its socket. An abort
// stops the replacement and the first worker stays current. A supervisor stop
// then ends the current worker and the worker it superseded.
func TestSupervisorStopsAbortedAndSupersededWorkers(t *testing.T) {
	// A unix socket path under t.TempDir exceeds the macOS path limit.
	runtimeDir := filepath.Join("/tmp", fmt.Sprintf("clyde-supervisor-abort-%d", os.Getpid()))
	if err := os.RemoveAll(runtimeDir); err != nil {
		t.Fatalf("clear stale runtime dir: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })

	pidFilePath := filepath.Join(runtimeDir, "worker-pids")
	t.Setenv(testWorkerPIDFileEnv, pidFilePath)
	workerPath, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	alive := func(pid int) bool { return syscall.Kill(pid, 0) == nil }
	waitFor := func(condition func() bool, failure string) {
		t.Helper()
		for range supervisorTestPollAttempts {
			if condition() {
				return
			}
			time.Sleep(supervisorTestPollInterval)
		}
		t.Fatal(failure)
	}
	requestReplacement := func(ctx context.Context) int {
		t.Helper()
		readyRead, readyWrite, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatalf("create readiness pipe: %v", pipeErr)
		}
		defer readyRead.Close()
		defer readyWrite.Close()
		pid, requestErr := RequestReplacement(ctx, SocketPath(runtimeDir), workerPath, nil, 3, os.Environ(), []*os.File{readyWrite})
		if requestErr != nil {
			t.Fatalf("request replacement: %v", requestErr)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		return pid
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisorDone := make(chan error, 1)
	go func() {
		supervisorDone <- SuperviseContext(ctx, slog.Default(), runtimeDir)
	}()
	waitFor(func() bool {
		_, statusErr := RequestStatus(ctx, SocketPath(runtimeDir))
		return statusErr == nil
	}, "supervisor did not answer a status request")
	pidFile, err := os.ReadFile(pidFilePath)
	if err != nil {
		t.Fatalf("read worker pid file: %v", err)
	}
	firstPID, err := strconv.Atoi(strings.Fields(string(pidFile))[0])
	if err != nil {
		t.Fatalf("parse first worker pid: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(firstPID, syscall.SIGKILL) })

	abortedPID := requestReplacement(ctx)
	if err := RequestAbort(ctx, SocketPath(runtimeDir), abortedPID); err != nil {
		t.Fatalf("request abort: %v", err)
	}
	waitFor(func() bool { return !alive(abortedPID) }, "aborted replacement worker is still alive")
	if !alive(firstPID) {
		t.Fatal("first worker stopped after its replacement was aborted")
	}

	secondPID := requestReplacement(ctx)
	cancel()
	if err := <-supervisorDone; err != nil {
		t.Fatalf("supervisor: %v", err)
	}
	if alive(secondPID) {
		t.Fatal("current worker is still alive after the supervisor stopped")
	}
	if alive(firstPID) {
		t.Fatal("superseded worker is still alive after the supervisor stopped")
	}
}

const (
	supervisorTestPollAttempts = 400
	supervisorTestPollInterval = 25 * time.Millisecond
	testWorkerPIDFileEnv       = "CLYDE_TEST_WORKER_PID_FILE"
	testWorkerLifetime         = 2 * time.Minute
)

// TestMain runs the test binary as a daemon worker when the supervisor starts
// it with the worker arguments. The worker appends its pid to the file named by
// testWorkerPIDFileEnv, writes the ready line to its ready descriptor, and
// sleeps until the supervisor stops it.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "daemon" && os.Args[2] == "worker" {
		runTestWorker()
		return
	}
	os.Exit(m.Run())
}

func runTestWorker() {
	pidFile, err := os.OpenFile(os.Getenv(testWorkerPIDFileEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(pidFile, os.Getpid())
	_ = pidFile.Close()
	readyFD, err := strconv.Atoi(os.Getenv(EnvReadyFD))
	if err != nil {
		os.Exit(1)
	}
	ready := os.NewFile(uintptr(readyFD), "ready")
	_, _ = ready.WriteString("ready\n")
	_ = ready.Close()
	time.Sleep(testWorkerLifetime)
}

func envContains(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func envContainsPrefix(env []string, prefix string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func countUnixRights(oob []byte) (int, error) {
	messages, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, message := range messages {
		fds, rightsErr := syscall.ParseUnixRights(&message)
		if rightsErr != nil {
			if errors.Is(rightsErr, syscall.EINVAL) {
				continue
			}
			return 0, rightsErr
		}
		for _, fd := range fds {
			total++
			_ = syscall.Close(fd)
		}
	}
	return total, nil
}
