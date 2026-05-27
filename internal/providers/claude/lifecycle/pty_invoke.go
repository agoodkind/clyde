// Package claude wraps Claude CLI invocation behavior.
//
// When a session opts into remote control, clyde owns claude's
// stdio so external clients (the dashboard sidecar, the daemon's
// SendToSession RPC) can inject text into claude as if the user
// typed it. The wrapper opens a per session Unix socket and copies
// any bytes received there into claude's pty stdin. Local terminal
// input still flows through unchanged. Output flows back to the
// terminal so the user keeps their normal in terminal experience.
package claude

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"goodkind.io/clyde/internal/terminalcontrol"
)

// invokeInteractivePTY runs claude inside a pty. The terminal is put
// into raw mode so claude's TUI rendering survives. A per session
// Unix socket accepts text from external clients (daemon
// SendToSession path) and writes the bytes into the pty's stdin.
// Returns when claude exits.
func invokeInteractivePTY(args []string, env map[string]string, workDir, sessionID string) error {
	return invokePTY(args, env, workDir, sessionID, true)
}

// StartHeadlessRemoteWorker runs Claude with remote control enabled in a PTY
// that is owned by a background process instead of an interactive terminal.
// The wrapper still exposes the inject socket so the daemon sidecar can send
// text to the running Claude session, but local terminal IO is detached.
func StartHeadlessRemoteWorker(env map[string]string, settingsFile string, workDir, sessionID string) error {
	sessionID = launchSessionID(env, sessionID)

	args := []string{}
	args = appendCommonArgs(args, settingsFile)
	if sessionID != "" {
		env["CLYDE_SESSION_ID"] = sessionID
		args = append(args, "--session-id", sessionID)
	}
	applyDaemonLaunchEnv(env)
	defer cleanupLaunchCredentialDir(env)
	return invokePTY(args, env, workDir, sessionID, false)
}

func invokePTY(args []string, env map[string]string, workDir, sessionID string, interactive bool) error {
	claudeBin := ClaudeBinaryPathFunc()

	// Daemon acquire mirrors the classic invokeInteractive path so
	// per session settings and the wrapper id flow consistently.
	ctx := context.Background()
	wrapperID := fmt.Sprintf("%d", os.Getpid())
	sessionName := env["CLYDE_SESSION_NAME"]
	if envSessionID := env["CLYDE_SESSION_ID"]; strings.TrimSpace(envSessionID) != "" {
		sessionID = envSessionID
	}
	if settingsFile := acquireDaemonSession(ctx, wrapperID, sessionName, sessionID); settingsFile != "" {
		args = append([]string{"--settings", settingsFile}, args...)
	}

	if interactive {
		displayCommand(claudeBin, args, env)
	} else {
		claudeRemoteLog.Logger().Info("wrapper.remote_headless.starting",
			"component", "wrapper",
			"session", sessionName,
			"session_id", sessionID,
			"workdir", workDir,
		)
	}

	cmd := exec.Command(claudeBin, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = commandEnvironment(env)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		remoteLog.Warn("wrapper.pty.start_failed",
			"component", "wrapper",
			"session", sessionName,
			"session_id", sessionID,
			"err", err,
		)
		return fmt.Errorf("start pty: %w", err)
	}
	defer func() { _ = ptmx.Close() }()

	cleanupTerminal := configurePTYTerminal(ptmx, interactive, sessionName, sessionID)
	defer cleanupTerminal()

	// Inject socket lifecycle. The daemon dials this socket from
	// SendToSession to forward text to the running claude process.
	cleanupListener := startPTYInjectListener(ptmx, sessionName, sessionID)
	defer cleanupListener()

	// Copy goroutines for pty <-> terminal.
	done, finish := startPTYCopy(ptmx, interactive, sessionName, sessionID)

	// Daemon settings sync runs alongside the pty path too so global
	// settings changes propagate to this session like any other.
	monitorDone := make(chan struct{})
	monitorStopped := make(chan struct{})
	monitor := &monitorState{}
	startPTYDaemonMonitor(ctx, wrapperID, sessionName, sessionID, monitorDone, monitor, monitorStopped)

	runErr := waitForPTYCommand(cmd, cleanupTerminal, cleanupListener, sessionName, sessionID)
	close(monitorDone)
	<-monitorStopped
	finish()
	<-done
	if interactive && shouldSelfReloadWrapper(env, runErr, monitor) {
		if reloadErr := selfReloadCurrentProcess(); reloadErr != nil {
			remoteLog.Warn("wrapper.pty.self_reload_failed",
				"component", "wrapper",
				"session", sessionName,
				"session_id", sessionID,
				"err", reloadErr,
			)
			return fmt.Errorf("self reload: %w", reloadErr)
		}
	}
	return runErr
}

