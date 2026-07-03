// Package updatehandoff executes the post-update daemon deploy handoff.
package updatehandoff

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// deployTimeout bounds the daemon deploy subprocess. A deploy legally takes up
// to a minute of worker drain plus service manager operations; the bound only
// stops a wedged deploy from blocking an update forever under the long-lived
// contexts both callers pass in.
const deployTimeout = 5 * time.Minute

// MaxLogBytes bounds captured deploy output before it is attached to a log
// event, so a chatty or looping deploy cannot produce huge log entries or
// spike memory.
const MaxLogBytes = 8 << 10

// TruncateForLog caps s at MaxLogBytes, marking the cut so a truncated log
// field is not mistaken for the full output.
func TruncateForLog(s string) string {
	if len(s) <= MaxLogBytes {
		return s
	}
	return s[:MaxLogBytes] + "... (truncated)"
}

// Deploy resolves the current executable path and runs its daemon deploy
// command as a subprocess.
func Deploy(ctx context.Context, stdout io.Writer, stderr io.Writer) error {
	installedPath, err := os.Executable()
	if err != nil {
		slog.WarnContext(ctx, "update.deploy_handoff.executable_failed", "concern", "cli.update", "component", "updatehandoff", "err", err)
		return fmt.Errorf("resolve installed binary path: %w", err)
	}
	return DeployPath(ctx, installedPath, stdout, stderr)
}

// DeployPath runs the installed binary's daemon deploy command as a subprocess.
func DeployPath(ctx context.Context, installedPath string, stdout io.Writer, stderr io.Writer) error {
	if strings.TrimSpace(installedPath) == "" {
		slog.WarnContext(ctx, "update.deploy_handoff.path_missing", "concern", "cli.update", "component", "updatehandoff")
		return fmt.Errorf("installed binary path is required")
	}
	// Require an absolute path so exec.CommandContext runs the installed binary
	// directly rather than resolving a bare name through PATH, which could exec
	// an unintended binary during this security-sensitive handoff.
	if !filepath.IsAbs(installedPath) {
		slog.WarnContext(ctx, "update.deploy_handoff.path_not_absolute", "concern", "cli.update", "component", "updatehandoff", "path", installedPath)
		return fmt.Errorf("installed binary path must be absolute: %q", installedPath)
	}
	slog.InfoContext(ctx, "update.deploy_handoff.start", "concern", "cli.update", "component", "updatehandoff", "path", installedPath)
	deployCtx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()
	cmd := exec.CommandContext(deployCtx, installedPath, "daemon", "deploy")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		slog.WarnContext(ctx, "update.deploy_handoff.failed", "concern", "cli.update", "component", "updatehandoff", "path", installedPath, "err", err)
		return fmt.Errorf("run new binary daemon deploy: %w", err)
	}
	slog.InfoContext(ctx, "update.deploy_handoff.done", "concern", "cli.update", "component", "updatehandoff", "path", installedPath)
	return nil
}
