//go:build darwin

package oauthcredentials

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
)

func runSecurity(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		wrapped := fmt.Errorf("run macOS Keychain query: %w", err)
		slog.WarnContext(
			ctx,
			"antigravity.keychain.command_failed",
			"concern",
			"adapter.providers.antigravity.oauth",
			"command",
			name,
			"err",
			wrapped,
		)

		return nil, wrapped
	}

	return output, nil
}
