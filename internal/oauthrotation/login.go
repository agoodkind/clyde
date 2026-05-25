package oauthrotation

import (
	"context"
	"fmt"
	"os"
	"strings"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// LoginResult is the outcome of a completed interactive login: the account the
// new credential resolved to, plus the label recorded for it.
type LoginResult struct {
	Account provider.AccountID
	Label   string
}

// Login drives a provider's interactive login end to end against the
// daemon-owned rotator. It calls the provider's SpawnLogin, surfaces the
// authorize URL through onAuthorizeURL as soon as it is available, then waits
// for CompleteLogin, derives the account id, persists the credential and label
// into the rotation store under the per-account lock, and folds the account
// into the in-memory slot map so the next Token call can serve it. The
// onAuthorizeURL callback is optional; a nil callback is ignored.
//
// This is the entry point that wires the provider's SpawnLogin and
// CompleteLogin methods into a running daemon: before this, those methods had
// no production caller.
func (r *Rotator) Login(ctx context.Context, name provider.Name, opts provider.LoginOptions, onAuthorizeURL func(authorizeURL string)) (LoginResult, error) {
	state, err := r.providerState(ctx, name)
	if err != nil {
		return LoginResult{}, err
	}
	prov := state.prov

	session, err := prov.SpawnLogin(ctx, opts)
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.login.spawn_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", err.Error(),
		)
		return LoginResult{}, fmt.Errorf("spawn login for provider %q: %w", name, err)
	}
	if onAuthorizeURL != nil {
		onAuthorizeURL(session.AuthorizeURL)
	}

	cred, err := prov.CompleteLogin(ctx, session)
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.login.complete_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", err.Error(),
		)
		return LoginResult{}, fmt.Errorf("complete login for provider %q: %w", name, err)
	}

	identity, err := prov.AccountIdentity(ctx, cred.AccessToken)
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.login.account_id_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", err.Error(),
		)
		return LoginResult{}, fmt.Errorf("derive account id after login for provider %q: %w", name, err)
	}
	account := identity.Account

	// The operator label wins when supplied; otherwise fall back to the
	// provider-derived label (an email or display name) so a login with no
	// explicit label still shows a human-readable account in `clyde oauth list`.
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		label = strings.TrimSpace(identity.Label)
	}
	if err := r.persistLoggedInAccount(ctx, prov, account, cred, label); err != nil {
		return LoginResult{}, err
	}
	r.foldLoggedInAccount(state, account, cred, label)
	r.clearReauthRequired(name, account)
	// Surface the fresh credential to the cross-process source of truth (the
	// macOS keychain entry on Anthropic) so Claude Code's next read picks up
	// the new login without prompting the operator a second time. A nil-
	// coordinator provider is a no-op.
	if coord, ok := prov.(provider.RefreshCoordinator); ok {
		if err := coord.WriteUpstreamCredentials(ctx, cred); err != nil {
			r.logger.WarnContext(ctx, "oauthrotation.login.upstream_write_failed",
				"component", "oauthrotation",
				"provider", string(name),
				"account", string(account),
				"err", err.Error(),
			)
		}
	}
	r.logger.InfoContext(ctx, "oauthrotation.login.completed",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
		"label", label,
	)
	return LoginResult{Account: account, Label: label}, nil
}

// persistLoggedInAccount encodes and writes the new credential and label under
// the per-account file lock so a concurrent refresh cannot interleave a partial
// write.
func (r *Rotator) persistLoggedInAccount(ctx context.Context, prov provider.Provider, account provider.AccountID, cred provider.Credentials, label string) error {
	release, err := r.acquireAccountLock(ctx, prov.Name(), account)
	if err != nil {
		return err
	}
	defer release()

	encoded, err := prov.EncodeStored(cred)
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.login.encode_failed",
			"component", "oauthrotation",
			"provider", string(prov.Name()),
			"account", string(account),
			"err", err.Error(),
		)
		return fmt.Errorf("encode login credential for account %q: %w", account, err)
	}
	if err := r.persistCredential(ctx, prov.Name(), account, encoded); err != nil {
		return err
	}
	if label != "" {
		if err := r.writeAccountLabel(ctx, prov.Name(), account, label); err != nil {
			return err
		}
	}
	return nil
}

// foldLoggedInAccount inserts or updates the in-memory slot for a just-logged-in
// account, preserving append-only order for newly added accounts.
func (r *Rotator) foldLoggedInAccount(state *providerState, account provider.AccountID, cred provider.Credentials, label string) {
	state.mu.Lock()
	slot, exists := state.slots[account]
	if !exists {
		slot = newAccountSlot(account)
		state.slots[account] = slot
		state.order = append(state.order, account)
	}
	state.mu.Unlock()
	slot.mu.Lock()
	slot.cred = cred
	if label != "" {
		slot.label = label
	}
	slot.mu.Unlock()
}

// writeAccountLabel persists the operator label into the per-account label file.
func (r *Rotator) writeAccountLabel(ctx context.Context, name provider.Name, account provider.AccountID, label string) error {
	dir := accountDir(name, account)
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.label.mkdir_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"dir", dir,
			"err", err.Error(),
		)
		return fmt.Errorf("mkdir account dir %s: %w", dir, err)
	}
	path := accountLabelPath(name, account)
	if err := os.WriteFile(path, []byte(label), credentialFileMode); err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.label.write_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"path", path,
			"err", err.Error(),
		)
		return fmt.Errorf("write account label %s: %w", path, err)
	}
	return nil
}

// readAccountLabel reads the per-account label file, returning an empty string
// when no label was recorded. A read error other than not-exist is logged and
// swallowed so a missing label never blocks account loading.
func (r *Rotator) readAccountLabel(name provider.Name, account provider.AccountID) string {
	raw, err := os.ReadFile(accountLabelPath(name, account))
	if err != nil {
		if !os.IsNotExist(err) {
			r.logger.Warn("oauthrotation.label.read_failed",
				"component", "oauthrotation",
				"provider", string(name),
				"account", string(account),
				"err", err.Error(),
			)
		}
		return ""
	}
	return strings.TrimSpace(string(raw))
}
