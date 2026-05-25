//go:build darwin

package oauthcredentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/user"

	"github.com/keybase/go-keychain"
)

// writeEntry creates or updates the generic-password entry for w.service
// with the given account name and payload bytes. The payload is passed
// through Security framework data parameters, never through process argv,
// so the secret is not visible to other processes via ps.
func (w *keychainWriter) writeEntry(ctx context.Context, account string, payload []byte) error {
	newItem := keychain.NewGenericPassword(w.service, account, "", payload, "")
	newItem.SetSynchronizable(keychain.SynchronizableNo)
	newItem.SetAccessible(keychain.AccessibleWhenUnlocked)

	addErr := keychain.AddItem(newItem)
	if addErr == nil {
		return nil
	}
	if !errors.Is(addErr, keychain.ErrorDuplicateItem) {
		slog.WarnContext(ctx, "oauthcredentials.keychain.add_failed",
			"component", "oauthcredentials",
			"service", w.service,
			"err", addErr.Error(),
		)
		return fmt.Errorf("keychain: add item %q: %w", w.service, addErr)
	}

	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(w.service)
	query.SetAccount(account)

	update := keychain.NewItem()
	update.SetData(payload)

	if err := keychain.UpdateItem(query, update); err != nil {
		slog.WarnContext(ctx, "oauthcredentials.keychain.update_failed",
			"component", "oauthcredentials",
			"service", w.service,
			"err", err.Error(),
		)
		return fmt.Errorf("keychain: update item %q: %w", w.service, err)
	}
	return nil
}

// readExistingAccount reads the kSecAttrAccount attribute for the existing
// keychain entry. A missing entry falls back to the current login user so
// the first-ever write still succeeds without prompting the user.
func (w *keychainWriter) readExistingAccount(ctx context.Context) (string, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(w.service)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnAttributes(true)

	results, err := keychain.QueryItem(query)
	if err != nil && !errors.Is(err, keychain.ErrorItemNotFound) {
		slog.WarnContext(ctx, "oauthcredentials.keychain.find_failed",
			"component", "oauthcredentials",
			"service", w.service,
			"err", err.Error(),
		)
		return "", fmt.Errorf("keychain: query item %q: %w", w.service, err)
	}
	for _, result := range results {
		if result.Account != "" {
			return result.Account, nil
		}
	}
	return readLoginUser()
}

// readLoginUser returns the current login user. The macOS keychain accepts any
// non-empty string for the account attribute; using the login user matches
// Claude Code's behavior when it creates the entry for the first time.
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
