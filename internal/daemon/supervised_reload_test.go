package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReplacementDaemonStarterForPlatformUsesSupervisorOnDarwinWithSocket(t *testing.T) {
	starter := replacementDaemonStarterForPlatform("darwin", "/tmp/clyde-supervisor.sock")
	if _, ok := starter.(supervisorReplacementDaemonStarter); !ok {
		t.Fatalf("starter type = %T, want supervisorReplacementDaemonStarter", starter)
	}
}

func TestReplacementDaemonStarterForPlatformFallsBackToDirectWithoutSocket(t *testing.T) {
	starter := replacementDaemonStarterForPlatform("darwin", "")
	if _, ok := starter.(directReplacementDaemonStarter); !ok {
		t.Fatalf("starter type = %T, want directReplacementDaemonStarter", starter)
	}
}

func TestSupervisorReplacementRequestPreservesReloadEnvironment(t *testing.T) {
	req, err := supervisorReplacementRequest(replacementDaemonRequest{
		executablePath: "/bin/clyde",
		specs: []inheritedListenerSpec{
			{Name: listenerNameDaemon, Network: "unix", Addr: "/tmp/clyde.sock", FD: 3},
		},
		readyFD: 4,
	})
	if err != nil {
		t.Fatalf("supervisor replacement request: %v", err)
	}
	if strings.Join(req.Arguments, " ") != "/bin/clyde daemon worker" {
		t.Fatalf("arguments = %#v, want executable daemon worker", req.Arguments)
	}
	if req.Operation != supervisorReloadOperation {
		t.Fatalf("operation = %q, want %q", req.Operation, supervisorReloadOperation)
	}
	if req.ReadyFD != 4 {
		t.Fatalf("ready fd = %d, want 4", req.ReadyFD)
	}
	if !envContains(req.Environment, envDaemonReloadChild+"=1") {
		t.Fatalf("environment does not include reload child marker")
	}
	if !envContainsPrefix(req.Environment, envDaemonInheritedListeners+"=") {
		t.Fatalf("environment does not include inherited listener specs")
	}
	if !envContains(req.Environment, envDaemonReadyFD+"=4") {
		t.Fatalf("environment does not include ready fd")
	}
}

func TestRequestSupervisorReplacementSendsRequestAndFiles(t *testing.T) {
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

	received := make(chan supervisorReloadRequest, 1)
	receivedFileCount := make(chan int, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()

		buffer := make([]byte, 4096)
		oob := make([]byte, syscall.CmsgSpace(4*4))
		n, oobn, _, _, readErr := conn.ReadMsgUnix(buffer, oob)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		var req supervisorReloadRequest
		if decodeErr := json.Unmarshal(buffer[:n], &req); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		fileCount, parseErr := countUnixRights(oob[:oobn])
		if parseErr != nil {
			serverErr <- parseErr
			return
		}
		received <- req
		receivedFileCount <- fileCount
		_, writeErr := conn.Write([]byte(`{"pid":9876}` + "\n"))
		serverErr <- writeErr
	}()

	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer readFile.Close()
	defer writeFile.Close()

	resp, err := requestSupervisorReplacement(context.Background(), socketPath, supervisorReloadRequest{
		Operation:      supervisorReloadOperation,
		ExecutablePath: "/bin/clyde",
		Arguments:      []string{"/bin/clyde", "daemon", "worker"},
		Environment:    []string{envDaemonReloadChild + "=1"},
		Listeners: []inheritedListenerSpec{
			{Name: listenerNameDaemon, Network: "unix", Addr: "/tmp/clyde.sock", FD: 3},
		},
		ReadyFD: 4,
	}, []*os.File{readFile, writeFile})
	if err != nil {
		t.Fatalf("request supervisor replacement: %v", err)
	}
	if resp.PID != 9876 {
		t.Fatalf("pid = %d, want 9876", resp.PID)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("supervisor server: %v", err)
	}
	req := <-received
	if req.ExecutablePath != "/bin/clyde" {
		t.Fatalf("executable path = %q, want /bin/clyde", req.ExecutablePath)
	}
	if req.Operation != supervisorReloadOperation {
		t.Fatalf("operation = %q, want %q", req.Operation, supervisorReloadOperation)
	}
	if count := <-receivedFileCount; count != 2 {
		t.Fatalf("received file count = %d, want 2", count)
	}
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
