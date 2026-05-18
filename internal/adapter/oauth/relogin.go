// Package oauth manages adapter OAuth token flows and persistence.
// fails with invalid_grant. Concurrent callers coalesce into a single
// child process via per-process singleflight + cross-process flock.
package oauth

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/term"
)

// reloginMinInterval throttles auto-relogin attempts to one per
// window. Without this, a tight retry loop in the caller could spawn
// `claude auth login` repeatedly and pop a browser window every time.
const reloginMinInterval = 60 * time.Second

// reloginBreakerThreshold is how many consecutive failures trip the
// circuit breaker. Once tripped, further attempts return the original
// error with a hint until a successful relogin resets the count.
const reloginBreakerThreshold = 3

// reloginTimeout caps how long we wait for `claude auth login` to
// finish (browser-based PKCE flow, user interaction included).
const reloginTimeout = 5 * time.Minute

// reloginState tracks rate-limit and breaker state across calls.
// Held inside Manager; access guarded by Manager.mu since all callers
// hold that mutex when reaching auto-relogin.
type reloginState struct {
	lastAttempt     time.Time
	consecutiveFail int
}

// isInvalidGrant matches the OAuth 2.0 error response that means the
// refresh token itself is dead and no amount of retrying with the
// same token will help. Anthropic returns this with HTTP 400 and a
// JSON body like {"error":"invalid_grant","error_description":"..."}.
func isInvalidGrant(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_grant")
}

// autoRelogin runs `claude auth login` and returns nil on success.
// Caller must hold m.mu. Returns the original refresh error wrapped
// with a hint when the breaker is open or the rate limit fires; the
// caller should bubble that up unchanged.
func (m *Manager) autoRelogin(ctx context.Context, originalErr error) error {
	log := oauthLog.Logger()
	if !isInvalidGrant(originalErr) {
		return originalErr
	}

	// `claude auth login` is a browser-based PKCE flow. Running it from a
	// non-interactive context (launchd, daemon) pops browser tabs at the
	// user with no way to answer prompts. Refuse to spawn when stderr is
	// not a TTY. The error returned bubbles up through the adapter to the
	// calling client with a hint to run `claude auth login` manually.
	// TODO: add terminal-notifier integration so daemons can nudge the
	// user via Notification Center without the osascript attribution
	// annoyance (clicking an osascript-posted notification opens Script
	// Editor).
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		log.WarnContext(ctx, "oauth.relogin.skipped",
			"subcomponent", "oauth",
			"reason", "non_tty",
		)
		return fmt.Errorf("%w; auto re-login skipped (non-interactive context); run `claude auth login` manually",
			originalErr)
	}

	if m.relogin.consecutiveFail >= reloginBreakerThreshold {
		log.WarnContext(ctx, "oauth.relogin.skipped",
			"subcomponent", "oauth",
			"reason", "breaker_open",
			"consecutive_fail", m.relogin.consecutiveFail,
		)
		return fmt.Errorf("%w; auto re-login disabled after %d failures, run `claude auth login` manually",
			originalErr, m.relogin.consecutiveFail)
	}

	if !m.relogin.lastAttempt.IsZero() && time.Since(m.relogin.lastAttempt) < reloginMinInterval {
		log.WarnContext(ctx, "oauth.relogin.skipped",
			"subcomponent", "oauth",
			"reason", "rate_limited",
			"since_last_ms", time.Since(m.relogin.lastAttempt).Milliseconds(),
		)
		return fmt.Errorf("%w; auto re-login rate-limited (one attempt per %s)",
			originalErr, reloginMinInterval)
	}

	// Cross-process lock: another clyde instance may also be relogging.
	// Wait briefly; whoever wins runs login, others re-read tokens.
	if m.credentialsDir == "" {
		log.WarnContext(ctx, "oauth.relogin.store_dir_missing",
			"subcomponent", "oauth",
		)
		return fmt.Errorf("relogin: empty credentials dir (original: %w)", originalErr)
	}
	if err := os.MkdirAll(m.credentialsDir, 0o700); err != nil {
		log.WarnContext(ctx, "oauth.relogin.mkdir_failed",
			"subcomponent", "oauth",
			"store_dir", m.credentialsDir,
			"err", err.Error(),
		)
		return fmt.Errorf("relogin mkdir: %w", err)
	}
	lockPath := filepath.Join(m.credentialsDir, ".clyde-relogin.lock")
	lock := flock.New(lockPath)
	lockCtx, cancel := context.WithTimeout(ctx, reloginTimeout)
	defer cancel()
	got, err := lock.TryLockContext(lockCtx, 500*time.Millisecond)
	if err != nil {
		log.WarnContext(ctx, "oauth.relogin.lock_failed",
			"subcomponent", "oauth",
			"lock_path", lockPath,
			"err", err.Error(),
		)
		return fmt.Errorf("acquire relogin lock: %w (original: %w)", err, originalErr)
	}
	if !got {
		log.WarnContext(ctx, "oauth.relogin.lock_timeout",
			"subcomponent", "oauth",
			"lock_path", lockPath,
		)
		return fmt.Errorf("acquire relogin lock: timed out (original: %w)", originalErr)
	}
	defer func() { _ = lock.Unlock() }()

	// Post-lock re-read: another process may have already relogged.
	if selected, rerr := m.reselectCredential(ctx); rerr == nil && selected != nil && !isExpired(selected.Tokens) {
		log.InfoContext(ctx, "oauth.relogin.raced",
			"subcomponent", "oauth",
			"store_kind", selected.Source,
			"expires_at_ms", selected.Tokens.ExpiresAt,
		)
		return nil
	}

	m.relogin.lastAttempt = oauthClock.Now()
	started := oauthClock.Now()

	cmd := exec.CommandContext(lockCtx, "claude", "auth", "login")
	log.InfoContext(ctx, "oauth.relogin.spawned",
		"subcomponent", "oauth",
		"binary", "claude",
		"args", []string{"auth", "login"},
		"timeout_s", int(reloginTimeout.Seconds()),
	)

	out, runErr := cmd.CombinedOutput()
	durationMs := time.Since(started).Milliseconds()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	if runErr != nil {
		m.relogin.consecutiveFail++
		log.ErrorContext(ctx, "oauth.relogin.failed",
			"subcomponent", "oauth",
			"exit_code", exitCode,
			"duration_ms", durationMs,
			"consecutive_fail", m.relogin.consecutiveFail,
			"output_bytes", len(out),
			slog.Any("err", runErr),
		)
		return fmt.Errorf("claude auth login failed (exit %d): %w (original: %w)",
			exitCode, runErr, originalErr)
	}

	m.relogin.consecutiveFail = 0
	log.InfoContext(ctx, "oauth.relogin.completed",
		"subcomponent", "oauth",
		"exit_code", exitCode,
		"duration_ms", durationMs,
	)

	// Force a re-read on the next Token() call by dropping cache.
	// Caller should retry refresh once.
	m.cached = nil
	m.snapshot = emptyCredentialSnapshot()
	return nil
}
