package daemonsupervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// EnvReloadChild marks a daemon worker that was spawned during reload.
	EnvReloadChild = "CLYDE_DAEMON_RELOAD_CHILD"
	// EnvInheritedListeners carries the inherited listener table for a worker.
	EnvInheritedListeners = "CLYDE_DAEMON_INHERITED_LISTENERS"
	// EnvReadyFD carries the readiness pipe file descriptor number for a worker.
	EnvReadyFD = "CLYDE_DAEMON_READY_FD"
	// EnvSupervisorSocket carries the supervisor control socket path for a worker.
	EnvSupervisorSocket = "CLYDE_DAEMON_SUPERVISOR_SOCKET"
)

const (
	buildFingerprintDefault = "development"
	controlOperationStatus  = "status"
	controlOperationReplace = "replace_daemon_worker"
	requestTimeout          = 5 * time.Second
	workerReadyTimeout      = 5 * time.Second
	workerStopTimeout       = 5 * time.Second
)

var (
	// BuildFingerprint is stamped at build time from the supervisor package.
	BuildFingerprint           = buildFingerprintDefault
	supervisorExecutablePath   = os.Executable
	supervisorCommand          = exec.Command
	errWorkerExitedBeforeReady = errors.New("daemon worker exited before readiness")
)

// ListenerSpec describes one inherited listener passed to a replacement worker.
type ListenerSpec struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Addr    string `json:"addr"`
	FD      int    `json:"fd"`
}

// Status reports the supervisor identity returned over the control socket.
type Status struct {
	Fingerprint string
	PID         int
}

type controlRequest struct {
	Operation      string         `json:"operation"`
	ExecutablePath string         `json:"executable_path,omitempty"`
	Arguments      []string       `json:"arguments,omitempty"`
	Environment    []string       `json:"environment,omitempty"`
	Listeners      []ListenerSpec `json:"listeners,omitempty"`
	ReadyFD        int            `json:"ready_fd,omitempty"`
}

