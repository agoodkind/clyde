package oauthcredentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// DefaultKeychainService is the macOS Keychain service name Claude Code uses
// for its OAuth credential entry. Clyde reuses the same name so a write here
// surfaces to Claude Code on its next read.
const DefaultKeychainService = "Claude Code-credentials"

// securityBinaryPath is the absolute path to the macOS keychain tool, used
// by the keychain writer's process invocations.
const securityBinaryPath = "/usr/bin/security"

// idBinaryPath is the absolute path to the login-user lookup tool, used as
// the fallback when no existing keychain entry can be inspected for its
// acct attribute.
const idBinaryPath = "/usr/bin/id"

// keychainWriter updates the OAuth entry in the macOS login keychain.
// Read-once-then-cache semantics on the kSecAttrAccount value (the "acct"
// attribute) protect against operator configurations where the account name
// is anything other than the current login user.
type keychainWriter struct {
	service string

	mu              sync.Mutex
	macOSAccountSet bool
	macOSAccount    string
}

// defaultKeychainWriter is the process-wide writer used by
// WriteKeychainCredentials. Tests construct their own keychainWriter scoped
// to a test-only service name.
var defaultKeychainWriter = &keychainWriter{
	service:         DefaultKeychainService,
	mu:              sync.Mutex{},
	macOSAccountSet: false,
	macOSAccount:    "",
}

// WriteKeychainCredentials updates the macOS keychain entry named
// "Claude Code-credentials" with jsonBytes. The bytes must be the same JSON
// shape Claude Code writes: an object with a single claudeAiOauth key. The
// kSecAttrAccount value of the existing entry is read once and reused so the
// rewrite preserves the entry's identity for Claude Code's reader.
//
// Errors from the underlying security invocation are wrapped with the
// trimmed stderr text and logged at warn. The token bytes are never
// logged.
func WriteKeychainCredentials(ctx context.Context, jsonBytes []byte) error {
	if err := defaultKeychainWriter.write(ctx, jsonBytes); err != nil {
		slog.WarnContext(ctx, "oauthcredentials.keychain.public_write_failed",
			"component", "oauthcredentials",
			"err", err.Error(),
		)
		return fmt.Errorf("oauthcredentials.WriteKeychainCredentials: %w", err)
	}
	return nil
}

// write is the receiver-side entry point so tests can drive a writer scoped
// to a test-only service name.
func (w *keychainWriter) write(ctx context.Context, jsonBytes []byte) error {
	if len(jsonBytes) == 0 {
		return errors.New("keychain: keychain write requires non-empty payload")
	}
	account, err := w.resolveAccount(ctx)
	if err != nil {
		return err
	}
	stderrText, runErr := writeSecurityEntry(ctx, securityBinaryPath, w.service, account, string(jsonBytes))
	stderrText = strings.TrimSpace(stderrText)
	if runErr != nil {
		slog.WarnContext(ctx, "oauthcredentials.keychain.write_failed",
			"component", "oauthcredentials",
			"service", w.service,
			"err", runErr.Error(),
			"stderr", stderrText,
		)
		return fmt.Errorf("keychain: security add-generic-password %q: %w (stderr=%s)", w.service, runErr, stderrText)
	}
	return nil
}

// resolveAccount returns the kSecAttrAccount value for the existing keychain
// entry, caching it after the first successful read. When the entry does not
// yet exist (typical on the very first Clyde-driven write), the function
// falls back to the current login user so the new entry is keyed the same
// way Claude Code's first write would key it.
func (w *keychainWriter) resolveAccount(ctx context.Context) (string, error) {
	w.mu.Lock()
	if w.macOSAccountSet {
		account := w.macOSAccount
		w.mu.Unlock()
		return account, nil
	}
	w.mu.Unlock()

	account, err := w.readExistingAccount(ctx)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	w.macOSAccountSet = true
	w.macOSAccount = account
	w.mu.Unlock()
	return account, nil
}

// readExistingAccount runs `security find-generic-password -g` and parses
// the "acct" attribute line. A missing entry returns the current login user
// as the fallback account name so the first-ever write still succeeds
// without prompting the user.
func (w *keychainWriter) readExistingAccount(ctx context.Context) (string, error) {
	combined, runErr := readSecurityEntry(ctx, securityBinaryPath, w.service)
	if account := parseAcctAttribute(combined); account != "" {
		return account, nil
	}
	if runErr == nil {
		// security succeeded but no acct line was found; fall through to the
		// login-user fallback so the write still has a valid key.
		return loginUser(ctx)
	}
	if keychainItemNotFound(runErr) {
		return loginUser(ctx)
	}
	slog.WarnContext(ctx, "oauthcredentials.keychain.find_failed",
		"component", "oauthcredentials",
		"service", w.service,
		"err", runErr.Error(),
	)
	return "", fmt.Errorf("keychain: security find-generic-password %q: %w", w.service, runErr)
}

// parseAcctAttribute scans the `security find-generic-password -g` output for
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
		// security wraps quoted strings in <blob>"..." for printable text and
		// in the bare form for non-printable bytes. Strip the quotes when
		// present; leave the value untouched otherwise.
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 2 {
			return value[1 : len(value)-1]
		}
		return value
	}
	return ""
}

// loginUser returns the current login user. The macOS keychain accepts any
// non-empty string for the -a attribute; using the login user matches
// Claude Code's behavior when it creates the entry for the first time.
func loginUser(ctx context.Context) (string, error) {
	out, err := readLoginUser(ctx, idBinaryPath)
	if err != nil {
		slog.WarnContext(ctx, "oauthcredentials.keychain.login_user_failed",
			"component", "oauthcredentials",
			"err", err.Error(),
		)
		return "", fmt.Errorf("keychain: read login user: %w", err)
	}
	return strings.TrimSpace(out), nil
}
