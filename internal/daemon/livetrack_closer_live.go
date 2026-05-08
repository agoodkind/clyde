package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"time"
)

// codexRuntimeCloser implements livetrack.Closer for a Codex live runtime.
// Close delegates to the underlying runtime so the app-server subprocess
// receives an orderly shutdown signal.
type codexRuntimeCloser struct {
	runtime interface{ Close() error }
}

// Close terminates the Codex live runtime. The reason argument is included
// in any returned error for operator diagnostics.
func (c *codexRuntimeCloser) Close(reason string) error {
	if c.runtime == nil {
		return nil
	}
	if err := c.runtime.Close(); err != nil {
		wrapped := fmt.Errorf("codex runtime close (%s): %w", reason, err)
		slog.Warn("daemon.live.codex_runtime_close_failed", "component", "daemon", "reason", reason, "err", wrapped)
		return wrapped
	}
	return nil
}

// claudeWorkerCloser implements livetrack.Closer for a daemon-owned Claude
// remote worker process. Close sends SIGINT, waits up to interruptGrace for
// a clean exit, then delivers SIGKILL.
type claudeWorkerCloser struct {
	proc           *os.Process
	done           <-chan struct{}
	interruptGrace time.Duration
}

// Close gracefully terminates the Claude remote worker. It mirrors the
// suspend path in suspendClaudeRemoteForForeground so the force-close
// behavior is consistent with the foreground-lease suspend path.
func (c *claudeWorkerCloser) Close(reason string) error {
	if c.proc == nil {
		return nil
	}
	if err := c.proc.Signal(os.Interrupt); err != nil {
		if killErr := c.proc.Kill(); killErr != nil {
			wrapped := fmt.Errorf("claude worker close (%s): kill: %w", reason, killErr)
			slog.Warn("daemon.live.claude_worker_kill_failed", "component", "daemon", "reason", reason, "err", wrapped)
			return wrapped
		}
		return nil
	}
	grace := c.interruptGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	if c.done == nil {
		return nil
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-c.done:
		return nil
	case <-timer.C:
		if err := c.proc.Kill(); err != nil {
			wrapped := fmt.Errorf("claude worker close (%s): kill after interrupt timeout: %w", reason, err)
			slog.Warn("daemon.live.claude_worker_kill_after_timeout_failed", "component", "daemon", "reason", reason, "err", wrapped)
			return wrapped
		}
		select {
		case <-c.done:
		case <-time.After(1 * time.Second):
		}
		return nil
	}
}
