package oauthrotation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"goodkind.io/clyde/internal/oauthrotation/mirror"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

const (
	// refreshLockTimeout bounds how long a refresh waits for the per-account
	// file lock before giving up.
	refreshLockTimeout = 10 * time.Second
	// refreshLockRetry is the poll interval while waiting for the lock.
	refreshLockRetry = 100 * time.Millisecond
	// defaultRefreshSafetyWindow is the fallback for how far ahead of a stored
	// credential's ExpiresAt a refresh is allowed to run when no configured
	// window is set on the rotator. An account is "due" only when now plus this
	// window reaches or passes its ExpiresAt, so a healthy account is renewed
	// about once per token lifetime rather than on every refresh-loop tick or
	// daemon reload. Anthropic rotates the refresh token on every refresh and
	// invalidates the prior one, so refreshing a still-valid account strands the
	// shared keychain credential other tools rely on (see CLYDE-457/CLYDE-458).
	defaultRefreshSafetyWindow = 5 * time.Minute
	// loadMinInterval is the debounce window between successive Load passes for
	// a given provider. A call inside this window of a prior successful load
	// returns nil immediately so a reload-time re-load or an external trigger
	// does not duplicate the startup walk.
	loadMinInterval = 10 * time.Second
)

// AccountSnapshot is a read-only view of one account slot for the RPC layer.
type AccountSnapshot struct {
	Account     provider.AccountID
	Label       string
	ExpiresAt   time.Time
	Fingerprint string
	// RefreshTokenPresent reports whether the slot holds a refresh credential.
	RefreshTokenPresent bool
	Throttled           bool
	ThrottledTo         time.Time
	Claim               string
	// NeedsReauth reports whether the account's refresh credential is dead and a
	// fresh login is required before the account re-enters rotation.
	NeedsReauth bool
}

// accountSlot holds one account's mutable credential state plus the mutex that
// serializes refresh and persist for that account. The mutex makes concurrent
// Token callers share a single inflight refresh per account.
type accountSlot struct {
	account provider.AccountID
	mu      sync.Mutex
	cred    provider.Credentials
	// label is the operator-supplied human label captured at login time. It is
	// loaded from the per-account label file on import and persisted on login.
	label string
}

// providerState groups everything the rotator tracks for one registered
// provider: the provider implementation, its ordered account slots, its
// throttle store, and a one-way mirror syncer.
type providerState struct {
	prov     provider.Provider
	mu       sync.Mutex
	slots    map[provider.AccountID]*accountSlot
	order    []provider.AccountID
	throttle *throttleStore
	syncer   *mirror.Syncer
	// reauth marks accounts whose refresh credential is dead (the provider
	// reported [provider.ErrReauthRequired]). A marked account is dropped from
	// token selection until a fresh login replaces its credential. The
	// user-facing re-auth surface lands in a later wave; the set keeps the
	// account out of rotation in the meantime.
	reauth map[provider.AccountID]bool
	// loadOnce gates the first-call startup load so the second caller in the
	// same process returns nil without re-running the upstream pass or the
	// on-disk walk. A future cross-process trigger uses the debounce and
	// single-flight gate below instead, so loadOnce only protects the
	// process-local first call. It is a pointer so test code can replace it
	// under state.mu without tripping the copylocks vet rule.
	loadOnce *sync.Once
	// lastLoadAt records when the most recent successful Load returned. A new
	// Load that lands inside loadMinInterval of this timestamp returns nil
	// immediately. It is guarded by mu.
	lastLoadAt time.Time
	// loadInFlight is true while a Load is running for this provider. A second
	// concurrent caller observes the flag and returns nil immediately instead
	// of duplicating the upstream pass and on-disk walk. It is guarded by mu.
	loadInFlight bool
}

// Rotator picks a non-throttled account per provider, mirrors upstream
// credentials one-way, refreshes tokens, and records rate-limit throttles. It
// implements [ratelimitsink.Sink].
type Rotator struct {
	mu        sync.RWMutex
	providers map[provider.Name]*providerState
	logger    *slog.Logger
	now       func() time.Time
	// refreshSafetyWindow is how far ahead of a credential's ExpiresAt a refresh
	// is allowed to run. It defaults to defaultRefreshSafetyWindow and is
	// overridden by SetRefreshSafetyWindow from configuration.
	refreshSafetyWindow time.Duration
}

