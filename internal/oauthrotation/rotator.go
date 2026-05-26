package oauthrotation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"goodkind.io/clyde/internal/oauthrotation/inuse"
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
	// maxObservationFreshness is the upper bound on how long a "rejected"
	// observation may keep an account out of rotation. Without this ceiling a
	// stale observation could block an account for hours the same way the
	// legacy throttle ledger did. With it, the rotator trusts the rejection
	// for at most one minute and the next live request finds out the current
	// truth.
	maxObservationFreshness = 60 * time.Second
	// legacyThrottleFileName is the on-disk name of the retired throttle
	// ledger. The startup loader deletes it (and its proper-lockfile
	// companion) so a daemon that upgraded to the in-memory observation
	// model does not leave stale state on disk.
	legacyThrottleFileName = "throttle.json"
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
	// lastObservedStatus is the most recent rate-limit status the Anthropic
	// HTTP client reported for this slot. quotaUnknown is the zero value;
	// every account starts there after a daemon restart and remains there
	// until the next live response writes truth.
	lastObservedStatus quotaStatus
	// lastObservedAt is when lastObservedStatus was recorded. Selection uses
	// this with maxObservationFreshness to bound how long a rejected
	// observation may keep the slot out of rotation.
	lastObservedAt time.Time
	// lastResetAt is the upstream-reported reset time the last observation
	// carried. Selection skips the slot only while now is before lastResetAt
	// AND within the freshness window.
	lastResetAt time.Time
	// lastClaim is the rate-limit window the last observation referred to
	// (five_hour, seven_day, seven_day_opus, or unknown). It is surfaced in
	// AccountSnapshot.Claim so the CLI and TUI can label the throttle.
	lastClaim string
	// pendingCoordRelease is the cross-process refresh-lock release function
	// the coordinator acquired in coordinateRefresh and that the subsequent
	// runProviderRefresh must run on its own defer. It is set and cleared
	// within a single refreshSlot call and never accessed concurrently
	// because refreshSlot holds the slot mutex for the duration.
	pendingCoordRelease func() error
}

// newAccountSlot returns a zero-valued slot keyed to the given account. Every
// callsite that constructs an accountSlot literal goes through this helper so
// adding a new field cannot leave a slot partially initialized.
func newAccountSlot(account provider.AccountID) *accountSlot {
	return &accountSlot{
		account:             account,
		mu:                  sync.Mutex{},
		cred:                provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""},
		label:               "",
		lastObservedStatus:  quotaUnknown,
		lastObservedAt:      time.Time{},
		lastResetAt:         time.Time{},
		lastClaim:           "",
		pendingCoordRelease: nil,
	}
}

