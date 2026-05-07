package daemon

import (
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
	"runtime"
	"strconv"
	"syscall"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

const (
	supervisorWorkerReadyTimeout = 5 * time.Second
	supervisorWorkerStopTimeout  = 5 * time.Second
)

var (
	daemonSupervisorExecutablePath       = os.Executable
	daemonSupervisorCommand              = exec.Command
	errSupervisorWorkerExitedBeforeReady = errors.New("daemon worker exited before readiness")
)

type supervisorWorkerHandle struct {
	cmd    *exec.Cmd
	waitCh <-chan error
}

func logSupervisorPanic(log *slog.Logger, event string, recovered string) {
	log.Warn(event,
		"component", "daemon",
		"panic", recovered,
	)
}

// RunCommand owns the platform-specific daemon command entrypoint.
// On Darwin, the command process is the launchd-owned supervisor; on other
// platforms it runs the daemon service directly.
func RunCommand(log *slog.Logger, extraLoops ...ExtraLoop) error {
	if runtime.GOOS == "darwin" {
		return Supervise(log)
	}
	return Run(log, extraLoops...)
}

// Supervise starts the daemon worker process and owns its signal lifecycle.
// launchd supervises this parent process, while the child runs the existing
// daemon service implementation.
func Supervise(log *slog.Logger) error {
	log = slogger.WithConcern(log, slogger.ConcernProcessDaemonLifecycle)
	if err := config.EnsureRuntimeDir(); err != nil {
		log.Warn("daemon.supervisor.runtime_dir_failed", "component", "daemon", "err", err)
		return fmt.Errorf("ensure daemon runtime dir: %w", err)
	}
	executablePath, err := daemonSupervisorExecutablePath()
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

	socketPath := supervisorSocketPath()
	_ = os.Remove(socketPath)
	controlListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		log.Warn("daemon.supervisor.reload_socket_failed", "component", "daemon", "socket_path", socketPath, "err", err)
		return fmt.Errorf("listen daemon supervisor reload socket %s: %w", socketPath, err)
	}
	defer func() { _ = controlListener.Close() }()
	defer func() { _ = os.Remove(socketPath) }()

	handle, err := startSupervisorWorker(supervisorWorkerCommand(executablePath, readyWrite, 3, socketPath))
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
	)

	readyCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logSupervisorPanic(log, "daemon.supervisor.worker_ready_wait", fmt.Sprint(recovered))
			}
		}()
		readyCh <- waitForSupervisorWorkerReady(context.Background(), readyRead, handle.waitCh)
	}()

	select {
	case sig := <-signalCh:
		log.Info("daemon.supervisor.signal_received",
			"component", "daemon",
			"signal", fmt.Sprint(sig),
			"pid", handle.cmd.Process.Pid,
		)
		stopSupervisorWorker(log, handle.cmd, sig, handle.waitCh)
		return nil
	case err := <-readyCh:
		if err != nil {
			if !errors.Is(err, errSupervisorWorkerExitedBeforeReady) {
				stopSupervisorWorker(log, handle.cmd, syscall.SIGTERM, handle.waitCh)
			}
			return err
		}
	}
	log.Info("daemon.supervisor.worker_ready",
		"component", "daemon",
		"pid", handle.cmd.Process.Pid,
	)

	replacementCh := make(chan supervisorWorkerHandle, 1)
	controlErrCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logSupervisorPanic(log, "daemon.supervisor.reload_control", fmt.Sprint(recovered))
			}
		}()
		serveSupervisorReloadControl(log, controlListener, replacementCh, controlErrCh)
	}()

	return runSupervisorLoop(log, signalCh, handle, replacementCh, controlErrCh)
}

func runSupervisorLoop(log *slog.Logger, signalCh <-chan os.Signal, handle supervisorWorkerHandle, replacementCh <-chan supervisorWorkerHandle, controlErrCh <-chan error) error {
	current := handle
	for {
		select {
		case sig := <-signalCh:
			log.Debug("daemon.supervisor.signal_received",
				"component", "daemon",
				"signal", fmt.Sprint(sig),
				"pid", current.cmd.Process.Pid,
			)
			stopSupervisorWorker(log, current.cmd, sig, current.waitCh)
			return nil
		case replacement := <-replacementCh:
			current = replacement
			log.Debug("daemon.supervisor.worker_replaced",
				"component", "daemon",
				"pid", current.cmd.Process.Pid,
			)
		case err := <-current.waitCh:
			return supervisorWorkerExitError(err)
		case err := <-controlErrCh:
			return err
		}
	}
}

func supervisorSocketPath() string {
	return filepath.Join(config.RuntimeDir(), "daemon.supervisor.sock")
}

func supervisorWorkerCommand(executablePath string, readyWrite *os.File, readyFD int, supervisorSocketPath string) *exec.Cmd {
	cmd := daemonSupervisorCommand(executablePath, "daemon", "worker")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.ExtraFiles = []*os.File{readyWrite}
	cmd.Env = daemonEnvWithOverrides(os.Environ(),
		envDaemonReloadChild+"=",
		envDaemonInheritedListeners+"=",
		envDaemonReadyFD+"="+strconv.Itoa(readyFD),
		envDaemonSupervisorSocket+"="+supervisorSocketPath,
	)
	return cmd
}

