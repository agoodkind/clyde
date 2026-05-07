package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSupervisorWorkerCommandUsesHiddenWorkerRoleAndReadyFD(t *testing.T) {
	readyFilePath := filepath.Join(t.TempDir(), "ready")
	readyFile, err := os.Create(readyFilePath)
	if err != nil {
		t.Fatalf("create ready file: %v", err)
	}
	defer readyFile.Close()

	cmd := supervisorWorkerCommand("/tmp/clyde", readyFile, 3, "/tmp/clyde-supervisor.sock")

	if strings.Join(cmd.Args, " ") != "/tmp/clyde daemon worker" {
		t.Fatalf("args = %q, want hidden daemon worker role", strings.Join(cmd.Args, " "))
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != readyFile {
		t.Fatalf("extra files = %v, want readiness writer", cmd.ExtraFiles)
	}
	if !slices.Contains(cmd.Env, envDaemonReadyFD+"=3") {
		t.Fatalf("env does not include %s=3", envDaemonReadyFD)
	}
	if !slices.Contains(cmd.Env, envDaemonSupervisorSocket+"=/tmp/clyde-supervisor.sock") {
		t.Fatalf("env does not include %s", envDaemonSupervisorSocket)
	}
}

func TestWaitForSupervisorWorkerReadyAcceptsReadyLine(t *testing.T) {
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

	err = waitForSupervisorWorkerReady(context.Background(), readyRead, waitCh)
	if err != nil {
		t.Fatalf("wait ready: %v", err)
	}
}

func TestWaitForSupervisorWorkerReadyFailsWhenWorkerExitsFirst(t *testing.T) {
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create readiness pipe: %v", err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()

	waitCh := make(chan error, 1)
	waitCh <- errors.New("boom")

	err = waitForSupervisorWorkerReady(context.Background(), readyRead, waitCh)
	if err == nil || !strings.Contains(err.Error(), "exited before readiness") {
		t.Fatalf("wait ready error = %v, want worker exit before readiness", err)
	}
}

func TestLoadInheritedRuntimeAcceptsReadyFDWithoutListeners(t *testing.T) {
	readyFilePath := filepath.Join(t.TempDir(), "ready")
	readyFile, err := os.Create(readyFilePath)
	if err != nil {
		t.Fatalf("create ready file: %v", err)
	}
	defer readyFile.Close()

	t.Setenv(envDaemonInheritedListeners, "")
	t.Setenv(envDaemonReadyFD, "3")

	oldNewFile := osNewFile
	t.Cleanup(func() {
		osNewFile = oldNewFile
	})
	osNewFile = func(fd uintptr, name string) *os.File {
		if fd != 3 {
			t.Fatalf("fd = %d, want 3", fd)
		}
		if name != "daemon-ready" {
			t.Fatalf("name = %q, want daemon-ready", name)
		}
		return readyFile
	}

	inherited, err := loadInheritedRuntime()
	if err != nil {
		t.Fatalf("load inherited runtime: %v", err)
	}
	if inherited.ready != readyFile {
		t.Fatalf("ready file = %v, want injected file", inherited.ready)
	}
}

func TestReadSupervisorReloadRequestReceivesUnixRights(t *testing.T) {
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
		t.Fatalf("listen supervisor socket: %v", err)
	}
	defer listener.Close()

	serverResult := make(chan supervisorReloadRequest, 1)
	serverFileCount := make(chan int, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		req, files, err := readSupervisorReloadRequest(conn)
		if err != nil {
			serverErr <- err
			return
		}
		defer closeFiles(files)
		serverResult <- req
		serverFileCount <- len(files)
		serverErr <- nil
	}()

	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatalf("create fd pipe: %v", err)
	}
	defer readFile.Close()
	defer writeFile.Close()

	_, err = requestSupervisorReplacement(context.Background(), socketPath, supervisorReloadRequest{
		Operation:      supervisorReloadOperation,
		ExecutablePath: "/bin/clyde",
		Arguments:      []string{"/bin/clyde", "daemon", "worker"},
		Environment:    []string{envDaemonReloadChild + "=1"},
		Listeners: []inheritedListenerSpec{
			{Name: listenerNameDaemon, Network: "unix", Addr: "/tmp/clyde.sock", FD: 3},
		},
		ReadyFD: 4,
	}, []*os.File{readFile, writeFile})
	if err == nil || !strings.Contains(err.Error(), "decode supervisor reload response") {
		t.Fatalf("request error = %v, want response decode error after server read", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server read request: %v", err)
	}
	req := <-serverResult
	if strings.Join(req.Arguments, " ") != "/bin/clyde daemon worker" {
		t.Fatalf("arguments = %#v, want daemon worker", req.Arguments)
	}
	if count := <-serverFileCount; count != 2 {
		t.Fatalf("received file count = %d, want 2", count)
	}
}