// providerState groups everything the rotator tracks for one registered
// provider: the provider implementation, its ordered account slots, and a
// one-way mirror syncer. The in-memory quota observations live on each slot;
// nothing is persisted to disk for the throttle state.
type providerState struct {
	prov   provider.Provider
	mu     sync.Mutex
	slots  map[provider.AccountID]*accountSlot
	order  []provider.AccountID
	syncer *mirror.Syncer
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
// credentials one-way, refreshes tokens, and records the most recent
// rate-limit observation per account. It implements [ratelimitsink.Sink].
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

// inUseSet returns the current in-use set for one provider by looking up the
// process-wide detector registry and calling Detect with the supplied account
// order. A registry miss returns the noop detector, which yields an empty
// set. A Detect error is logged and treated as "nothing in use" so a transient
// detection failure never blocks a refresh.
func (r *Rotator) inUseSet(ctx context.Context, name provider.Name, accounts []provider.AccountID) inuse.Set {
	detector := inuse.Lookup(name)
	set, err := detector.Detect(ctx, accounts)
	if err != nil {
		r.logger.WarnContext(ctx, "oauthrotation.inuse.detect_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"err", err.Error(),
		)
		return inuse.NewSet()
	}
	return set
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

// Token runs one mirror-sync pass, walks the provider's accounts in a stable
// order, and returns the first account whose last observation is not a fresh
// rejection along with its access token. When every account is rejected
// inside the freshness window it returns AllAccountsThrottledError with the
// soonest reset.
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

// selectActiveSlot runs the same selection Token uses: one mirror-sync pass,
// then a stable walk over the provider's accounts that skips re-auth-marked
// slots and slots whose last observation is a fresh rejection still inside
// the freshness window. It returns the selected slot so callers can read
// either the access token (Token) or the stored credential encoding
// (SelectForLaunch) without duplicating selection semantics. When every
// account is fresh-rejected it returns AllAccountsThrottledError with the
// soonest reset.
func (r *Rotator) selectActiveSlot(ctx context.Context, name provider.Name) (*accountSlot, provider.AccountID, error) {
	state, err := r.providerState(ctx, name)
	if err != nil {
		return nil, "", err
	}

	r.runMirrorSync(ctx, name, state)

	now := r.now()

	state.mu.Lock()
	order := append([]provider.AccountID(nil), state.order...)
	slots := make(map[provider.AccountID]*accountSlot, len(state.slots))
	maps.Copy(slots, state.slots)
	reauth := make(map[provider.AccountID]bool, len(state.reauth))
	maps.Copy(reauth, state.reauth)
	state.mu.Unlock()

	eligibles, blocked := partitionEligibleSlots(order, slots, reauth, now)
	if len(eligibles) > 0 {
		chosen := pickEligibleSlot(eligibles)
		return chosen.slot, chosen.account, nil
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
	if blocked.reauthSeen {
		r.logger.WarnContext(ctx, "oauthrotation.token.needs_reauth",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(blocked.reauthAccount),
		)
		return nil, "", NeedsReauthError{
			Provider: name,
			Account:  blocked.reauthAccount,
		}
	}
	r.logger.WarnContext(ctx, "oauthrotation.token.all_throttled",
		"component", "oauthrotation",
		"provider", string(name),
		"soonest_account", string(blocked.soonestAccount),
		"soonest_reset_ms", blocked.soonestReset.UnixMilli(),
	)
	return nil, "", AllAccountsThrottledError{
		Provider:     name,
		SoonestReset: blocked.soonestReset,
		Account:      blocked.soonestAccount,
	}
}

// eligibleSlot pairs an account that can serve right now with the observation
// timestamps the soonest-reset selector compares.
type eligibleSlot struct {
	account        provider.AccountID
	slot           *accountSlot
	resetAt        time.Time
	observedAt     time.Time
	resetUnknown   bool
	observedAtZero bool
}

// blockedSlots tracks the worst-case fallback selection inputs: the first
// reauth-marked account encountered (its remedy is actionable) and the
// throttled account with the soonest upstream reset (its window clears first).
type blockedSlots struct {
	soonestSet     bool
	soonestReset   time.Time
	soonestAccount provider.AccountID
	reauthSeen     bool
	reauthAccount  provider.AccountID
}

// partitionEligibleSlots walks the account order in stable order and returns
// the slots that can serve right now along with the worst-case fallback
// fields the caller surfaces when no slot is eligible. A reauth-marked slot
// is dropped entirely. A throttled slot contributes to the soonest-reset
// fallback through its unclamped upstream reset.
func partitionEligibleSlots(order []provider.AccountID, slots map[provider.AccountID]*accountSlot, reauth map[provider.AccountID]bool, now time.Time) ([]eligibleSlot, blockedSlots) {
	var (
		eligibles []eligibleSlot
		blocked   blockedSlots
	)
	for _, account := range order {
		if reauth[account] {
			if !blocked.reauthSeen {
				blocked.reauthSeen = true
				blocked.reauthAccount = account
			}
			continue
		}
		slot := slots[account]
		throttled, _ := slotIsFreshRejected(slot, now)
		if !throttled {
			slot.mu.Lock()
			resetAt := slot.lastResetAt
			observedAt := slot.lastObservedAt
			slot.mu.Unlock()
			resetUnknown := resetAt.IsZero() || !resetAt.After(now)
			eligibles = append(eligibles, eligibleSlot{
				account:        account,
				slot:           slot,
				resetAt:        resetAt,
				observedAt:     observedAt,
				resetUnknown:   resetUnknown,
				observedAtZero: observedAt.IsZero(),
			})
			continue
		}
		// Use the unclamped upstream reset for the soonest-reset comparison so
		// the typed error still surfaces the earlier real recovery window when
		// two slots share the same observation freshness cap.
		slot.mu.Lock()
		resetAt := slot.lastResetAt
		slot.mu.Unlock()
		if !blocked.soonestSet || resetAt.Before(blocked.soonestReset) {
			blocked.soonestSet = true
			blocked.soonestReset = resetAt
			blocked.soonestAccount = account
		}
	}
	return eligibles, blocked
}

// pickEligibleSlot orders the eligible slots by soonest upstream reset and
// returns the front of the order. A stable sort keeps the original account
// order as the final tie-break so a fresh rotator with no observations still
// selects the first registered account, matching the historical contract.
func pickEligibleSlot(eligibles []eligibleSlot) eligibleSlot {
	sort.SliceStable(eligibles, func(i, j int) bool {
		a, b := eligibles[i], eligibles[j]
		// Accounts with no recorded reset sort after accounts with one.
		if a.resetUnknown != b.resetUnknown {
			return !a.resetUnknown
		}
		// When both have a recorded reset, prefer the soonest in the future.
		if !a.resetUnknown && !b.resetUnknown && !a.resetAt.Equal(b.resetAt) {
			return a.resetAt.Before(b.resetAt)
		}
		// Tie-break by oldest lastObservedAt so unused accounts get a turn.
		// A zero observedAt (never observed) sorts after any real timestamp.
		if a.observedAtZero != b.observedAtZero {
			return !a.observedAtZero
		}
		if !a.observedAt.Equal(b.observedAt) {
			return a.observedAt.Before(b.observedAt)
		}
		return false
	})
	return eligibles[0]
}

// TokenAfterAuthFailure recovers from an upstream 401 by returning a fresh
// access token for the same account Token would currently pick. The caller
// passes the access token that just failed so the rotator can decide whether
// a recovery would actually produce a different value. Two paths are taken,
// depending on whether the account is currently in use by an external Claude
// Code session.
//
// For an in-use account, the rotator runs one mirror-sync pass to re-import
// whatever credential the keychain holds today, then re-reads the slot's
// access token. When the freshly imported token equals failedToken byte for
// byte, the rotator returns the same token plus ErrTokenUnchanged so the
// client skips a pointless retry. Otherwise the new token is returned.
//
// For a non-in-use account, the rotator takes the per-account flock and runs
// the same refresh path RefreshDue uses; the resulting token is returned. A
// refresh failure surfaces verbatim so the client can fall through to its
// existing error classification.
func (r *Rotator) TokenAfterAuthFailure(ctx context.Context, name provider.Name, failedToken string) (string, error) {
	slot, account, err := r.selectActiveSlot(ctx, name)
	if err != nil {
		return "", err
	}
	state, err := r.providerState(ctx, name)
	if err != nil {
		return "", err
	}

	inUse := r.inUseSet(ctx, name, []provider.AccountID{account})
	if inUse.Contains(account) {
		// Re-import the live keychain credential through the same syncer the
		// startup load runs. A fresh import lands on disk and folds into the
		// slot in place, so the next Token call sees the updated value.
		r.runMirrorSync(ctx, name, state)
		slot.mu.Lock()
		token := slot.cred.AccessToken
		slot.mu.Unlock()
		if token == failedToken {
			r.logger.DebugContext(ctx, "oauthrotation.recover.token_unchanged",
				"component", "oauthrotation",
				"provider", string(name),
				"account", string(account),
			)
			return token, ErrTokenUnchanged
		}
		r.logger.InfoContext(ctx, "oauthrotation.recover.synced_from_keychain",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
		)
		return token, nil
	}

	if err := r.refreshSlot(ctx, state.prov, slot); err != nil {
		return "", err
	}
	slot.mu.Lock()
	token := slot.cred.AccessToken
	slot.mu.Unlock()
	if token == failedToken {
		// A successful refresh that produced the same token is degenerate;
		// surface ErrTokenUnchanged so the client does not loop on it.
		return token, ErrTokenUnchanged
	}
	r.logger.InfoContext(ctx, "oauthrotation.recover.refreshed",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
	)
	return token, nil
}

// Observe implements [ratelimitsink.Sink]. It reverse-looks-up the account
// slot by access token and records the signal's status, claim, reset time,
// and observation timestamp into the slot's in-memory observation fields.
// Observe runs on every upstream response, not only on 429: a 200 with
// status=allowed clears any stale rejection on the slot, and a 429 with
// status=rejected makes the slot ineligible for the next selection pass for
// up to maxObservationFreshness.
func (r *Rotator) Observe(ctx context.Context, sig ratelimitsink.Signal) error {
	name := provider.Name(sig.Provider)
	state, err := r.providerState(ctx, name)
	if err != nil {
		return err
	}

	account, found := r.accountForToken(state, sig.AccessToken)
	if !found {
		r.logger.WarnContext(ctx, "oauthrotation.observe.unknown_token",
			"component", "oauthrotation",
			"provider", string(name),
			"http_status", sig.HTTPStatus,
		)
		return fmt.Errorf("observe signal for unknown access token on provider %q", name)
	}

	state.mu.Lock()
	slot, ok := state.slots[account]
	state.mu.Unlock()
	if !ok {
		return fmt.Errorf("observe signal for missing slot on provider %q", name)
	}

	status := quotaFromSink(sig.Status)
	observedAt := sig.ObservedAt
	if observedAt.IsZero() {
		observedAt = r.now()
	}

	slot.mu.Lock()
	slot.lastObservedStatus = status
	slot.lastObservedAt = observedAt
	slot.lastResetAt = sig.ResetAt
	slot.lastClaim = string(sig.Claim)
	slot.mu.Unlock()

	r.logger.InfoContext(ctx, "oauthrotation.account.observed",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
		"status", status.String(),
		"claim", string(sig.Claim),
		"http_status", sig.HTTPStatus,
		"reset_at_ms", sig.ResetAt.UnixMilli(),
	)
	return nil
}

// quotaFromSink maps the typed sink status to the rotator's internal
// quotaStatus enum. An unrecognized value is treated as quotaUnknown so the
// rotator never invents a state Anthropic did not report.
func quotaFromSink(status ratelimitsink.Status) quotaStatus {
	switch status {
	case ratelimitsink.StatusAllowed:
		return quotaAllowed
	case ratelimitsink.StatusAllowedWarning:
		return quotaAllowedWarning
	case ratelimitsink.StatusRejected:
		return quotaRejected
	case ratelimitsink.StatusUnknown:
		return quotaUnknown
	default:
		return quotaUnknown
	}
}

// RefreshAll fans a refresh out over every slot of every provider, persisting
// each renewed credential through the provider's EncodeStored under a
// per-account file lock. Every account is refreshed, including any account
// currently in use by an external Claude Code session: the per-provider
// cross-process refresh lock (when the provider implements
// [provider.RefreshCoordinator]) serializes Clyde's refresh against Claude
// Code's, and the re-read step inside refreshSlot absorbs a fresher upstream
// credential without re-calling the token endpoint. RefreshAll returns the
// first error encountered after attempting every slot.
func (r *Rotator) RefreshAll(ctx context.Context) error {
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
			if err := r.refreshSlot(ctx, prov, slot); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// RefreshDue refreshes only the accounts whose stored credential is at or
// inside the safety window, leaving still-valid accounts untouched. The
// daemon refresh loop calls it on every tick and at startup so a reload of a
// healthy fleet mints zero new tokens, while a token approaching expiry is
// still renewed ahead of time. The due decision reads each slot's persisted
// ExpiresAt (loaded from the account's .credentials.json on Harvest), so the
// persisted expiry is the source of truth across restarts.
//
// Every due account is refreshed, including any account currently in use by
// an external Claude Code session. The cross-process refresh lock (acquired
// inside refreshSlot for providers implementing
// [provider.RefreshCoordinator]) serializes Clyde's refresh against Claude
// Code's. RefreshDue returns the first error encountered after attempting
// every due slot.
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

// slotIsFreshRejected reports whether selection should skip the slot because
// the last observation is a rejection that is both still inside the freshness
// window and still before its upstream-reported reset. It also returns the
// effective reset time (capped at lastObservedAt + maxObservationFreshness)
// so the caller can surface "skipped until X" without duplicating the cap
// logic. A slot that is not rejected returns (false, zero).
func slotIsFreshRejected(slot *accountSlot, now time.Time) (bool, time.Time) {
	if slot == nil {
		return false, time.Time{}
	}
	slot.mu.Lock()
	status := slot.lastObservedStatus
	observedAt := slot.lastObservedAt
	resetAt := slot.lastResetAt
	slot.mu.Unlock()
	if status != quotaRejected {
		return false, time.Time{}
	}
	if observedAt.IsZero() {
		return false, time.Time{}
	}
	if now.Sub(observedAt) >= maxObservationFreshness {
		return false, time.Time{}
	}
	if !now.Before(resetAt) {
		return false, time.Time{}
	}
	freshnessCap := observedAt.Add(maxObservationFreshness)
	effective := resetAt
	if freshnessCap.Before(effective) {
		effective = freshnessCap
	}
	return true, effective
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
// The throttle fields (Throttled, ThrottledTo, Claim) reflect the in-memory
// observation state of each slot, gated by the same freshness rule selection
// uses; an account whose last rejection is older than maxObservationFreshness
// reads as not throttled even when the upstream-reported reset time is still
// in the future.
func (r *Rotator) Accounts(ctx context.Context, name provider.Name) ([]AccountSnapshot, error) {
	state, err := r.providerState(ctx, name)
	if err != nil {
		return nil, err
	}
	now := r.now()
	state.mu.Lock()
	defer state.mu.Unlock()
	snapshots := make([]AccountSnapshot, 0, len(state.order))
	for _, account := range state.order {
		slot := state.slots[account]
		slot.mu.Lock()
		cred := slot.cred
		label := slot.label
		claim := slot.lastClaim
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
		if throttled, until := slotIsFreshRejected(slot, now); throttled {
			snapshot.Throttled = true
			snapshot.ThrottledTo = until
			snapshot.Claim = claim
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
// persists the renewed credential. When the provider implements
// [provider.RefreshCoordinator] the slot is wrapped in the provider's
// cross-process refresh lock and the upstream source of truth is re-read
// inside the lock: when the upstream copy is fresher than the slot's current
// credential, the rotator absorbs that copy and skips the token-endpoint
// call entirely. Concurrent Token callers do not refresh, but RefreshAll
// shares this lock so only one refresh per account runs at a time.
func (r *Rotator) refreshSlot(ctx context.Context, prov provider.Provider, slot *accountSlot) error {
	slot.mu.Lock()
	defer slot.mu.Unlock()

	release, err := r.acquireAccountLock(ctx, prov.Name(), slot.account)
	if err != nil {
		return err
	}
	defer release()

	coord, hasCoord := prov.(provider.RefreshCoordinator)
	if hasCoord {
		absorbed, coordErr := r.coordinateRefresh(ctx, coord, prov, slot)
		if coordErr != nil {
			return coordErr
		}
		if absorbed {
			return nil
		}
	}

	return r.runProviderRefresh(ctx, prov, slot, hasCoord, coord)
}

// coordinateRefresh acquires the cross-process refresh lock and absorbs a
// fresher upstream credential when one is available, returning absorbed=true
// in that case so the caller skips the token-endpoint call. When the
// upstream copy is not fresher (or the upstream read fails non-fatally), it
// returns absorbed=false with the lock held: the caller's deferred release
// path must release the lock. To keep the deferred release simple this
// helper does NOT defer the release itself; instead it returns a release
// function on the slot's mutex deferral path.
//
// The implementation: take the lock; if absorbed, persist+release; if not
// absorbed, register the release on the slot's per-call defer chain by
// piggy-backing through the closure stored on the rotator.
func (r *Rotator) coordinateRefresh(ctx context.Context, coord provider.RefreshCoordinator, prov provider.Provider, slot *accountSlot) (bool, error) {
	coordRelease, err := coord.AcquireRefreshLock(ctx)
	if err != nil {
		r.logger.WarnContext(ctx, "oauthrotation.refresh.coord_lock_failed",
			"component", "oauthrotation",
			"provider", string(prov.Name()),
			"account", string(slot.account),
			"err", err.Error(),
		)
		return false, fmt.Errorf("acquire refresh coord lock for account %q: %w", slot.account, err)
	}

	upstream, present, readErr := coord.ReadUpstreamCredentials(ctx)
	if readErr == nil && present && upstream.ExpiresAt.After(slot.cred.ExpiresAt) {
		// Claude Code (or some other peer holding the same keychain entry)
		// already produced a fresher credential during our wait. Absorb it
		// into the slot and persist the imported bytes; skip the token-
		// endpoint call entirely so we do not invalidate the peer's freshly
		// minted credential by minting another.
		persistErr := r.absorbUpstream(ctx, prov, slot, upstream)
		r.releaseCoordLock(ctx, prov, slot.account, coordRelease)
		if persistErr != nil {
			return false, persistErr
		}
		r.logger.InfoContext(ctx, "oauthrotation.refresh.absorbed_upstream",
			"component", "oauthrotation",
			"provider", string(prov.Name()),
			"account", string(slot.account),
			"expires_at_ms", upstream.ExpiresAt.UnixMilli(),
		)
		return true, nil
	}
	if readErr != nil {
		// A keychain read failure is not fatal: fall through to the
		// existing refresh path so a transient read does not block the
		// refresh tick. The lock is still held so the actual refresh
		// remains serialized against the peer.
		r.logger.WarnContext(ctx, "oauthrotation.refresh.upstream_read_failed",
			"component", "oauthrotation",
			"provider", string(prov.Name()),
			"account", string(slot.account),
			"err", readErr.Error(),
		)
	}
	// The slot's caller is responsible for releasing through
	// runProviderRefresh which is called with hasCoord=true and the same
	// coord reference; we store the release here on a per-slot field for
	// pickup. To keep this implementation straightforward, we instead defer
	// the release on the goroutine via a closure: the runProviderRefresh
	// helper receives the release and runs it on its own defer.
	slot.pendingCoordRelease = coordRelease
	return false, nil
}

// releaseCoordLock runs the coordinator release and logs any failure. The
// helper exists so callers can keep their happy path linear.
func (r *Rotator) releaseCoordLock(ctx context.Context, prov provider.Provider, account provider.AccountID, release func() error) {
	if release == nil {
		return
	}
	if releaseErr := release(); releaseErr != nil {
		r.logger.WarnContext(ctx, "oauthrotation.refresh.coord_release_failed",
			"component", "oauthrotation",
			"provider", string(prov.Name()),
			"account", string(account),
			"err", releaseErr.Error(),
		)
	}
}

// runProviderRefresh performs the actual upstream token-endpoint exchange
// and persists the renewed credential. When hasCoord is true the helper also
// writes the renewed credential back to the cross-process source of truth
// and releases the coordinator lock the caller acquired.
func (r *Rotator) runProviderRefresh(ctx context.Context, prov provider.Provider, slot *accountSlot, hasCoord bool, coord provider.RefreshCoordinator) error {
	if hasCoord {
		defer r.releaseCoordLock(ctx, prov, slot.account, slot.pendingCoordRelease)
		defer func() { slot.pendingCoordRelease = nil }()
	}

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
	if hasCoord {
		if writeErr := coord.WriteUpstreamCredentials(ctx, renewed); writeErr != nil {
			// Surface the write failure but do not unwind the local persist;
			// Clyde's per-account copy is the canonical one for the next
			// rotation pass, and Claude Code will pull from disk on its next
			// read if the keychain write missed.
			r.logger.WarnContext(ctx, "oauthrotation.refresh.upstream_write_failed",
				"component", "oauthrotation",
				"provider", string(prov.Name()),
				"account", string(slot.account),
				"err", writeErr.Error(),
			)
		}
	}
	r.logger.InfoContext(ctx, "oauthrotation.account.refreshed",
		"component", "oauthrotation",
		"provider", string(prov.Name()),
		"account", string(slot.account),
		"expires_at_ms", renewed.ExpiresAt.UnixMilli(),
	)
	return nil
}

// absorbUpstream persists a credential that came from the cross-process
// upstream source (for example Claude Code's keychain refresh) into the
// per-account .credentials.json and replaces the slot's in-memory credential
// with it. The caller must hold both the per-account file lock and the slot
// mutex. The cross-process refresh lock should also be held so a peer cannot
// race with the persist.
func (r *Rotator) absorbUpstream(ctx context.Context, prov provider.Provider, slot *accountSlot, upstream provider.Credentials) error {
	encoded := upstream.Raw
	if len(encoded) == 0 {
		var encodeErr error
		encoded, encodeErr = prov.EncodeStored(upstream)
		if encodeErr != nil {
			r.logger.ErrorContext(ctx, "oauthrotation.refresh.absorb_encode_failed",
				"component", "oauthrotation",
				"provider", string(prov.Name()),
				"account", string(slot.account),
				"err", encodeErr.Error(),
			)
			return fmt.Errorf("encode absorbed credential for account %q: %w", slot.account, encodeErr)
		}
	}
	if err := r.persistCredential(ctx, prov.Name(), slot.account, encoded); err != nil {
		return err
	}
	slot.cred = upstream
	r.clearReauthRequired(prov.Name(), slot.account)
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
// gates. As a side effect it removes the legacy throttle.json file (and its
// proper-lockfile companion) so a daemon that upgraded to the in-memory
// observation model does not leave stale on-disk state.
func (r *Rotator) runStartupLoad(ctx context.Context, name provider.Name, state *providerState) {
	r.removeLegacyThrottleFile(ctx, name)
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

// removeLegacyThrottleFile deletes the retired throttle.json and its
// proper-lockfile companion from the per-provider directory. The on-disk
// throttle ledger was replaced by in-memory observation state on each slot,
// so the file is dead weight after the first daemon start on the new code
// and would otherwise sit there indefinitely. Missing files are a no-op so
// the function is safe to call on every startup.
func (r *Rotator) removeLegacyThrottleFile(ctx context.Context, name provider.Name) {
	dir := providerDir(name)
	candidates := []string{
		filepath.Join(dir, legacyThrottleFileName),
		filepath.Join(dir, legacyThrottleFileName+".lock"),
	}
	var removed []string
	for _, path := range candidates {
		err := os.Remove(path)
		if err == nil {
			removed = append(removed, path)
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		r.logger.WarnContext(ctx, "oauthrotation.throttle.legacy_file_remove_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"path", path,
			"err", err.Error(),
		)
	}
	if len(removed) > 0 {
		r.logger.InfoContext(ctx, "oauthrotation.throttle.legacy_file_removed",
			"component", "oauthrotation",
			"provider", string(name),
			"paths", removed,
		)
	}
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
//
// The keychain harvest stays even though upstream requests no longer
// use the keychain bearer. Claude Code's /login command writes its
// result to the keychain on each interactive login. The harvest
// absorbs those tokens into the rotator so a reflex `claude /login`
// does not leave its credential stranded outside the managed pool.
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
