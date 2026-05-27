//go:build darwin

package oauthcredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"os/user"
	"strings"
)

// writeEntry creates or updates the generic-password entry for w.service
// with the given account name and payload bytes. The implementation shells
// out to `security add-generic-password -U`. The calling process is
// /usr/bin/security, which is in every keychain entry's ACL by default, so
// the write proceeds without prompting the operator.
func (w *keychainWriter) writeEntry(ctx context.Context, account string, payload []byte) error {
	cmd := exec.CommandContext(ctx, securityBinaryPath, "add-generic-password",
		"-U",
		"-s", w.service,
		"-a", account,
		"-w", string(payload),
	)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		slog.WarnContext(ctx, "oauthcredentials.keychain.add_failed",
			"component", "oauthcredentials",
			"service", w.service,
			"err", err.Error(),
			"stderr", stderrText,
		)
		return fmt.Errorf("keychain: security add-generic-password %q: %w (stderr=%s)", w.service, err, stderrText)
	}
	return nil
}

// readExistingAccount returns the kSecAttrAccount attribute for the existing
// keychain entry. It shells out to `security find-generic-password -g`,
// which prints attribute metadata (including the "acct" line) to stderr,
// and parses the value from there. A missing entry falls back to the
// current login user so the first-ever write still succeeds.
func (w *keychainWriter) readExistingAccount(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, securityBinaryPath, "find-generic-password",
		"-s", w.service,
		"-g",
	)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	combined := stdout.String() + stderr.String()
	if account := parseAcctAttribute(combined); account != "" {
		return account, nil
	}
	if runErr == nil {
		return readLoginUser()
	}
	if keychainItemNotFound(runErr) {
		return readLoginUser()
	}
	slog.WarnContext(ctx, "oauthcredentials.keychain.find_failed",
		"component", "oauthcredentials",
		"service", w.service,
		"err", runErr.Error(),
	)
	return "", fmt.Errorf("keychain: security find-generic-password %q: %w", w.service, runErr)
}

// parseAcctAttribute scans `security find-generic-password -g` output for
// the line:
//
//	"acct"<blob>="some-account"
//
// and returns the unquoted value. Returns "" when the line is absent.
func parseAcctAttribute(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `"acct"`) {
			continue
		}
		_, after, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		value := strings.TrimSpace(after)
		value = strings.TrimSuffix(value, ",")
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 2 {
			return value[1 : len(value)-1]
		}
		return value
	}
	return ""
}

// readLoginUser returns the current login user. The macOS keychain accepts
// any non-empty string for the account attribute; using the login user
// matches Claude Code's behavior when it creates the entry for the first
// time.
func readLoginUser() (string, error) {
	current, err := user.Current()
	if err != nil {
		slog.Warn("oauthcredentials.keychain.user_current_failed",
			"component", "oauthcredentials",
			"err", err.Error(),
		)
		return "", fmt.Errorf("keychain: read login user: %w", err)
	}
	if current.Username == "" {
		slog.Warn("oauthcredentials.keychain.user_current_empty",
			"component", "oauthcredentials",
		)
		return "", errors.New("keychain: current user has empty username")
	}
	return current.Username, nil
}