func configurePTYTerminal(ptmx *os.File, interactive bool, sessionName, sessionID string) func() {
	if !interactive {
		_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})
		return func() {}
	}
	terminalRestorer := terminalcontrol.CaptureProcessRestorer(claudeRemoteLog.Logger())
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				claudeRemoteLog.Logger().Error("wrapper.pty.resize_panic",
					"component", "wrapper",
					"session", sessionName,
					"session_id", sessionID,
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		for range winchCh {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	winchCh <- syscall.SIGWINCH

	oldState, _ := term.MakeRaw(int(os.Stdin.Fd()))
	var cleanupOnce sync.Once
	return func() {
		cleanupOnce.Do(func() {
			signal.Stop(winchCh)
			close(winchCh)
			if terminalRestorer != nil {
				terminalRestorer.Restore()
				return
			}
			if oldState != nil {
				_ = term.Restore(int(os.Stdin.Fd()), oldState)
			}
			terminalcontrol.WriteResetToTerminal()
		})
	}
}

func waitForPTYCommand(
	cmd *exec.Cmd,
	cleanupTerminal func(),
	cleanupListener func(),
	sessionName string,
	sessionID string,
) error {
	waitCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				claudeRemoteLog.Logger().Error("wrapper.pty.wait_panic",
					"component", "wrapper",
					"session", sessionName,
					"session_id", sessionID,
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		waitCh <- cmd.Wait()
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signalCh)

	select {
	case err := <-waitCh:
		return err
	case sig := <-signalCh:
		cleanupTerminal()
		cleanupListener()
		if cmd.Process != nil {
			if signalErr := cmd.Process.Signal(sig); signalErr != nil {
				claudeRemoteLog.Logger().Warn("wrapper.pty.signal_child_failed",
					"component", "wrapper",
					"session", sessionName,
					"session_id", sessionID,
					"signal", sig.String(),
					"err", signalErr,
				)
			}
		}
		select {
		case err := <-waitCh:
			return err
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return fmt.Errorf("terminated by %s", sig)
		}
	}
}

func startPTYInjectListener(ptmx *os.File, sessionName, sessionID string) func() {
	socketPath := injectSocketPathFor(sessionID)
	listener, err := openInjectListener(socketPath)
	if err != nil {
		return func() {}
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				claudeRemoteLog.Logger().Error("wrapper.inject.accept_panic",
					"component", "wrapper",
					"session", sessionName,
					"session_id", sessionID,
					"socket", socketPath,
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		acceptInjectConns(listener, ptmx)
	}()
	return func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
}

func startPTYCopy(ptmx *os.File, interactive bool, sessionName, sessionID string) (<-chan struct{}, func()) {
	done := make(chan struct{})
	var copyOnce sync.Once
	finish := func() { copyOnce.Do(func() { close(done) }) }
	if interactive {
		startPTYCopyGoroutine("stdin", ptmx, os.Stdin, finish, sessionName, sessionID)
		startPTYCopyGoroutine("stdout", os.Stdout, ptmx, finish, sessionName, sessionID)
		return done, finish
	}
	startPTYCopyGoroutine("discard", io.Discard, ptmx, finish, sessionName, sessionID)
	return done, finish
}

func startPTYCopyGoroutine(name string, dst io.Writer, src io.Reader, finish func(), sessionName, sessionID string) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				claudeRemoteLog.Logger().Error("wrapper.pty."+name+"_copy_panic",
					"component", "wrapper",
					"session", sessionName,
					"session_id", sessionID,
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		_, _ = io.Copy(dst, src)
		finish()
	}()
}

func startPTYDaemonMonitor(
	ctx context.Context,
	wrapperID string,
	sessionName string,
	sessionID string,
	monitorDone <-chan struct{},
	monitor *monitorState,
	monitorStopped chan<- struct{},
) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				claudeRemoteLog.Logger().Error("wrapper.pty.daemon_monitor_panic",
					"component", "wrapper",
					"session", sessionName,
					"session_id", sessionID,
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		monitorDaemon(ctx, wrapperID, sessionName, sessionID, monitorDone, monitor, monitorStopped)
	}()
}

// injectSocketPathFor returns the path of the inject socket the
// daemon will dial for sessionID. The directory is created lazily
// here so the daemon can stat the file without racing the wrapper.
func injectSocketPathFor(sessionID string) string {
	dir := injectSocketDir()
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, sessionID+".sock")
}

// injectSocketDir matches the daemon's resolution. Kept duplicated to
// avoid an import cycle between claude and daemon.
func injectSocketDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "clyde", "inject")
	}
	return filepath.Join(os.TempDir(), "clyde-inject")
}

// openInjectListener removes any stale socket from a previous run
// and opens the listener. Failures return nil so the pty wrapper can
// continue without injection support.
func openInjectListener(path string) (net.Listener, error) {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return l, nil
}

// acceptInjectConns reads any bytes a connecting client sends and
// writes them into the pty's stdin. Each connection is short lived:
// the daemon writes, then closes.
func acceptInjectConns(l net.Listener, ptmx io.Writer) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() {
				if recovered := recover(); recovered != nil {
					claudeRemoteLog.Logger().Error("wrapper.inject.connection_panic",
						"component", "wrapper",
						"err", fmt.Errorf("panic: %v", recovered),
					)
				}
			}()
			defer func() { _ = c.Close() }()
			_ = c.SetReadDeadline(currentTime().Add(5 * time.Second))
			_, _ = io.Copy(ptmx, c)
		}(conn)
	}
}