// NewRotator builds an empty rotator. A nil logger falls back to [slog.Default].
func NewRotator(logger *slog.Logger) *Rotator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Rotator{
		mu:                  sync.RWMutex{},
		providers:           make(map[provider.Name]*providerState),
		logger:              logger,
		now:                 time.Now,
		refreshSafetyWindow: defaultRefreshSafetyWindow,
	}
}

// SetRefreshSafetyWindow overrides how far ahead of a credential's ExpiresAt a
// refresh is allowed to run. It assigns only when d is positive so a zero or
// negative configured value leaves the default in place.
func (r *Rotator) SetRefreshSafetyWindow(d time.Duration) {
	if d > 0 {
		r.refreshSafetyWindow = d
	}
}

// Register adds a provider to the rotator. Registering the same provider name
// twice replaces the prior registration.
func (r *Rotator) Register(p provider.Provider) {
	name := p.Name()
	mirrorPaths := mirror.Paths{
		AccountDir: func(account provider.AccountID) string {
			return accountDir(name, account)
		},
		CredentialsFile: func(account provider.AccountID) string {
			return accountCredentialsPath(name, account)
		},
		LabelFile: func(account provider.AccountID) string {
			return accountLabelPath(name, account)
		},
	}
	modes := mirror.Modes{FileMode: credentialFileMode, DirMode: storeDirMode}
	state := &providerState{
		prov:         p,
		mu:           sync.Mutex{},
		slots:        make(map[provider.AccountID]*accountSlot),
		order:        nil,
		throttle:     newThrottleStore(name, r.logger),
		syncer:       mirror.NewSyncer(p, mirrorPaths, modes, r.logger),
		reauth:       make(map[provider.AccountID]bool),
		loadOnce:     &sync.Once{},
		lastLoadAt:   time.Time{},
		loadInFlight: false,
	}
	r.mu.Lock()
	r.providers[name] = state
	r.mu.Unlock()
}

// Token runs one mirror-sync pass, drops expired throttle entries, walks the
// provider's accounts in a stable order, and returns the first account that is
// not throttled along with its access token. When every account is throttled
// it returns AllAccountsThrottledError with the soonest reset.
func (r *Rotator) Token(ctx context.Context, name provider.Name) (string, provider.AccountID, error) {
	slot, account, err := r.selectActiveSlot(ctx, name)
	if err != nil {
		return "", "", err
	}
	slot.mu.Lock()
	token := slot.cred.AccessToken
	slot.mu.Unlock()
	return token, account, nil
}

// selectActiveSlot runs the same selection Token uses: one mirror-sync pass, a
// throttle-ledger load, then a stable walk over the provider's accounts that
// skips re-auth-marked and throttled slots and returns the first usable one.
// It returns the selected slot so callers can read either the access token
// (Token) or the stored credential encoding (SelectForLaunch) without
// duplicating selection semantics. When every account is throttled it returns
// AllAccountsThrottledError with the soonest reset.
func (r *Rotator) selectActiveSlot(ctx context.Context, name provider.Name) (*accountSlot, provider.AccountID, error) {
	state, err := r.providerState(ctx, name)
	if err != nil {
		return nil, "", err
	}

	r.runMirrorSync(ctx, name, state)

	now := r.now()
	active, err := state.throttle.load(ctx, now)
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.token.load_throttle_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", err.Error(),
		)
		return nil, "", fmt.Errorf("load throttle ledger for %q: %w", name, err)
	}

	state.mu.Lock()
	order := append([]provider.AccountID(nil), state.order...)
	slots := make(map[provider.AccountID]*accountSlot, len(state.slots))
	maps.Copy(slots, state.slots)
	reauth := make(map[provider.AccountID]bool, len(state.reauth))
	maps.Copy(reauth, state.reauth)
	state.mu.Unlock()

	var (
		soonestSet     bool
		soonestReset   time.Time
		soonestAccount provider.AccountID
		reauthSeen     bool
		reauthAccount  provider.AccountID
	)
	for _, account := range order {
		if reauth[account] {
			if !reauthSeen {
				reauthSeen = true
				reauthAccount = account
			}
			continue
		}
		entry, throttled := active[account]
		if !throttled {
			return slots[account], account, nil
		}
		until := time.UnixMilli(entry.UntilMS)
		if !soonestSet || until.Before(soonestReset) {
			soonestSet = true
			soonestReset = until
			soonestAccount = account
		}
	}

	if len(order) == 0 {
		r.logger.WarnContext(ctx, "oauthrotation.token.no_accounts",
			"component", "oauthrotation",
			"provider", string(name),
		)
		return nil, "", fmt.Errorf("no accounts registered for provider %q", name)
	}
	// No usable account remained. A re-auth account never recovers without an
	// operator login, while a throttled account clears on its own at its reset
	// time, so when both kinds blocked selection the re-auth remedy is the
	// actionable one to surface. Throttled-only failures keep the existing
	// AllAccountsThrottledError so the soonest-reset hint survives.
	if reauthSeen {
		r.logger.WarnContext(ctx, "oauthrotation.token.needs_reauth",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(reauthAccount),
		)
		return nil, "", NeedsReauthError{
			Provider: name,
			Account:  reauthAccount,
		}
	}
	r.logger.WarnContext(ctx, "oauthrotation.token.all_throttled",
		"component", "oauthrotation",
		"provider", string(name),
		"soonest_account", string(soonestAccount),
		"soonest_reset_ms", soonestReset.UnixMilli(),
	)
	return nil, "", AllAccountsThrottledError{
		Provider:     name,
		SoonestReset: soonestReset,
		Account:      soonestAccount,
	}
}