type controlResponse struct {
	PID         int    `json:"pid,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Error       string `json:"error,omitempty"`
}

type workerHandle struct {
	cmd    *exec.Cmd
	waitCh <-chan error
}

// CompiledFingerprint returns the build-stamped supervisor fingerprint.
func CompiledFingerprint() string {
	fingerprint := strings.TrimSpace(BuildFingerprint)
	if fingerprint == "" {
		return buildFingerprintDefault
	}
	return fingerprint
}

// SocketPath returns the supervisor control socket path for one runtime dir.
func SocketPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "daemon.supervisor.sock")
}

// Supervise runs the launchd-owned supervisor loop for Clyde's daemon worker.
func Supervise(log *slog.Logger, runtimeDir string) error {
	if strings.TrimSpace(runtimeDir) == "" {
		return fmt.Errorf("daemon supervisor runtime dir is empty")
	}
	executablePath, err := supervisorExecutablePath()
	if err != nil {
		log.Warn("daemon.supervisor.executable_failed", "component", "daemon", "err", err)
		return fmt.Errorf("resolve daemon supervisor executable: %w", err)
	}

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		log.Warn("daemon.supervisor.ready_pipe_failed", "component", "daemon", "err", err)
		return fmt.Errorf("create daemon worker readiness pipe: %w", err)
	}
	defer func() { _ = readyRead.Close() }()
	defer func() { _ = readyWrite.Close() }()

	socketPath := SocketPath(runtimeDir)
	_ = os.Remove(socketPath)
	controlListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		log.Warn("daemon.supervisor.reload_socket_failed", "component", "daemon", "socket_path", socketPath, "err", err)
		return fmt.Errorf("listen daemon supervisor reload socket %s: %w", socketPath, err)
	}
	defer func() { _ = controlListener.Close() }()
	defer func() { _ = os.Remove(socketPath) }()

	handle, err := startWorker(workerCommand(executablePath, readyWrite, 3, socketPath))
	if err != nil {
		log.Warn("daemon.supervisor.worker_start_failed", "component", "daemon", "err", err)
		return fmt.Errorf("start daemon worker: %w", err)
	}
	_ = readyWrite.Close()

	signalCh := make(chan os.Signal, 2)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signalCh)

	log.Info("daemon.supervisor.worker_started",
		"component", "daemon",
		"pid", handle.cmd.Process.Pid,
		"socket_path", socketPath,
		"fingerprint", CompiledFingerprint(),
	)

	readyCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logSupervisorPanic(log, "daemon.supervisor.worker_ready_wait", fmt.Sprint(recovered))
			}
		}()
		readyCh <- waitForWorkerReady(context.Background(), readyRead, handle.waitCh)
	}()

	select {
	case sig := <-signalCh:
		log.Info("daemon.supervisor.signal_received",
			"component", "daemon",
			"signal", fmt.Sprint(sig),
			"pid", handle.cmd.Process.Pid,
		)
		stopWorker(log, handle.cmd, sig, handle.waitCh)
		return nil
	case err := <-readyCh:
		if err != nil {
			if !errors.Is(err, errWorkerExitedBeforeReady) {
				stopWorker(log, handle.cmd, syscall.SIGTERM, handle.waitCh)
			}
			return err
		}
	}
	log.Info("daemon.supervisor.worker_ready",
		"component", "daemon",
		"pid", handle.cmd.Process.Pid,
	)

	replacementCh := make(chan workerHandle, 1)
	controlErrCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logSupervisorPanic(log, "daemon.supervisor.reload_control", fmt.Sprint(recovered))
			}
		}()
		serveControl(log, controlListener, replacementCh, controlErrCh)
	}()

	return runSupervisorLoop(log, signalCh, handle, replacementCh, controlErrCh)
}

// RequestReplacement asks the running supervisor to replace its worker process.
func RequestReplacement(
	ctx context.Context,
	socketPath string,
	executablePath string,
	listeners []ListenerSpec,
	readyFD int,
	environment []string,
	files []*os.File,
) (int, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return 0, fmt.Errorf("supervisor reload socket is empty")
	}
	specJSON, err := json.Marshal(listeners)
	if err != nil {
		slog.WarnContext(ctx, "daemon.supervisor.reload_request.encode_listeners_failed", "err", err)
		return 0, fmt.Errorf("encode inherited listeners: %w", err)
	}
	if environment == nil {
		environment = os.Environ()
	}
	req := controlRequest{
		Operation:      controlOperationReplace,
		ExecutablePath: executablePath,
		Arguments:      []string{executablePath, "daemon", "worker"},
		Environment: EnvWithOverrides(environment,
			EnvReloadChild+"=1",
			EnvInheritedListeners+"="+string(specJSON),
			EnvReadyFD+"="+strconv.Itoa(readyFD),
		),
		Listeners: listeners,
		ReadyFD:   readyFD,
	}
	resp, err := sendControlRequest(ctx, socketPath, req, files)
	if err != nil {
		return 0, err
	}
	if resp.PID <= 0 {
		return 0, fmt.Errorf("supervisor reload returned invalid pid %d", resp.PID)
	}
	return resp.PID, nil
}

// RequestStatus asks the running supervisor for its fingerprint and pid.
func RequestStatus(ctx context.Context, socketPath string) (Status, error) {
	resp, err := sendControlRequest(ctx, socketPath, controlRequest{
		Operation:      controlOperationStatus,
		ExecutablePath: "",
		Arguments:      nil,
		Environment:    nil,
		Listeners:      nil,
		ReadyFD:        0,
	}, nil)
	if err != nil {
		return Status{}, err
	}
	return Status{
		Fingerprint: strings.TrimSpace(resp.Fingerprint),
		PID:         resp.PID,
	}, nil
}

// EnvWithOverrides removes prior values for overridden keys and appends overrides.
func EnvWithOverrides(base []string, overrides ...string) []string {
	remove := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		key, ok := envKey(override)
		if !ok {
			continue
		}
		remove[key] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, ok := envKey(entry)
		if !ok {
			out = append(out, entry)
			continue
		}
		if remove[key] {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, overrides...)
	return out
}

func runSupervisorLoop(log *slog.Logger, signalCh <-chan os.Signal, handle workerHandle, replacementCh <-chan workerHandle, controlErrCh <-chan error) error {
	current := handle
	for {
		select {
		case sig := <-signalCh:
			log.Debug("daemon.supervisor.signal_received",
				"component", "daemon",
				"signal", fmt.Sprint(sig),
				"pid", current.cmd.Process.Pid,
			)
			stopWorker(log, current.cmd, sig, current.waitCh)
			return nil
		case replacement := <-replacementCh:
			current = replacement
			log.Debug("daemon.supervisor.worker_replaced",
				"component", "daemon",
				"pid", current.cmd.Process.Pid,
			)
		case err := <-current.waitCh:
			return workerExitError(err)
		case err := <-controlErrCh:
			return err
		}
	}
}

func workerCommand(executablePath string, readyWrite *os.File, readyFD int, supervisorSocketPath string) *exec.Cmd {
	cmd := supervisorCommand(executablePath, "daemon", "worker")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.ExtraFiles = []*os.File{readyWrite}
	cmd.Env = EnvWithOverrides(os.Environ(),
		EnvReloadChild+"=",
		EnvInheritedListeners+"=",
		EnvReadyFD+"="+strconv.Itoa(readyFD),
		EnvSupervisorSocket+"="+supervisorSocketPath,
	)
	return cmd
}

func startWorker(cmd *exec.Cmd) (workerHandle, error) {
	if err := cmd.Start(); err != nil {
		slog.Warn("daemon.supervisor.worker_start_failed", "err", err)
		return workerHandle{}, fmt.Errorf("start daemon worker: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logSupervisorPanic(slog.Default(), "daemon.supervisor.worker_wait", fmt.Sprint(recovered))
			}
		}()
		waitCh <- cmd.Wait()
	}()
	return workerHandle{cmd: cmd, waitCh: waitCh}, nil
}

func waitForWorkerReady(ctx context.Context, ready io.Reader, waitCh <-chan error) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, workerReadyTimeout)
	defer cancel()
	readyCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logSupervisorPanic(slog.Default(), "daemon.supervisor.ready_read", fmt.Sprint(recovered))
			}
		}()
		data, err := io.ReadAll(ready)
		if err != nil {
			readyCh <- err
			return
		}
		if string(data) != "ready\n" {
			readyCh <- fmt.Errorf("daemon worker readiness failed: %q", string(data))
			return
		}
		readyCh <- nil
	}()

	select {
	case err := <-readyCh:
		return err
	case err := <-waitCh:
		slog.WarnContext(ctx, "daemon.supervisor.worker_exited_before_ready", "err", err)
		return fmt.Errorf("%w: %w", errWorkerExitedBeforeReady, workerExitError(err))
	case <-deadlineCtx.Done():
		slog.WarnContext(ctx, "daemon.supervisor.worker_ready_timeout", "err", deadlineCtx.Err())
		return fmt.Errorf("daemon worker did not become ready: %w", deadlineCtx.Err())
	}
}

func stopWorker(log *slog.Logger, cmd *exec.Cmd, sig os.Signal, waitCh <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		log.Warn("daemon.supervisor.worker_signal_failed",
			"component", "daemon",
			"pid", cmd.Process.Pid,
			"signal", fmt.Sprint(sig),
			"err", err,
		)
	}
	select {
	case err := <-waitCh:
		if err != nil {
			log.Info("daemon.supervisor.worker_stopped",
				"component", "daemon",
				"pid", cmd.Process.Pid,
				"err", err,
			)
		}
	case <-time.After(workerStopTimeout):
		log.Warn("daemon.supervisor.worker_stop_timeout",
			"component", "daemon",
			"pid", cmd.Process.Pid,
		)
		_ = cmd.Process.Kill()
		<-waitCh
	}
}

func workerExitError(err error) error {
	if err == nil {
		return fmt.Errorf("daemon worker exited")
	}
	slog.Warn("daemon.supervisor.worker_exited", "err", err)
	return fmt.Errorf("daemon worker exited: %w", err)
}

func serveControl(log *slog.Logger, listener *net.UnixListener, replacementCh chan<- workerHandle, errCh chan<- error) {
	supervisorSocketPath := listener.Addr().String()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errCh <- fmt.Errorf("accept supervisor reload control: %w", err)
			return
		}
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					logSupervisorPanic(log, "daemon.supervisor.reload_request", fmt.Sprint(recovered))
				}
			}()
			handleControl(log, conn, replacementCh, supervisorSocketPath)
		}()
	}
}

func handleControl(log *slog.Logger, conn *net.UnixConn, replacementCh chan<- workerHandle, supervisorSocketPath string) {
	defer func() { _ = conn.Close() }()
	req, files, err := readControlRequest(conn)
	if err != nil {
		writeControlError(conn, err)
		return
	}
	defer closeFiles(files)

	switch req.Operation {
	case controlOperationStatus:
		resp := controlResponse{
			PID:         os.Getpid(),
			Fingerprint: CompiledFingerprint(),
			Error:       "",
		}
		if err := json.NewEncoder(conn).Encode(resp); err != nil {
			log.Warn("daemon.supervisor.status_response_failed",
				"component", "daemon",
				"err", err,
			)
		}
		return
	case controlOperationReplace:
	default:
		writeControlError(conn, fmt.Errorf("unsupported supervisor operation %q", req.Operation))
		return
	}

	if len(req.Arguments) < 1 {
		writeControlError(conn, fmt.Errorf("supervisor reload request missing arguments"))
		return
	}
	env, err := replacementWorkerEnvironment(req, supervisorSocketPath)
	if err != nil {
		writeControlError(conn, err)
		return
	}
	cmd := supervisorCommand(req.ExecutablePath, req.Arguments[1:]...)
	cmd.Args = append([]string{}, req.Arguments...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = env
	cmd.ExtraFiles = append([]*os.File{}, files...)
	handle, err := startWorker(cmd)
	if err != nil {
		writeControlError(conn, fmt.Errorf("start replacement daemon worker: %w", err))
		return
	}
	replacementCh <- handle
	log.Info("daemon.supervisor.reload_replacement_started",
		"component", "daemon",
		"pid", handle.cmd.Process.Pid,
	)
	resp := controlResponse{
		PID:         handle.cmd.Process.Pid,
		Fingerprint: "",
		Error:       "",
	}
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		log.Warn("daemon.supervisor.reload_response_failed",
			"component", "daemon",
			"pid", handle.cmd.Process.Pid,
			"err", err,
		)
	}
}

func replacementWorkerEnvironment(req controlRequest, supervisorSocketPath string) ([]string, error) {
	specJSON, err := json.Marshal(req.Listeners)
	if err != nil {
		slog.Warn("daemon.supervisor.reload_request.encode_listeners_failed", "err", err)
		return nil, fmt.Errorf("encode supervisor reload listener specs: %w", err)
	}
	return EnvWithOverrides(req.Environment,
		EnvReloadChild+"=1",
		EnvInheritedListeners+"="+string(specJSON),
		EnvReadyFD+"="+strconv.Itoa(req.ReadyFD),
		EnvSupervisorSocket+"="+supervisorSocketPath,
	), nil
}

func sendControlRequest(ctx context.Context, socketPath string, req controlRequest, files []*os.File) (controlResponse, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(deadlineCtx, "unix", socketPath)
	if err != nil {
		slog.WarnContext(ctx, "daemon.supervisor.control.connect_failed", "socket_path", socketPath, "err", err)
		return controlResponse{}, fmt.Errorf("connect supervisor: %w", err)
	}
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return controlResponse{}, fmt.Errorf("supervisor connection has type %T, want *net.UnixConn", conn)
	}
	if deadline, ok := deadlineCtx.Deadline(); ok {
		_ = unixConn.SetDeadline(deadline)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		slog.WarnContext(ctx, "daemon.supervisor.control.encode_request_failed", "err", err)
		return controlResponse{}, fmt.Errorf("encode supervisor request: %w", err)
	}
	payload = append(payload, '\n')
	rights := syscall.UnixRights(fileDescriptors(files)...)
	if _, _, err := unixConn.WriteMsgUnix(payload, rights, nil); err != nil {
		slog.WarnContext(ctx, "daemon.supervisor.control.send_request_failed", "err", err)
		return controlResponse{}, fmt.Errorf("send supervisor request: %w", err)
	}

	var resp controlResponse
	if err := json.NewDecoder(bufio.NewReader(unixConn)).Decode(&resp); err != nil {
		slog.WarnContext(ctx, "daemon.supervisor.control.decode_response_failed", "err", err)
		return controlResponse{}, fmt.Errorf("decode supervisor response: %w", err)
	}
	if resp.Error != "" {
		return controlResponse{}, fmt.Errorf("supervisor request rejected: %s", resp.Error)
	}
	return resp, nil
}

func readControlRequest(conn *net.UnixConn) (controlRequest, []*os.File, error) {
	buffer := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(256*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buffer, oob)
	if err != nil {
		slog.Warn("daemon.supervisor.reload_request.read_failed", "err", err)
		return controlRequest{}, nil, fmt.Errorf("read supervisor request: %w", err)
	}
	var req controlRequest
	if err := json.Unmarshal(bytesTrimSpace(buffer[:n]), &req); err != nil {
		slog.Warn("daemon.supervisor.reload_request.decode_failed", "err", err)
		return controlRequest{}, nil, fmt.Errorf("decode supervisor request: %w", err)
	}
	files, err := filesFromUnixRights(oob[:oobn])
	if err != nil {
		return controlRequest{}, nil, err
	}
	return req, files, nil
}

func filesFromUnixRights(oob []byte) ([]*os.File, error) {
	messages, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		slog.Warn("daemon.supervisor.reload_request.control_messages_failed", "err", err)
		return nil, fmt.Errorf("parse supervisor control messages: %w", err)
	}
	files := make([]*os.File, 0)
	for _, message := range messages {
		fds, err := syscall.ParseUnixRights(&message)
		if err != nil {
			if errors.Is(err, syscall.EINVAL) {
				continue
			}
			closeFiles(files)
			slog.Warn("daemon.supervisor.reload_request.file_descriptors_failed", "err", err)
			return nil, fmt.Errorf("parse supervisor file descriptors: %w", err)
		}
		for _, fd := range fds {
			if fd < 0 {
				continue
			}
			files = append(files, os.NewFile(uintptr(fd), "daemon-supervisor-fd"))
		}
	}
	return files, nil
}

func writeControlError(conn *net.UnixConn, err error) {
	resp := controlResponse{
		PID:         0,
		Fingerprint: "",
		Error:       err.Error(),
	}
	if encodeErr := json.NewEncoder(conn).Encode(resp); encodeErr != nil {
		return
	}
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

func envKey(entry string) (string, bool) {
	index := strings.IndexByte(entry, '=')
	if index <= 0 {
		return "", false
	}
	return entry[:index], true
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

func bytesTrimSpace(data []byte) []byte {
	for len(data) > 0 {
		last := data[len(data)-1]
		if last != '\n' && last != '\r' && last != ' ' && last != '\t' {
			break
		}
		data = data[:len(data)-1]
	}
	return data
}

func logSupervisorPanic(log *slog.Logger, event string, recovered string) {
	log.Warn(event,
		"component", "daemon",
		"panic", recovered,
	)
}