func startSupervisorWorker(cmd *exec.Cmd) (supervisorWorkerHandle, error) {
	if err := cmd.Start(); err != nil {
		slog.Warn("daemon.supervisor.worker_start_failed", "err", err)
		return supervisorWorkerHandle{}, fmt.Errorf("start daemon worker: %w", err)
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
	return supervisorWorkerHandle{cmd: cmd, waitCh: waitCh}, nil
}

func waitForSupervisorWorkerReady(ctx context.Context, ready io.Reader, waitCh <-chan error) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, supervisorWorkerReadyTimeout)
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
		return fmt.Errorf("%w: %w", errSupervisorWorkerExitedBeforeReady, supervisorWorkerExitError(err))
	case <-deadlineCtx.Done():
		slog.WarnContext(ctx, "daemon.supervisor.worker_ready_timeout", "err", deadlineCtx.Err())
		return fmt.Errorf("daemon worker did not become ready: %w", deadlineCtx.Err())
	}
}

func stopSupervisorWorker(log *slog.Logger, cmd *exec.Cmd, sig os.Signal, waitCh <-chan error) {
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
	case <-time.After(supervisorWorkerStopTimeout):
		log.Warn("daemon.supervisor.worker_stop_timeout",
			"component", "daemon",
			"pid", cmd.Process.Pid,
		)
		_ = cmd.Process.Kill()
		<-waitCh
	}
}

func supervisorWorkerExitError(err error) error {
	if err == nil {
		return fmt.Errorf("daemon worker exited")
	}
	slog.Warn("daemon.supervisor.worker_exited", "err", err)
	return fmt.Errorf("daemon worker exited: %w", err)
}

func serveSupervisorReloadControl(log *slog.Logger, listener *net.UnixListener, replacementCh chan<- supervisorWorkerHandle, errCh chan<- error) {
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
			handleSupervisorReloadControl(log, conn, replacementCh, supervisorSocketPath)
		}()
	}
}

func handleSupervisorReloadControl(log *slog.Logger, conn *net.UnixConn, replacementCh chan<- supervisorWorkerHandle, supervisorSocketPath string) {
	defer func() { _ = conn.Close() }()
	req, files, err := readSupervisorReloadRequest(conn)
	if err != nil {
		writeSupervisorReloadError(conn, err)
		return
	}
	defer closeFiles(files)
	if req.Operation != supervisorReloadOperation {
		writeSupervisorReloadError(conn, fmt.Errorf("unsupported supervisor operation %q", req.Operation))
		return
	}
	if len(req.Arguments) < 1 {
		writeSupervisorReloadError(conn, fmt.Errorf("supervisor reload request missing arguments"))
		return
	}
	env, err := supervisorReloadWorkerEnvironment(req, supervisorSocketPath)
	if err != nil {
		writeSupervisorReloadError(conn, err)
		return
	}
	cmd := daemonSupervisorCommand(req.ExecutablePath, req.Arguments[1:]...)
	cmd.Args = append([]string{}, req.Arguments...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = env
	cmd.ExtraFiles = append([]*os.File{}, files...)
	handle, err := startSupervisorWorker(cmd)
	if err != nil {
		writeSupervisorReloadError(conn, fmt.Errorf("start replacement daemon worker: %w", err))
		return
	}
	replacementCh <- handle
	log.Info("daemon.supervisor.reload_replacement_started",
		"component", "daemon",
		"pid", handle.cmd.Process.Pid,
	)
	resp := supervisorReloadResponse{
		PID:   handle.cmd.Process.Pid,
		Error: "",
	}
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		log.Warn("daemon.supervisor.reload_response_failed",
			"component", "daemon",
			"pid", handle.cmd.Process.Pid,
			"err", err,
		)
	}
}

func supervisorReloadWorkerEnvironment(req supervisorReloadRequest, supervisorSocketPath string) ([]string, error) {
	specJSON, err := json.Marshal(req.Listeners)
	if err != nil {
		slog.Warn("daemon.supervisor.reload_request.encode_listeners_failed", "err", err)
		return nil, fmt.Errorf("encode supervisor reload listener specs: %w", err)
	}
	return daemonEnvWithOverrides(req.Environment,
		envDaemonReloadChild+"=1",
		envDaemonInheritedListeners+"="+string(specJSON),
		envDaemonReadyFD+"="+strconv.Itoa(req.ReadyFD),
		envDaemonSupervisorSocket+"="+supervisorSocketPath,
	), nil
}

func readSupervisorReloadRequest(conn *net.UnixConn) (supervisorReloadRequest, []*os.File, error) {
	buffer := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(256*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buffer, oob)
	if err != nil {
		slog.Warn("daemon.supervisor.reload_request.read_failed", "err", err)
		return supervisorReloadRequest{}, nil, fmt.Errorf("read supervisor reload request: %w", err)
	}
	var req supervisorReloadRequest
	if err := json.Unmarshal(bytesTrimSpace(buffer[:n]), &req); err != nil {
		slog.Warn("daemon.supervisor.reload_request.decode_failed", "err", err)
		return supervisorReloadRequest{}, nil, fmt.Errorf("decode supervisor reload request: %w", err)
	}
	files, err := filesFromUnixRights(oob[:oobn])
	if err != nil {
		return supervisorReloadRequest{}, nil, err
	}
	return req, files, nil
}

func filesFromUnixRights(oob []byte) ([]*os.File, error) {
	messages, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		slog.Warn("daemon.supervisor.reload_request.control_messages_failed", "err", err)
		return nil, fmt.Errorf("parse supervisor reload control messages: %w", err)
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
			return nil, fmt.Errorf("parse supervisor reload file descriptors: %w", err)
		}
		for _, fd := range fds {
			if fd < 0 {
				continue
			}
			files = append(files, os.NewFile(uintptr(fd), "daemon-supervisor-reload-fd"))
		}
	}
	return files, nil
}

func writeSupervisorReloadError(conn *net.UnixConn, err error) {
	resp := supervisorReloadResponse{
		PID:   0,
		Error: err.Error(),
	}
	if encodeErr := json.NewEncoder(conn).Encode(resp); encodeErr != nil {
		return
	}
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