// Throttle implements [ratelimitsink.Sink]. It reverse-looks-up the account
// slot by access token, persists a throttle entry keyed by that account, and
// logs.
func (r *Rotator) Throttle(ctx context.Context, sig ratelimitsink.Signal) error {
	name := provider.Name(sig.Provider)
	state, err := r.providerState(ctx, name)
	if err != nil {
		return err
	}

	account, found := r.accountForToken(state, sig.AccessToken)
	if !found {
		r.logger.WarnContext(ctx, "oauthrotation.throttle.unknown_token",
			"component", "oauthrotation",
			"provider", string(name),
			"http_status", sig.HTTPStatus,
		)
		return fmt.Errorf("throttle signal for unknown access token on provider %q", name)
	}

	entry := throttleEntry{
		UntilMS:    sig.ResetAt.UnixMilli(),
		ObservedMS: sig.ObservedAt.UnixMilli(),
		Claim:      string(sig.Claim),
		HTTPStatus: sig.HTTPStatus,
	}
	if err := state.throttle.put(ctx, r.now(), account, entry); err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.throttle.persist_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"err", err.Error(),
		)
		return fmt.Errorf("persist throttle for account %q: %w", account, err)
	}
	r.logger.InfoContext(ctx, "oauthrotation.account.throttled",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
		"claim", string(sig.Claim),
		"http_status", sig.HTTPStatus,
		"reset_at_ms", entry.UntilMS,
	)
	return nil
}

