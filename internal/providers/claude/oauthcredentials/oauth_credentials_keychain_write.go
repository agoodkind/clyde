package oauthcredentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// DefaultKeychainService is the macOS Keychain service name Claude Code uses
// for its OAuth credential entry. Clyde reuses the same name so a write here
// surfaces to Claude Code on its next read.
const DefaultKeychainService = "Claude Code-credentials"

// keychainWriter updates the OAuth entry in the macOS login keychain.
// Read-once-then-cache semantics on the kSecAttrAccount value protect
// against operator configurations where the account name is anything other
// than the current login user.
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
// Errors from the underlying keychain call are wrapped and logged at warn.
// The token bytes are never logged.
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
	return w.writeEntry(ctx, account, jsonBytes)
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
