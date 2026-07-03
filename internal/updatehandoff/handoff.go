// Package updatehandoff executes the post-update daemon deploy handoff.
package updatehandoff

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

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
	slog.InfoContext(ctx, "update.deploy_handoff.start", "concern", "cli.update", "component", "updatehandoff", "path", installedPath)
	cmd := exec.CommandContext(ctx, installedPath, "daemon", "deploy")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		slog.WarnContext(ctx, "update.deploy_handoff.failed", "concern", "cli.update", "component", "updatehandoff", "path", installedPath, "err", err)
		return fmt.Errorf("run new binary daemon deploy: %w", err)
	}
	slog.InfoContext(ctx, "update.deploy_handoff.done", "concern", "cli.update", "component", "updatehandoff", "path", installedPath)
	return nil
}