// RefreshAll fans a refresh out over every slot of every provider, persisting
// each renewed credential through the provider's EncodeStored under a
// per-account file lock. It returns the first error encountered after
// attempting every slot.
func (r *Rotator) RefreshAll(ctx context.Context) error {
	r.mu.RLock()
	states := make([]*providerState, 0, len(r.providers))
	for _, state := range r.providers {
		states = append(states, state)
	}
	r.mu.RUnlock()

	var firstErr error
	for _, state := range states {
		state.mu.Lock()
		slots := make([]*accountSlot, 0, len(state.order))
		for _, account := range state.order {
			slots = append(slots, state.slots[account])
		}
		prov := state.prov
		state.mu.Unlock()

		for _, slot := range slots {
			if err := r.refreshSlot(ctx, prov, slot); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// RefreshDue refreshes only the accounts whose stored credential is at or
// inside the safety window, leaving still-valid accounts untouched. The daemon
// refresh loop calls it on every tick and at startup so a reload of a healthy
// fleet mints zero new tokens, while a token approaching expiry is still
// renewed ahead of time. The due decision reads each slot's persisted
// ExpiresAt (loaded from the account's .credentials.json on Harvest), so the
// persisted expiry is the source of truth across restarts. It returns the
// first error encountered after attempting every due slot.
func (r *Rotator) RefreshDue(ctx context.Context) error {
	r.mu.RLock()
	type named struct {
		name  provider.Name
		state *providerState
	}
	states := make([]named, 0, len(r.providers))
	for name, state := range r.providers {
		states = append(states, named{name: name, state: state})
	}
	r.mu.RUnlock()

	now := r.now()
	var firstErr error
	for _, entry := range states {
		state := entry.state
		state.mu.Lock()
		slots := make([]*accountSlot, 0, len(state.order))
		for _, account := range state.order {
			slots = append(slots, state.slots[account])
		}
		prov := state.prov
		state.mu.Unlock()

		for _, slot := range slots {
			slot.mu.Lock()
			expiresAt := slot.cred.ExpiresAt
			account := slot.account
			slot.mu.Unlock()
			if !r.refreshIsDue(now, expiresAt) {
				r.logger.DebugContext(ctx, "oauthrotation.refresh.skipped_not_due",
					"component", "oauthrotation",
					"provider", string(entry.name),
					"account", string(account),
					"expires_at_ms", expiresAt.UnixMilli(),
				)
				continue
			}
			if err := r.refreshSlot(ctx, prov, slot); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// refreshIsDue reports whether a credential expiring at expiresAt should be
// refreshed at now: it is due once now plus the safety window reaches or passes
// expiresAt. A zero expiresAt is treated as due because an unknown expiry can
// not be proven still valid.
func (r *Rotator) refreshIsDue(now time.Time, expiresAt time.Time) bool {
	if expiresAt.IsZero() {
		return true
	}
	return !now.Add(r.refreshSafetyWindow).Before(expiresAt)
}

// Accounts returns read-only snapshots for the RPC layer, in stable order.
func (r *Rotator) Accounts(ctx context.Context, name provider.Name) ([]AccountSnapshot, error) {
	state, err := r.providerState(ctx, name)
	if err != nil {
		return nil, err
	}
	active, err := state.throttle.load(ctx, r.now())
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.accounts.load_throttle_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", err.Error(),
		)
		return nil, fmt.Errorf("load throttle ledger for %q: %w", name, err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	snapshots := make([]AccountSnapshot, 0, len(state.order))
	for _, account := range state.order {
		slot := state.slots[account]
		slot.mu.Lock()
		cred := slot.cred
		label := slot.label
		slot.mu.Unlock()
		snapshot := AccountSnapshot{
			Account:             account,
			Label:               label,
			ExpiresAt:           cred.ExpiresAt,
			Fingerprint:         cred.Fingerprint,
			RefreshTokenPresent: cred.RefreshToken != "",
			Throttled:           false,
			ThrottledTo:         time.Time{},
			Claim:               "",
			NeedsReauth:         state.reauth[account],
		}
		if entry, throttled := active[account]; throttled {
			snapshot.Throttled = true
			snapshot.ThrottledTo = time.UnixMilli(entry.UntilMS)
			snapshot.Claim = entry.Claim
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

// ProviderNames returns the registered provider names in no guaranteed order.
// The RPC layer uses it to list accounts across every provider when no provider
// filter is supplied.
func (r *Rotator) ProviderNames() []provider.Name {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]provider.Name, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	return names
}

// refreshSlot refreshes one account under its per-account file lock and
// persists the renewed credential. Concurrent Token callers do not refresh,
// but RefreshAll and any future refresh-on-use path share this lock so only one
// refresh per account runs at a time.
func (r *Rotator) refreshSlot(ctx context.Context, prov provider.Provider, slot *accountSlot) error {
	slot.mu.Lock()
	defer slot.mu.Unlock()

	release, err := r.acquireAccountLock(ctx, prov.Name(), slot.account)
	if err != nil {
		return err
	}
	defer release()

	renewed, err := prov.Refresh(ctx, slot.cred)
	if err != nil {
		if errors.Is(err, provider.ErrReauthRequired) {
			r.markReauthRequired(ctx, prov.Name(), slot.account)
			return fmt.Errorf("refresh account %q: %w", slot.account, err)
		}
		r.logger.ErrorContext(ctx, "oauthrotation.refresh.failed",
			"component", "oauthrotation",
			"provider", string(prov.Name()),
			"account", string(slot.account),
			"err", err.Error(),
		)
		return fmt.Errorf("refresh account %q: %w", slot.account, err)
	}
	r.clearReauthRequired(prov.Name(), slot.account)
	encoded, err := prov.EncodeStored(renewed)
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.refresh.encode_failed",
			"component", "oauthrotation",
			"provider", string(prov.Name()),
			"account", string(slot.account),
			"err", err.Error(),
		)
		return fmt.Errorf("encode refreshed credential for account %q: %w", slot.account, err)
	}
	if err := r.persistCredential(ctx, prov.Name(), slot.account, encoded); err != nil {
		return err
	}
	slot.cred = renewed
	r.logger.InfoContext(ctx, "oauthrotation.account.refreshed",
		"component", "oauthrotation",
		"provider", string(prov.Name()),
		"account", string(slot.account),
		"expires_at_ms", renewed.ExpiresAt.UnixMilli(),
	)
	return nil
}

// persistCredential writes encoded credential bytes into the per-account file.
// It emits a structured event on success so the state-mutation boundary is
// observable.
func (r *Rotator) persistCredential(ctx context.Context, name provider.Name, account provider.AccountID, encoded []byte) error {
	dir := accountDir(name, account)
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.persist.mkdir_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"dir", dir,
			"err", err.Error(),
		)
		return fmt.Errorf("mkdir account dir %s: %w", dir, err)
	}
	credPath := accountCredentialsPath(name, account)
	if err := os.WriteFile(credPath, encoded, credentialFileMode); err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.persist.write_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"path", credPath,
			"err", err.Error(),
		)
		return fmt.Errorf("write credential %s: %w", credPath, err)
	}
	r.logger.DebugContext(ctx, "oauthrotation.persist.written",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
		"path", credPath,
	)
	return nil
}

// acquireAccountLock takes the per-account flock used for refresh and persist.
func (r *Rotator) acquireAccountLock(ctx context.Context, name provider.Name, account provider.AccountID) (func(), error) {
	dir := accountDir(name, account)
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.lock.mkdir_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"dir", dir,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("mkdir account dir %s: %w", dir, err)
	}
	lockPath := accountLockPath(name, account)
	lock := flock.New(lockPath)
	lockCtx, cancel := context.WithTimeout(ctx, refreshLockTimeout)
	got, err := lock.TryLockContext(lockCtx, refreshLockRetry)
	if err != nil {
		cancel()
		r.logger.WarnContext(ctx, "oauthrotation.lock.acquire_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"lock_path", lockPath,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("acquire account lock %s: %w", lockPath, err)
	}
	if !got {
		cancel()
		r.logger.WarnContext(ctx, "oauthrotation.lock.acquire_timeout",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"lock_path", lockPath,
		)
		return nil, fmt.Errorf("acquire account lock %s: timed out", lockPath)
	}
	return func() {
		_ = lock.Unlock()
		cancel()
	}, nil
}

// Load runs the daemon's one-shot startup load for a provider: one upstream
// import pass through the mirror syncer, then a walk of the per-account
// directory on disk to fold any account that exists on disk but was not
// imported by the upstream pass. Both steps fold into the same in-memory slot
// map, and the fold is idempotent on existing ids so a partial run followed
// by a full run remains correct.
//
// Load is gated three ways for a given provider. [sync.Once] guards the
// in-process first call so two callers in the same process trigger at most
// one full pass. A single-flight bool drops a concurrent re-entrant call
// while the first one runs. A debounce window drops a call that lands inside
// loadMinInterval of a prior successful return. The fold logic itself is
// idempotent on state.slots, so a partial run plus a later full run remains
// correct even when the gates drop a caller.
//
// Load returns nil to a caller that was dropped by any gate. It emits an
// info event at the end of every pass that ran with the final in-memory
// account count, the upstream sources read, the upstream imports written,
// and the wall time spent.
func (r *Rotator) Load(ctx context.Context, name provider.Name) error {
	state, err := r.providerState(ctx, name)
	if err != nil {
		return err
	}

	now := r.now()
	state.mu.Lock()
	if state.loadInFlight {
		state.mu.Unlock()
		return nil
	}
	if !state.lastLoadAt.IsZero() && now.Sub(state.lastLoadAt) < loadMinInterval {
		state.mu.Unlock()
		return nil
	}
	state.loadInFlight = true
	state.mu.Unlock()

	defer func() {
		state.mu.Lock()
		state.loadInFlight = false
		state.mu.Unlock()
	}()

	var ranOnce bool
	state.loadOnce.Do(func() {
		r.runStartupLoad(ctx, name, state)
		ranOnce = true
	})
	if !ranOnce {
		// The in-process Once already fired on a prior call. Treat this as a
		// successful no-op and refresh the debounce timestamp so subsequent
		// callers continue to coalesce against the most recent return.
		state.mu.Lock()
		state.lastLoadAt = r.now()
		state.mu.Unlock()
		return nil
	}

	state.mu.Lock()
	state.lastLoadAt = r.now()
	state.mu.Unlock()
	return nil
}

// runStartupLoad performs the actual upstream pass plus on-disk walk for one
// provider. It is the body Load gates; it never enforces gating itself, so a
// test can call it directly when it needs to bypass the once and debounce
// gates.
func (r *Rotator) runStartupLoad(ctx context.Context, name provider.Name, state *providerState) {
	start := r.now()
	syncResult, syncErr := state.syncer.Sync(ctx, r.now())
	if syncErr != nil {
		r.logger.WarnContext(ctx, "oauthrotation.mirror.sync_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", syncErr.Error(),
		)
	}
	for _, account := range syncResult.Imported {
		r.loadImportedAccount(ctx, name, state, account)
	}

	r.foldOnDiskAccounts(ctx, name, state)

	state.mu.Lock()
	accountCount := len(state.order)
	state.mu.Unlock()

	r.logger.InfoContext(ctx, "oauthrotation.startup.loaded",
		"component", "oauthrotation",
		"provider", string(name),
		"accounts", accountCount,
		"sources_read", syncResult.SourcesRead,
		"sources_written", len(syncResult.Imported),
		"duration_ms", r.now().Sub(start).Milliseconds(),
	)
}

// foldOnDiskAccounts walks the per-account directory for a provider and
// folds every entry whose id is not already in state.slots. Entries are
// processed in lexicographic order so the in-memory account order is stable
// across process restarts. A missing accounts directory is a no-op. A
// missing or unparseable per-account credential file emits the
// oauthrotation.startup.account_skipped warn event with the account id and
// the underlying error, then the walk continues to the next entry.
func (r *Rotator) foldOnDiskAccounts(ctx context.Context, name provider.Name, state *providerState) {
	dir := accountsDir(name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		r.logger.WarnContext(ctx, "oauthrotation.startup.accounts_dir_read_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"dir", dir,
			"err", err.Error(),
		)
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		account := provider.AccountID(entry.Name())
		state.mu.Lock()
		_, exists := state.slots[account]
		state.mu.Unlock()
		if exists {
			continue
		}
		credPath := accountCredentialsPath(name, account)
		raw, readErr := os.ReadFile(credPath)
		if readErr != nil {
			r.logger.WarnContext(ctx, "oauthrotation.startup.account_skipped",
				"component", "oauthrotation",
				"provider", string(name),
				"account", string(account),
				"err", readErr.Error(),
			)
			continue
		}
		cred, parseErr := state.prov.ParseStored(raw)
		if parseErr != nil {
			r.logger.WarnContext(ctx, "oauthrotation.startup.account_skipped",
				"component", "oauthrotation",
				"provider", string(name),
				"account", string(account),
				"err", parseErr.Error(),
			)
			continue
		}
		label := r.readAccountLabel(name, account)
		r.foldAccount(state, account, cred, label)
	}
}

// Harvest runs one mirror-import pass for every registered provider, folding
// any newly imported account into its slot map. It is the public entry point
// the daemon refresh loop calls before RefreshAll so freshly added upstream
// accounts are present in memory before the refresh fan-out. A per-provider
// sync error is logged and swallowed so one provider's transient upstream read
// failure does not block the others.
func (r *Rotator) Harvest(ctx context.Context) {
	r.mu.RLock()
	type named struct {
		name  provider.Name
		state *providerState
	}
	states := make([]named, 0, len(r.providers))
	for name, state := range r.providers {
		states = append(states, named{name: name, state: state})
	}
	r.mu.RUnlock()
	for _, entry := range states {
		r.runMirrorSync(ctx, entry.name, entry.state)
	}
}

// markReauthRequired records that an account's refresh credential is dead and
// emits the oauth.login.needed lifecycle event. It logs the provider and
// account only; no token material is included.
func (r *Rotator) markReauthRequired(ctx context.Context, name provider.Name, account provider.AccountID) {
	r.mu.RLock()
	state, ok := r.providers[name]
	r.mu.RUnlock()
	if !ok {
		return
	}
	state.mu.Lock()
	if state.reauth == nil {
		state.reauth = make(map[provider.AccountID]bool)
	}
	state.reauth[account] = true
	state.mu.Unlock()
	r.logger.InfoContext(ctx, "oauth.login.needed",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
	)
}

// clearReauthRequired removes any re-auth mark for an account after a
// successful refresh restores a usable credential.
func (r *Rotator) clearReauthRequired(name provider.Name, account provider.AccountID) {
	r.mu.RLock()
	state, ok := r.providers[name]
	r.mu.RUnlock()
	if !ok {
		return
	}
	state.mu.Lock()
	if state.reauth != nil && state.reauth[account] {
		delete(state.reauth, account)
	}
	state.mu.Unlock()
}

// runMirrorSync runs one import pass and folds any newly imported account into
// the slot map. A sync error is logged and swallowed so a transient upstream
// read failure does not block token selection from already-known accounts.
func (r *Rotator) runMirrorSync(ctx context.Context, name provider.Name, state *providerState) {
	result, err := state.syncer.Sync(ctx, r.now())
	if err != nil {
		r.logger.WarnContext(ctx, "oauthrotation.mirror.sync_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", err.Error(),
		)
		return
	}
	for _, account := range result.Imported {
		r.loadImportedAccount(ctx, name, state, account)
	}
}

// loadImportedAccount reads the just-written per-account credential and folds
// it into the slot map, preserving append-only account order. It is a thin
// wrapper over foldAccount that reads the credential bytes and label from the
// per-account directory the mirror syncer just wrote.
func (r *Rotator) loadImportedAccount(ctx context.Context, name provider.Name, state *providerState, account provider.AccountID) {
	credPath := accountCredentialsPath(name, account)
	raw, err := os.ReadFile(credPath)
	if err != nil {
		r.logger.WarnContext(ctx, "oauthrotation.mirror.read_imported_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"err", err.Error(),
		)
		return
	}
	cred, err := state.prov.ParseStored(raw)
	if err != nil {
		r.logger.WarnContext(ctx, "oauthrotation.mirror.parse_imported_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"err", err.Error(),
		)
		return
	}
	label := r.readAccountLabel(name, account)
	r.foldAccount(state, account, cred, label)
}

// foldAccount folds a parsed credential into state.slots and state.order
// under state.mu, preserving append-only account order. When an account id
// already exists, the slot's cred is updated in place and the label is
// overwritten only when the caller supplies a non-empty label, so a later
// fold never clears an earlier label. The state mutex is taken once.
//
// Parsing happens at the call site so each caller can log a parse failure
// with the source-specific event name (mirror-import vs startup walk). This
// helper only does the state mutation step shared between the two.
func (r *Rotator) foldAccount(state *providerState, account provider.AccountID, cred provider.Credentials, label string) {
	state.mu.Lock()
	slot, exists := state.slots[account]
	if !exists {
		slot = &accountSlot{
			account: account,
			mu:      sync.Mutex{},
			cred:    provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""},
			label:   "",
		}
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

// accountForToken reverse-looks-up the account whose stored credential carries
// the given access token.
func (r *Rotator) accountForToken(state *providerState, accessToken string) (provider.AccountID, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, account := range state.order {
		slot := state.slots[account]
		slot.mu.Lock()
		match := slot.cred.AccessToken == accessToken
		slot.mu.Unlock()
		if match {
			return account, true
		}
	}
	return "", false
}

// providerState resolves a registered provider or returns an error naming it.
func (r *Rotator) providerState(ctx context.Context, name provider.Name) (*providerState, error) {
	r.mu.RLock()
	state, ok := r.providers[name]
	r.mu.RUnlock()
	if !ok {
		r.logger.WarnContext(ctx, "oauthrotation.provider.not_registered",
			"component", "oauthrotation",
			"provider", string(name),
		)
		return nil, fmt.Errorf("provider %q not registered", name)
	}
	return state, nil
}

var _ ratelimitsink.Sink = (*Rotator)(nil)
