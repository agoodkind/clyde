package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	supervisorReloadOperation      = "replace_daemon_worker"
	supervisorReloadRequestTimeout = 5 * time.Second
)

type supervisorReplacementDaemonStarter struct {
	socketPath string
}

type supervisorReloadRequest struct {
	Operation      string                  `json:"operation"`
	ExecutablePath string                  `json:"executable_path"`
	Arguments      []string                `json:"arguments"`
	Environment    []string                `json:"environment"`
	Listeners      []inheritedListenerSpec `json:"listeners"`
	ReadyFD        int                     `json:"ready_fd"`
}

type supervisorReloadResponse struct {
	PID   int    `json:"pid"`
	Error string `json:"error,omitempty"`
}

func (s supervisorReplacementDaemonStarter) startReplacementDaemon(ctx context.Context, log *slog.Logger, req replacementDaemonRequest) (*replacementDaemonProcess, error) {
	socketPath := strings.TrimSpace(s.socketPath)
	if socketPath == "" {
		return nil, fmt.Errorf("supervisor reload socket is empty")
	}
	files := append([]*os.File{}, req.files...)
	files = append(files, req.readyWrite)
	controlReq, err := supervisorReplacementRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := requestSupervisorReplacement(ctx, socketPath, controlReq, files)
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.supervisor_request_failed",
			"component", "daemon",
			"socket_path", socketPath,
			"path", req.executablePath,
			"err", err,
		)
		return nil, fmt.Errorf("request supervisor replacement daemon: %w", err)
	}
	log.InfoContext(ctx, "daemon.reload.supervisor_replacement_started",
		"component", "daemon",
		"socket_path", socketPath,
		"new_pid", resp.PID,
	)
	return &replacementDaemonProcess{
		pid:  resp.PID,
		wait: func() error { return nil },
		kill: func() error { return nil },
	}, nil
}

func supervisorReplacementRequest(req replacementDaemonRequest) (supervisorReloadRequest, error) {
	specJSON, err := json.Marshal(req.specs)
	if err != nil {
		return supervisorReloadRequest{}, fmt.Errorf("encode inherited listeners: %w", err)
	}
	return supervisorReloadRequest{
		Operation:      supervisorReloadOperation,
		ExecutablePath: req.executablePath,
		Arguments:      []string{req.executablePath, "daemon", "worker"},
		Environment: append(os.Environ(),
			envDaemonReloadChild+"=1",
			envDaemonInheritedListeners+"="+string(specJSON),
			envDaemonReadyFD+"="+strconv.Itoa(req.readyFD),
		),
		Listeners: req.specs,
		ReadyFD:   req.readyFD,
	}, nil
}

func requestSupervisorReplacement(ctx context.Context, socketPath string, req supervisorReloadRequest, files []*os.File) (supervisorReloadResponse, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, supervisorReloadRequestTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(deadlineCtx, "unix", socketPath)
	if err != nil {
		return supervisorReloadResponse{}, fmt.Errorf("connect supervisor: %w", err)
	}
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return supervisorReloadResponse{}, fmt.Errorf("supervisor connection has type %T, want *net.UnixConn", conn)
	}
	if deadline, ok := deadlineCtx.Deadline(); ok {
		_ = unixConn.SetDeadline(deadline)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return supervisorReloadResponse{}, fmt.Errorf("encode supervisor reload request: %w", err)
	}
	payload = append(payload, '\n')
	rights := syscall.UnixRights(fileDescriptors(files)...)
	if _, _, err := unixConn.WriteMsgUnix(payload, rights, nil); err != nil {
		return supervisorReloadResponse{}, fmt.Errorf("send supervisor reload request: %w", err)
	}

	var resp supervisorReloadResponse
	if err := json.NewDecoder(bufio.NewReader(unixConn)).Decode(&resp); err != nil {
		return supervisorReloadResponse{}, fmt.Errorf("decode supervisor reload response: %w", err)
	}
	if resp.Error != "" {
		return supervisorReloadResponse{}, fmt.Errorf("supervisor reload rejected: %s", resp.Error)
	}
	if resp.PID <= 0 {
		return supervisorReloadResponse{}, fmt.Errorf("supervisor reload returned invalid pid %d", resp.PID)
	}
	return resp, nil
}

func fileDescriptors(files []*os.File) []int {
	fds := make([]int, 0, len(files))
	maxInt := uintptr(^uint(0) >> 1)
	for _, file := range files {
		if file == nil {
			continue
		}
		fd := file.Fd()
		if fd > maxInt {
			continue
		}
		fds = append(fds, int(fd))
	}
	return fds
}
