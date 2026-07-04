//go:build darwin

package clipboard

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
)

// Copy writes body to the macOS pasteboard via pbcopy.
func Copy(ctx context.Context, body []byte) error {
	cmd := exec.CommandContext(ctx, "pbcopy")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.WarnContext(ctx, "cli.clipboard.copy_stdin_failed", "concern", "cli.output", "component", "cli", "err", err)
		return fmt.Errorf("open pbcopy stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		slog.WarnContext(ctx, "cli.clipboard.copy_start_failed", "concern", "cli.output", "component", "cli", "err", err)
		return fmt.Errorf("start pbcopy: %w", err)
	}
	if _, err := stdin.Write(body); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		slog.WarnContext(ctx, "cli.clipboard.copy_write_failed", "concern", "cli.output", "component", "cli", "err", err)
		return fmt.Errorf("write pbcopy stdin: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		slog.WarnContext(ctx, "cli.clipboard.copy_close_failed", "concern", "cli.output", "component", "cli", "err", err)
		return fmt.Errorf("close pbcopy stdin: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		slog.WarnContext(ctx, "cli.clipboard.copy_wait_failed", "concern", "cli.output", "component", "cli", "err", err)
		return fmt.Errorf("finish pbcopy: %w", err)
	}
	return nil
}
