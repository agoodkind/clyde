package oauthrotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

// fakeProvider is a provider.Provider test double. It derives the account from
// a stored JSON encoding and counts Refresh calls per access token so tests can
// assert single-flight behavior.
type fakeProvider struct {
	name         provider.Name
	mirror       []provider.MirrorSource
	mirrorErr    error
	refreshDelay time.Duration
	refreshCount map[string]*int64
	// refreshErr, when set, is returned by Refresh for any account so tests can
	// exercise the reauth-required path.
	refreshErr error
	mu         sync.Mutex
}

type fakeStored struct {
	Account      string `json:"account"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
	Fingerprint  string `json:"fingerprint"`
}

func newFakeProvider(name provider.Name) *fakeProvider {
	return &fakeProvider{
		name:         name,
		mirror:       nil,
		mirrorErr:    nil,
		refreshDelay: 0,
		refreshCount: make(map[string]*int64),
		refreshErr:   nil,
		mu:           sync.Mutex{},
	}
}

func (f *fakeProvider) Name() provider.Name { return f.name }

func (f *fakeProvider) AccountIdentity(_ context.Context, accessToken string) (provider.AccountIdentity, error) {
	account, err := f.accountFromToken(accessToken)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	return provider.AccountIdentity{Account: account, Label: ""}, nil
}

// accountFromToken decodes the account the fake encodes into the token as
// "<account>:token". It backs both AccountIdentity and EncodeStored.
func (f *fakeProvider) accountFromToken(accessToken string) (provider.AccountID, error) {
	for i := 0; i < len(accessToken); i++ {
		if accessToken[i] == ':' {
			return provider.AccountID(accessToken[:i]), nil
		}
	}
	if accessToken == "" {
		return "", errors.New("empty access token")
	}
	return provider.AccountID(accessToken), nil
}

func (f *fakeProvider) Refresh(_ context.Context, current provider.Credentials) (provider.Credentials, error) {
	f.mu.Lock()
	refreshErr := f.refreshErr
	f.mu.Unlock()
	if refreshErr != nil {
		return provider.Credentials{}, refreshErr
	}
	f.mu.Lock()
	counter, ok := f.refreshCount[current.AccessToken]
	if !ok {
		counter = new(int64)
		f.refreshCount[current.AccessToken] = counter
	}
	delay := f.refreshDelay
	f.mu.Unlock()
	atomic.AddInt64(counter, 1)
	if delay > 0 {
		time.Sleep(delay)
	}
	renewed := current
	renewed.AccessToken = current.AccessToken + "+refreshed"
	renewed.ExpiresAt = current.ExpiresAt.Add(time.Hour)
	renewed.Fingerprint = current.Fingerprint + "+r"
	return renewed, nil
}

func (f *fakeProvider) MirrorSources(_ context.Context) ([]provider.MirrorSource, error) {
	if f.mirrorErr != nil {
		return nil, f.mirrorErr
	}
	return f.mirror, nil
}

// ReadMirror reads a file-backed mirror source by reading the path and parsing
// it, mirroring how the real Anthropic provider reads a "file" source. A
// missing file returns present=false with no error.
func (f *fakeProvider) ReadMirror(_ context.Context, src provider.MirrorSource) (provider.Credentials, bool, error) {
	raw, err := os.ReadFile(src.Location)
	if err != nil {
		if os.IsNotExist(err) {
			return provider.Credentials{}, false, nil
		}
		return provider.Credentials{}, false, err
	}
	cred, err := f.ParseStored(raw)
	if err != nil {
		return provider.Credentials{}, false, err
	}
	return cred, true, nil
}

func (f *fakeProvider) SpawnLogin(_ context.Context, _ provider.LoginOptions) (provider.LoginSession, error) {
	return provider.LoginSession{Handle: "", AuthorizeURL: "", ScratchDir: ""}, errors.New("not implemented")
}

func (f *fakeProvider) CompleteLogin(_ context.Context, _ provider.LoginSession) (provider.Credentials, error) {
	return provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}, errors.New("not implemented")
}

func (f *fakeProvider) ParseStored(raw []byte) (provider.Credentials, error) {
	var stored fakeStored
	if err := json.Unmarshal(raw, &stored); err != nil {
		return provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}, err
	}
	return provider.Credentials{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		ExpiresAt:    time.UnixMilli(stored.ExpiresAtMS),
		Raw:          raw,
		Fingerprint:  stored.Fingerprint,
	}, nil
}

func (f *fakeProvider) EncodeStored(c provider.Credentials) ([]byte, error) {
	account, err := f.accountFromToken(c.AccessToken)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fakeStored{
		Account:      string(account),
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		ExpiresAtMS:  c.ExpiresAt.UnixMilli(),
		Fingerprint:  c.Fingerprint,
	})
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func credFor(account string, expiresAt time.Time) provider.Credentials {
	return provider.Credentials{
		AccessToken:  account + ":token",
		RefreshToken: account + ":refresh",
		ExpiresAt:    expiresAt,
		Raw:          nil,
		Fingerprint:  account + "-fp",
	}
}

// seedRotator builds a rotator scoped to a temp state dir, registers one fake
// provider whose mirror sources are per-account upstream files, and harvests
// them so each account lands in a slot. Seeding through the production harvest
// path keeps the rotator's only slot-loading entry points exercised.
func seedRotator(t *testing.T, now time.Time, accounts ...string) (*Rotator, *fakeProvider) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	upstreamDir := t.TempDir()
	prov := newFakeProvider("anthropic")
	for _, account := range accounts {
		path := filepath.Join(upstreamDir, account+".json")
		cred := credFor(account, now.Add(time.Hour))
		encoded, err := prov.EncodeStored(cred)
		if err != nil {
			t.Fatalf("EncodeStored(%q): %v", account, err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatalf("write upstream %q: %v", account, err)
		}
		prov.mirror = append(prov.mirror, provider.MirrorSource{Kind: provider.MirrorSourceKindFile, Location: path})
	}
	rot := NewRotator(nil)
	rot.now = fixedClock(now)
	rot.Register(prov)
	rot.Harvest(context.Background())
	return rot, prov
}

func TestRotatorToken(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	hour := int64(time.Hour / time.Millisecond)

	type throttle struct {
		account string
		untilMS int64
	}
	tests := []struct {
		name        string
		accounts    []string
		throttles   []throttle
		wantAccount provider.AccountID
		wantErrAll  bool
		wantSoonest provider.AccountID
	}{
		{
			name:        "ready account picked",
			accounts:    []string{"acct-a", "acct-b"},
			throttles:   nil,
			wantAccount: "acct-a",
			wantErrAll:  false,
			wantSoonest: "",
		},
		{
			name:        "throttled first skips to second",
			accounts:    []string{"acct-a", "acct-b"},
			throttles:   []throttle{{account: "acct-a", untilMS: now.UnixMilli() + hour}},
			wantAccount: "acct-b",
			wantErrAll:  false,
			wantSoonest: "",
		},
		{
			name:     "all throttled returns typed error with soonest reset",
			accounts: []string{"acct-a", "acct-b"},
			throttles: []throttle{
				{account: "acct-a", untilMS: now.UnixMilli() + 2*hour},
				{account: "acct-b", untilMS: now.UnixMilli() + hour},
			},
			wantAccount: "",
			wantErrAll:  true,
			wantSoonest: "acct-b",
		},
		{
			name:     "expired throttle entries are ignored",
			accounts: []string{"acct-a", "acct-b"},
			throttles: []throttle{
				{account: "acct-a", untilMS: now.UnixMilli() - hour},
			},
			wantAccount: "acct-a",
			wantErrAll:  false,
			wantSoonest: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rot, prov := seedRotator(t, now, tc.accounts...)
			for _, th := range tc.throttles {
				store := newThrottleStore(prov.Name(), nil)
				entry := throttleEntry{UntilMS: th.untilMS, ObservedMS: now.UnixMilli(), Claim: string(ratelimitsink.ClaimFiveHour), HTTPStatus: 429}
				if err := store.put(ctx, now, provider.AccountID(th.account), entry); err != nil {
					t.Fatalf("seed throttle: %v", err)
				}
			}

			token, account, err := rot.Token(ctx, prov.Name())
			if tc.wantErrAll {
				var typed AllAccountsThrottledError
				if !errors.As(err, &typed) {
					t.Fatalf("want AllAccountsThrottledError, got %v", err)
				}
				if typed.Account != tc.wantSoonest {
					t.Fatalf("soonest account = %q, want %q", typed.Account, tc.wantSoonest)
				}
				if typed.SoonestReset.IsZero() {
					t.Fatalf("soonest reset is zero")
				}
				return
			}
			if err != nil {
				t.Fatalf("Token: %v", err)
			}
			if account != tc.wantAccount {
				t.Fatalf("account = %q, want %q", account, tc.wantAccount)
			}
			wantToken := string(tc.wantAccount) + ":token"
			if token != wantToken {
				t.Fatalf("token = %q, want %q", token, wantToken)
			}
		})
	}
}

func TestRotatorThrottleReverseLookup(t *testing.T) {
	now := time.UnixMilli(2_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	sig := ratelimitsink.Signal{
		Provider:    string(prov.Name()),
		AccessToken: "acct-b:token",
		Claim:       ratelimitsink.ClaimSevenDay,
		ResetAt:     now.Add(time.Hour),
		ObservedAt:  now,
		HTTPStatus:  429,
	}
	if err := rot.Throttle(ctx, sig); err != nil {
		t.Fatalf("Throttle: %v", err)
	}

	// acct-b is now throttled, so Token must pick acct-a.
	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token after throttle: %v", err)
	}
	if account != "acct-a" {
		t.Fatalf("account = %q, want acct-a", account)
	}

	// An unknown token must not throttle any slot.
	unknown := sig
	unknown.AccessToken = "ghost:token"
	if err := rot.Throttle(ctx, unknown); err == nil {
		t.Fatalf("Throttle for unknown token: want error, got nil")
	}
}

func TestRotatorConcurrentTokenSharesOneRefresh(t *testing.T) {
	now := time.UnixMilli(3_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a")
	prov.mu.Lock()
	prov.refreshDelay = 20 * time.Millisecond
	prov.mu.Unlock()

	state, err := rot.providerState(ctx, prov.Name())
	if err != nil {
		t.Fatalf("providerState: %v", err)
	}
	slot := state.slots["acct-a"]

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_ = rot.refreshSlot(ctx, prov, slot)
		}()
	}
	wg.Wait()

	// The slot mutex serializes refresh, so the access token only refreshes
	// from its original value once; subsequent callers see the renewed token.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	original := prov.refreshCount["acct-a:token"]
	if original == nil || atomic.LoadInt64(original) != 1 {
		t.Fatalf("original token refreshed %v times, want exactly 1", original)
	}
}

func TestRotatorRefreshAll(t *testing.T) {
	now := time.UnixMilli(4_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	if err := rot.RefreshAll(ctx); err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}

	snapshots, err := rot.Accounts(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(snapshots))
	}
	for _, snap := range snapshots {
		if !snap.ExpiresAt.Equal(now.Add(2 * time.Hour)) {
			t.Fatalf("account %q expiry = %s, want refreshed +2h", snap.Account, snap.ExpiresAt)
		}
	}
}

func TestRotatorReauthRequiredExcludesAccount(t *testing.T) {
	now := time.UnixMilli(5_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	// acct-a's refresh fails with a reauth-required error; acct-b refreshes fine.
	// RefreshAll attempts every slot, so set the error only while acct-a is the
	// first slot, then clear it before acct-b. Simpler: fail all, assert both
	// excluded; here we fail only the whole provider and verify selection skips.
	prov.mu.Lock()
	prov.refreshErr = fmt.Errorf("dead: %w", provider.ErrReauthRequired)
	prov.mu.Unlock()

	// RefreshAll returns the first error but still attempts every slot, marking
	// both accounts as needing re-auth.
	if err := rot.RefreshAll(ctx); err == nil {
		t.Fatal("RefreshAll: want error, got nil")
	}

	// With both accounts marked and none throttled, Token must report the
	// re-auth outcome with a typed NeedsReauthError naming the first marked
	// account so the client can surface the actionable login instruction.
	_, _, err := rot.Token(ctx, prov.Name())
	if err == nil {
		t.Fatal("Token: want error after both accounts marked reauth-required, got nil")
	}
	var reauthErr NeedsReauthError
	if !errors.As(err, &reauthErr) {
		t.Fatalf("want NeedsReauthError, got %v", err)
	}
	if reauthErr.Account != "acct-a" {
		t.Fatalf("reauth account = %q, want acct-a", reauthErr.Account)
	}
	if reauthErr.Provider != prov.Name() {
		t.Fatalf("reauth provider = %q, want %q", reauthErr.Provider, prov.Name())
	}

	// A successful refresh clears the mark and restores selection.
	prov.mu.Lock()
	prov.refreshErr = nil
	prov.mu.Unlock()
	if err := rot.RefreshAll(ctx); err != nil {
		t.Fatalf("RefreshAll after recovery: %v", err)
	}
	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token after recovery: %v", err)
	}
	if account != "acct-a" {
		t.Fatalf("account = %q, want acct-a after recovery", account)
	}
}

// markReauth flips an account's re-auth bit directly so a test can isolate the
// selection outcome without driving a full refresh failure.
func markReauth(t *testing.T, rot *Rotator, name provider.Name, account provider.AccountID) {
	t.Helper()
	rot.mu.RLock()
	state, ok := rot.providers[name]
	rot.mu.RUnlock()
	if !ok {
		t.Fatalf("provider %q not registered", name)
	}
	state.mu.Lock()
	state.reauth[account] = true
	state.mu.Unlock()
}

func TestRotatorSoleReauthAccountReturnsNeedsReauth(t *testing.T) {
	now := time.UnixMilli(6_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a")
	markReauth(t, rot, prov.Name(), "acct-a")

	_, _, err := rot.Token(ctx, prov.Name())
	var reauthErr NeedsReauthError
	if !errors.As(err, &reauthErr) {
		t.Fatalf("want NeedsReauthError, got %v", err)
	}
	if reauthErr.Account != "acct-a" {
		t.Fatalf("reauth account = %q, want acct-a", reauthErr.Account)
	}
}

func TestRotatorUsableAccountWinsOverReauthMarked(t *testing.T) {
	now := time.UnixMilli(7_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	// acct-a needs re-auth, acct-b is healthy: selection must return acct-b
	// rather than erroring, because a usable account exists.
	markReauth(t, rot, prov.Name(), "acct-a")

	token, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if account != "acct-b" {
		t.Fatalf("account = %q, want acct-b", account)
	}
	if token != "acct-b:token" {
		t.Fatalf("token = %q, want acct-b:token", token)
	}
}

func TestRotatorReauthPreferredOverThrottledWhenNoneUsable(t *testing.T) {
	now := time.UnixMilli(8_000_000)
	hour := int64(time.Hour / time.Millisecond)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	// acct-a needs re-auth (never self-recovers); acct-b is throttled (recovers
	// at its reset). With no usable account, the re-auth remedy is actionable so
	// the rotator surfaces NeedsReauthError rather than AllAccountsThrottledError.
	markReauth(t, rot, prov.Name(), "acct-a")
	store := newThrottleStore(prov.Name(), nil)
	entry := throttleEntry{UntilMS: now.UnixMilli() + hour, ObservedMS: now.UnixMilli(), Claim: string(ratelimitsink.ClaimFiveHour), HTTPStatus: 429}
	if err := store.put(ctx, now, provider.AccountID("acct-b"), entry); err != nil {
		t.Fatalf("seed throttle: %v", err)
	}

	_, _, err := rot.Token(ctx, prov.Name())
	var reauthErr NeedsReauthError
	if !errors.As(err, &reauthErr) {
		t.Fatalf("want NeedsReauthError, got %v", err)
	}
	if reauthErr.Account != "acct-a" {
		t.Fatalf("reauth account = %q, want acct-a", reauthErr.Account)
	}
	var throttledErr AllAccountsThrottledError
	if errors.As(err, &throttledErr) {
		t.Fatalf("did not expect AllAccountsThrottledError, got %v", err)
	}
}
