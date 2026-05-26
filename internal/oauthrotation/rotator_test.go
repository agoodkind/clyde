package oauthrotation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/inuse"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

// fakeProvider is a provider.Provider test double. It derives the account from
// a stored JSON encoding and counts Refresh calls per access token so tests can
// assert single-flight behavior. It also counts MirrorSources and ReadMirror
// calls so the startup-load tests can assert the second Load performs no
// upstream reads.
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
	// mirrorSourcesCalls counts how many times MirrorSources was invoked. It is
	// taken under mu.
	mirrorSourcesCalls int64
	// readMirrorCalls counts how many times ReadMirror was invoked. It is
	// taken under mu.
	readMirrorCalls int64
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
		name:               name,
		mirror:             nil,
		mirrorErr:          nil,
		refreshDelay:       0,
		refreshCount:       make(map[string]*int64),
		refreshErr:         nil,
		mu:                 sync.Mutex{},
		mirrorSourcesCalls: 0,
		readMirrorCalls:    0,
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
	f.mu.Lock()
	f.mirrorSourcesCalls++
	mirror := f.mirror
	mirrorErr := f.mirrorErr
	f.mu.Unlock()
	if mirrorErr != nil {
		return nil, mirrorErr
	}
	return mirror, nil
}

// ReadMirror reads a file-backed mirror source by reading the path and parsing
// it, mirroring how the real Anthropic provider reads a "file" source. A
// missing file returns present=false with no error.
func (f *fakeProvider) ReadMirror(_ context.Context, src provider.MirrorSource) (provider.Credentials, bool, error) {
	f.mu.Lock()
	f.readMirrorCalls++
	f.mu.Unlock()
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

// seedRejection drives the rotator's Observe entry point with a rejected
// signal for a given account so the test exercises the same code path the
// Anthropic client takes on a 429. observedAt and resetAt let the test pin
// the freshness window relative to the rotator's fake clock.
func seedRejection(t *testing.T, rot *Rotator, prov *fakeProvider, account string, observedAt, resetAt time.Time) {
	t.Helper()
	sig := ratelimitsink.Signal{
		Provider:    string(prov.Name()),
		AccessToken: account + ":token",
		Status:      ratelimitsink.StatusRejected,
		Claim:       ratelimitsink.ClaimFiveHour,
		ResetAt:     resetAt,
		ObservedAt:  observedAt,
		HTTPStatus:  429,
	}
	if err := rot.Observe(context.Background(), sig); err != nil {
		t.Fatalf("Observe: %v", err)
	}
}

func TestRotatorToken(t *testing.T) {
	now := time.UnixMilli(1_000_000)

	type rejection struct {
		account    string
		observedAt time.Time
		resetAt    time.Time
	}
	tests := []struct {
		name        string
		accounts    []string
		rejections  []rejection
		wantAccount provider.AccountID
		wantErrAll  bool
		wantSoonest provider.AccountID
	}{
		{
			name:        "ready account picked",
			accounts:    []string{"acct-a", "acct-b"},
			rejections:  nil,
			wantAccount: "acct-a",
			wantErrAll:  false,
			wantSoonest: "",
		},
		{
			name:     "rejected first skips to second",
			accounts: []string{"acct-a", "acct-b"},
			rejections: []rejection{
				{account: "acct-a", observedAt: now, resetAt: now.Add(time.Hour)},
			},
			wantAccount: "acct-b",
			wantErrAll:  false,
			wantSoonest: "",
		},
		{
			name:     "all rejected inside freshness returns typed error with soonest reset",
			accounts: []string{"acct-a", "acct-b"},
			rejections: []rejection{
				{account: "acct-a", observedAt: now, resetAt: now.Add(2 * time.Hour)},
				{account: "acct-b", observedAt: now, resetAt: now.Add(time.Hour)},
			},
			wantAccount: "",
			wantErrAll:  true,
			wantSoonest: "acct-b",
		},
		{
			name:     "rejection past reset is ignored",
			accounts: []string{"acct-a", "acct-b"},
			rejections: []rejection{
				{account: "acct-a", observedAt: now.Add(-30 * time.Second), resetAt: now.Add(-time.Minute)},
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
			for _, rej := range tc.rejections {
				seedRejection(t, rot, prov, rej.account, rej.observedAt, rej.resetAt)
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

func TestRotatorObserveReverseLookup(t *testing.T) {
	now := time.UnixMilli(2_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	sig := ratelimitsink.Signal{
		Provider:    string(prov.Name()),
		AccessToken: "acct-b:token",
		Status:      ratelimitsink.StatusRejected,
		Claim:       ratelimitsink.ClaimSevenDay,
		ResetAt:     now.Add(time.Hour),
		ObservedAt:  now,
		HTTPStatus:  429,
	}
	if err := rot.Observe(ctx, sig); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// acct-b is now fresh-rejected, so Token must pick acct-a.
	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token after Observe: %v", err)
	}
	if account != "acct-a" {
		t.Fatalf("account = %q, want acct-a", account)
	}

	// An unknown token must not match any slot.
	unknown := sig
	unknown.AccessToken = "ghost:token"
	if err := rot.Observe(ctx, unknown); err == nil {
		t.Fatalf("Observe for unknown token: want error, got nil")
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

// seedRotatorWithExpiry builds a rotator like seedRotator but lets each account
// carry its own stored ExpiresAt, so a test can place one account well within
// validity and another at or inside the safety window. Seeding through the
// production Harvest path keeps the persisted-expiry source of truth exercised:
// each account's ExpiresAt is read back from its written .credentials.json.
func seedRotatorWithExpiry(t *testing.T, now time.Time, expiries map[string]time.Time) (*Rotator, *fakeProvider) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	upstreamDir := t.TempDir()
	prov := newFakeProvider("anthropic")
	accounts := make([]string, 0, len(expiries))
	for account := range expiries {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		path := filepath.Join(upstreamDir, account+".json")
		cred := credFor(account, expiries[account])
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

// refreshCountFor reads how many times the fake refreshed the account's
// original stored token. The fake keys its counter by the access token it was
// handed, which credFor sets to "<account>:token", so a never-refreshed account
// has no counter and reports 0.
func refreshCountFor(prov *fakeProvider, account string) int64 {
	prov.mu.Lock()
	defer prov.mu.Unlock()
	counter, ok := prov.refreshCount[account+":token"]
	if !ok {
		return 0
	}
	return atomic.LoadInt64(counter)
}

func TestRefreshIsDue(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	tests := []struct {
		name      string
		window    time.Duration
		expiresAt time.Time
		want      bool
	}{
		{name: "well within validity not due", window: defaultRefreshSafetyWindow, expiresAt: now.Add(time.Hour), want: false},
		{name: "just outside safety window not due", window: defaultRefreshSafetyWindow, expiresAt: now.Add(defaultRefreshSafetyWindow + time.Minute), want: false},
		{name: "exactly at safety window edge is due", window: defaultRefreshSafetyWindow, expiresAt: now.Add(defaultRefreshSafetyWindow), want: true},
		{name: "inside safety window is due", window: defaultRefreshSafetyWindow, expiresAt: now.Add(defaultRefreshSafetyWindow - time.Minute), want: true},
		{name: "already expired is due", window: defaultRefreshSafetyWindow, expiresAt: now.Add(-time.Hour), want: true},
		{name: "zero expiry is due", window: defaultRefreshSafetyWindow, expiresAt: time.Time{}, want: true},
		{name: "one hour window treats thirty minutes to expiry as due", window: time.Hour, expiresAt: now.Add(30 * time.Minute), want: true},
		{name: "one hour window treats five hours to expiry as not due", window: time.Hour, expiresAt: now.Add(5 * time.Hour), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rot := NewRotator(nil)
			rot.SetRefreshSafetyWindow(tc.window)
			if got := rot.refreshIsDue(now, tc.expiresAt); got != tc.want {
				t.Fatalf("refreshIsDue(now, %s) = %v, want %v", tc.expiresAt, got, tc.want)
			}
		})
	}
}

func TestRotatorRefreshDue(t *testing.T) {
	now := time.UnixMilli(20_000_000)
	tests := []struct {
		name          string
		expiries      map[string]time.Time
		wantRefreshed map[string]int64
	}{
		{
			name: "healthy account is skipped",
			expiries: map[string]time.Time{
				"acct-a": now.Add(time.Hour),
			},
			wantRefreshed: map[string]int64{"acct-a": 0},
		},
		{
			name: "account inside safety window is refreshed",
			expiries: map[string]time.Time{
				"acct-a": now.Add(defaultRefreshSafetyWindow - time.Minute),
			},
			wantRefreshed: map[string]int64{"acct-a": 1},
		},
		{
			name: "due refreshed and healthy skipped in one pass",
			expiries: map[string]time.Time{
				"acct-a": now.Add(time.Hour),
				"acct-b": now.Add(-time.Minute),
			},
			wantRefreshed: map[string]int64{"acct-a": 0, "acct-b": 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rot, prov := seedRotatorWithExpiry(t, now, tc.expiries)
			if err := rot.RefreshDue(ctx); err != nil {
				t.Fatalf("RefreshDue: %v", err)
			}
			for account, want := range tc.wantRefreshed {
				if got := refreshCountFor(prov, account); got != want {
					t.Fatalf("account %q refreshed %d times, want %d", account, got, want)
				}
			}
		})
	}
}

// TestRotatorRefreshDueStartupHealthyFleetMintsZeroTokens models a daemon
// reload of a healthy persisted fleet: Harvest imports the accounts (a
// read-only mirror pass that performs no refresh) and the following RefreshDue
// skips every still-valid account, so zero new tokens are minted.
func TestRotatorRefreshDueStartupHealthyFleetMintsZeroTokens(t *testing.T) {
	now := time.UnixMilli(30_000_000)
	ctx := context.Background()
	rot, prov := seedRotatorWithExpiry(t, now, map[string]time.Time{
		"acct-a": now.Add(time.Hour),
		"acct-b": now.Add(2 * time.Hour),
	})

	// Harvest alone must not refresh: it is the read-only import the startup
	// path runs before the due-check.
	rot.Harvest(ctx)
	if got := refreshCountFor(prov, "acct-a") + refreshCountFor(prov, "acct-b"); got != 0 {
		t.Fatalf("Harvest refreshed %d times, want 0", got)
	}

	if err := rot.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	for _, account := range []string{"acct-a", "acct-b"} {
		if got := refreshCountFor(prov, account); got != 0 {
			t.Fatalf("healthy account %q refreshed %d times, want 0", account, got)
		}
	}
}

// TestRotatorRefreshDueStartupRefreshesDueAccount models a reload where one
// persisted account is already inside its safety window: the startup due-check
// refreshes exactly that account.
func TestRotatorRefreshDueStartupRefreshesDueAccount(t *testing.T) {
	now := time.UnixMilli(40_000_000)
	ctx := context.Background()
	rot, prov := seedRotatorWithExpiry(t, now, map[string]time.Time{
		"acct-a": now.Add(defaultRefreshSafetyWindow - time.Minute),
	})

	if err := rot.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	if got := refreshCountFor(prov, "acct-a"); got != 1 {
		t.Fatalf("due account refreshed %d times, want 1", got)
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
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	// acct-a needs re-auth (never self-recovers); acct-b is rejected (recovers
	// at its reset). With no usable account, the re-auth remedy is actionable so
	// the rotator surfaces NeedsReauthError rather than AllAccountsThrottledError.
	markReauth(t, rot, prov.Name(), "acct-a")
	seedRejection(t, rot, prov, "acct-b", now, now.Add(time.Hour))

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

// mirrorSourcesCallCount reads the fake's MirrorSources call counter under
// the fake's own mutex.
func mirrorSourcesCallCount(f *fakeProvider) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mirrorSourcesCalls
}

// readMirrorCallCount reads the fake's ReadMirror call counter under the
// fake's own mutex.
func readMirrorCallCount(f *fakeProvider) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readMirrorCalls
}

// writeAccountDir lays out one per-account directory under accountsDir(name)
// with a .credentials.json the fake provider can ParseStored, plus an
// optional .label file when label is non-empty. The credential bytes are
// produced through the same fakeProvider.EncodeStored helper the production
// path uses, so the test exercises the real parse round-trip on Load.
func writeAccountDir(t *testing.T, prov *fakeProvider, name provider.Name, account string, expiresAt time.Time, label string) {
	t.Helper()
	dir := accountDir(name, provider.AccountID(account))
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cred := credFor(account, expiresAt)
	encoded, err := prov.EncodeStored(cred)
	if err != nil {
		t.Fatalf("EncodeStored(%q): %v", account, err)
	}
	credPath := accountCredentialsPath(name, provider.AccountID(account))
	if err := os.WriteFile(credPath, encoded, credentialFileMode); err != nil {
		t.Fatalf("write %s: %v", credPath, err)
	}
	if label != "" {
		labelPath := accountLabelPath(name, provider.AccountID(account))
		if err := os.WriteFile(labelPath, []byte(label), credentialFileMode); err != nil {
			t.Fatalf("write %s: %v", labelPath, err)
		}
	}
}

// captureLogger builds a slog logger that writes JSON events to buf so a test
// can assert which events fired.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{
		AddSource:   false,
		Level:       slog.LevelDebug,
		ReplaceAttr: nil,
	})
	return slog.New(handler)
}

// countEventLines reports how many log lines in buf carry an exact msg field.
// The handler writes one JSON object per line, so a simple substring match on
// the exact "msg":"<name>" field is sufficient.
func countEventLines(buf *bytes.Buffer, msg string) int {
	count := 0
	needle := `"msg":"` + msg + `"`
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

// newLoadTestRotator wires a rotator with a captured logger and the fixed
// clock. It registers the fake but does not Harvest or Load. Callers are
// responsible for the rest of the setup.
func newLoadTestRotator(t *testing.T, now time.Time, prov *fakeProvider) (*Rotator, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	buf := &bytes.Buffer{}
	rot := NewRotator(captureLogger(buf))
	rot.now = fixedClock(now)
	rot.Register(prov)
	return rot, buf
}

func TestLoadDiskOnly(t *testing.T) {
	now := time.UnixMilli(50_000_000)
	ctx := context.Background()
	prov := newFakeProvider("anthropic")
	// Provider stub declares no upstream sources so the upstream pass is a
	// no-op and the on-disk fold is the only path that populates state.slots.
	prov.mirror = nil
	rot, _ := newLoadTestRotator(t, now, prov)

	// Two valid per-account dirs on disk, written before Load runs.
	writeAccountDir(t, prov, prov.Name(), "acct-b", now.Add(time.Hour), "label-b")
	writeAccountDir(t, prov, prov.Name(), "acct-a", now.Add(time.Hour), "label-a")

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	snapshots, err := rot.Accounts(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(snapshots))
	}
	// Lexicographic order: acct-a before acct-b.
	if snapshots[0].Account != "acct-a" || snapshots[1].Account != "acct-b" {
		t.Fatalf("order = %q,%q, want acct-a,acct-b", snapshots[0].Account, snapshots[1].Account)
	}
	if snapshots[0].Label != "label-a" {
		t.Fatalf("acct-a label = %q, want label-a", snapshots[0].Label)
	}
	if snapshots[1].Label != "label-b" {
		t.Fatalf("acct-b label = %q, want label-b", snapshots[1].Label)
	}
}

func TestLoadKeychainHarvest(t *testing.T) {
	now := time.UnixMilli(60_000_000)
	ctx := context.Background()
	prov := newFakeProvider("anthropic")
	rot, _ := newLoadTestRotator(t, now, prov)

	// Zero per-account dirs on disk. The provider stub's only mirror source is
	// a "keychain" entry whose Location points to a file the fake's ReadMirror
	// can read; the fake's ReadMirror is content-only and ignores Kind, so this
	// faithfully exercises the upstream-pass path that imports an account from
	// a source other than the on-disk per-account store.
	upstreamDir := t.TempDir()
	keychainSourcePath := filepath.Join(upstreamDir, "keychain.json")
	cred := credFor("acct-k", now.Add(time.Hour))
	encoded, err := prov.EncodeStored(cred)
	if err != nil {
		t.Fatalf("EncodeStored: %v", err)
	}
	if err := os.WriteFile(keychainSourcePath, encoded, 0o600); err != nil {
		t.Fatalf("write keychain source: %v", err)
	}
	prov.mirror = []provider.MirrorSource{
		{Kind: provider.MirrorSourceKindKeychain, Location: keychainSourcePath},
	}

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	snapshots, err := rot.Accounts(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Account != "acct-k" {
		t.Fatalf("snapshots = %+v, want one acct-k", snapshots)
	}
	// The mirror pass writes a per-account file under accountsDir(name).
	credPath := accountCredentialsPath(prov.Name(), "acct-k")
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("expected per-account file at %s: %v", credPath, err)
	}
}

func TestLoadIdempotent(t *testing.T) {
	now := time.UnixMilli(70_000_000)
	ctx := context.Background()
	prov := newFakeProvider("anthropic")
	prov.mirror = nil
	rot, _ := newLoadTestRotator(t, now, prov)
	writeAccountDir(t, prov, prov.Name(), "acct-a", now.Add(time.Hour), "")

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load (first): %v", err)
	}
	firstMirror := mirrorSourcesCallCount(prov)
	firstReadMirror := readMirrorCallCount(prov)

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load (second): %v", err)
	}
	secondMirror := mirrorSourcesCallCount(prov)
	secondReadMirror := readMirrorCallCount(prov)

	if firstMirror != 1 {
		t.Fatalf("first Load MirrorSources calls = %d, want 1", firstMirror)
	}
	if secondMirror != firstMirror {
		t.Fatalf("second Load MirrorSources delta = %d, want 0", secondMirror-firstMirror)
	}
	if secondReadMirror != firstReadMirror {
		t.Fatalf("second Load ReadMirror delta = %d, want 0", secondReadMirror-firstReadMirror)
	}
	// And the in-memory slot is still present after the second Load.
	snapshots, err := rot.Accounts(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Account != "acct-a" {
		t.Fatalf("snapshots = %+v, want one acct-a", snapshots)
	}
}

// TestLoadDebounced covers the debounce gate independently of the in-process
// sync.Once. The test calls Load once, then resets the provider state's Once
// so the loadOnce.Do path would fire again if the debounce check did not
// short-circuit first. Within loadMinInterval the second Load must return nil
// without invoking Sync, even though the Once gate would otherwise re-trigger
// the upstream pass.
func TestLoadDebounced(t *testing.T) {
	now := time.UnixMilli(80_000_000)
	ctx := context.Background()
	prov := newFakeProvider("anthropic")
	prov.mirror = nil
	rot, _ := newLoadTestRotator(t, now, prov)
	writeAccountDir(t, prov, prov.Name(), "acct-a", now.Add(time.Hour), "")

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load (first): %v", err)
	}
	firstMirror := mirrorSourcesCallCount(prov)

	// Reset loadOnce so the debounce gate is the only thing that can keep the
	// second pass from running. Advance the clock by less than loadMinInterval
	// to land inside the debounce window relative to the first Load.
	state, err := rot.providerState(ctx, prov.Name())
	if err != nil {
		t.Fatalf("providerState: %v", err)
	}
	state.mu.Lock()
	state.loadOnce = &sync.Once{}
	state.mu.Unlock()
	rot.now = fixedClock(now.Add(loadMinInterval / 2))

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load (debounced): %v", err)
	}
	secondMirror := mirrorSourcesCallCount(prov)
	if secondMirror != firstMirror {
		t.Fatalf("debounced Load MirrorSources delta = %d, want 0", secondMirror-firstMirror)
	}

	// After loadMinInterval elapses, a third Load runs the upstream pass
	// again so the debounce window is bounded as advertised.
	rot.now = fixedClock(now.Add(loadMinInterval + time.Second))
	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load (post-debounce): %v", err)
	}
	thirdMirror := mirrorSourcesCallCount(prov)
	if thirdMirror != firstMirror+1 {
		t.Fatalf("post-debounce Load MirrorSources delta = %d, want 1", thirdMirror-firstMirror)
	}
}

// fakeDetector implements inuse.Detector with a fixed in-use set so tests can
// drive the rotator's skip branches without depending on a real keychain or
// process scanner. It records each Detect call so cache-style assertions
// remain available if a test needs them.
type fakeDetector struct {
	inUse inuse.Set
	err   error
	calls atomic.Int64
}

func (f *fakeDetector) Detect(_ context.Context, _ []provider.AccountID) (inuse.Set, error) {
	f.calls.Add(1)
	if f.err != nil {
		return inuse.NewSet(), f.err
	}
	return f.inUse, nil
}

// registerFakeDetector swaps the process-wide registry entry for one provider
// with a fake detector returning inUseAccounts as the in-use set. The fixture
// restores the prior entry on test cleanup so concurrent tests do not leak
// detector state across the suite.
func registerFakeDetector(t *testing.T, name provider.Name, inUseAccounts ...provider.AccountID) *fakeDetector {
	t.Helper()
	det := &fakeDetector{
		inUse: inuse.NewSet(inUseAccounts...),
		err:   nil,
		calls: atomic.Int64{},
	}
	inuse.Register(name, det)
	t.Cleanup(func() { inuse.Register(name, nil) })
	return det
}

// TestRefreshDueRefreshesEveryDueAccountIncludingInUse pins the
// coordinated-refresh contract: the in-use skip is gone. Both accounts are
// due (already past their stored ExpiresAt), one of them is reported as
// in-use by the detector, and RefreshDue refreshes BOTH so the in-use
// account does not silently decay.
func TestRefreshDueRefreshesEveryDueAccountIncludingInUse(t *testing.T) {
	now := time.UnixMilli(100_000_000)
	ctx := context.Background()
	rot, prov := seedRotatorWithExpiry(t, now, map[string]time.Time{
		"acct-a": now.Add(-time.Minute),
		"acct-b": now.Add(-time.Minute),
	})
	registerFakeDetector(t, prov.Name(), provider.AccountID("acct-a"))

	if err := rot.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	if got := refreshCountFor(prov, "acct-a"); got != 1 {
		t.Fatalf("in-use acct-a refreshed %d times, want 1 (in-use skip is gone)", got)
	}
	if got := refreshCountFor(prov, "acct-b"); got != 1 {
		t.Fatalf("acct-b refreshed %d times, want 1", got)
	}
}

// TestRefreshAllRefreshesEveryAccountIncludingInUse mirrors
// TestRefreshDueRefreshesEveryDueAccountIncludingInUse for RefreshAll.
func TestRefreshAllRefreshesEveryAccountIncludingInUse(t *testing.T) {
	now := time.UnixMilli(110_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	registerFakeDetector(t, prov.Name(), provider.AccountID("acct-b"))

	if err := rot.RefreshAll(ctx); err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
	if got := refreshCountFor(prov, "acct-a"); got != 1 {
		t.Fatalf("acct-a refreshed %d times, want 1", got)
	}
	if got := refreshCountFor(prov, "acct-b"); got != 1 {
		t.Fatalf("in-use acct-b refreshed %d times, want 1 (in-use skip is gone)", got)
	}
}

// TestTokenAfterAuthFailureInUseSyncsKeychain models the in-use path: the
// detector reports acct-a in use, so the rotator runs one mirror sync and
// returns the slot's access token. When the slot is already on the failed
// token (because the upstream mirror file holds the same value), the rotator
// returns ErrTokenUnchanged so the client skips the retry.
func TestTokenAfterAuthFailureInUseSyncsKeychain(t *testing.T) {
	now := time.UnixMilli(120_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a")
	registerFakeDetector(t, prov.Name(), provider.AccountID("acct-a"))

	// The upstream file is unchanged from the original Harvest, so a second
	// mirror sync re-imports the same access token. The rotator therefore
	// returns ErrTokenUnchanged with that same value.
	token, err := rot.TokenAfterAuthFailure(ctx, prov.Name(), "acct-a:token")
	if !errors.Is(err, ErrTokenUnchanged) {
		t.Fatalf("err = %v, want ErrTokenUnchanged", err)
	}
	if token != "acct-a:token" {
		t.Fatalf("token = %q, want acct-a:token", token)
	}
	// The non-in-use branch is the refresh path, which would have bumped the
	// refresh counter. Confirm the in-use path did not trigger it.
	if got := refreshCountFor(prov, "acct-a"); got != 0 {
		t.Fatalf("in-use TokenAfterAuthFailure refreshed %d times, want 0", got)
	}
}

// TestTokenAfterAuthFailureNotInUseRefreshes models the refresh path: the
// detector reports nothing in use, so the rotator runs the standard refresh
// against the provider. The returned token is the renewed access token and
// the refresh counter increments by exactly one.
func TestTokenAfterAuthFailureNotInUseRefreshes(t *testing.T) {
	now := time.UnixMilli(130_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a")
	registerFakeDetector(t, prov.Name())

	token, err := rot.TokenAfterAuthFailure(ctx, prov.Name(), "acct-a:token")
	if err != nil {
		t.Fatalf("TokenAfterAuthFailure: %v", err)
	}
	if token != "acct-a:token+refreshed" {
		t.Fatalf("token = %q, want acct-a:token+refreshed", token)
	}
	if got := refreshCountFor(prov, "acct-a"); got != 1 {
		t.Fatalf("non-in-use TokenAfterAuthFailure refreshed %d times, want 1", got)
	}
}

// seedAllowedObservation drives the rotator's Observe entry point with an
// allowed signal for one account so the slot carries a recorded lastResetAt
// and lastObservedAt the soonest-reset selector can read. The fixed clock on
// the rotator pins the relationship between observation timestamps and the
// selection moment.
func seedAllowedObservation(t *testing.T, rot *Rotator, prov *fakeProvider, account string, observedAt, resetAt time.Time) {
	t.Helper()
	sig := ratelimitsink.Signal{
		Provider:    string(prov.Name()),
		AccessToken: account + ":token",
		Status:      ratelimitsink.StatusAllowed,
		Claim:       ratelimitsink.ClaimFiveHour,
		ResetAt:     resetAt,
		ObservedAt:  observedAt,
		HTTPStatus:  200,
	}
	if err := rot.Observe(context.Background(), sig); err != nil {
		t.Fatalf("Observe: %v", err)
	}
}

// TestSelectActiveSlotPrefersSoonestReset pins the soonest-reset selection
// contract: two eligible accounts both carry an allowed observation with
// distinct upstream reset times in the future, and the rotator returns the
// account whose window resets first so its credit is spent before refresh.
func TestSelectActiveSlotPrefersSoonestReset(t *testing.T) {
	now := time.UnixMilli(300_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	seedAllowedObservation(t, rot, prov, "acct-a", now, now.Add(2*time.Hour))
	seedAllowedObservation(t, rot, prov, "acct-b", now, now.Add(4*time.Hour))

	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if account != "acct-a" {
		t.Fatalf("account = %q, want acct-a (soonest reset)", account)
	}
}

// TestSelectActiveSlotTieBreaksByLeastRecentlyObserved pins the tie-break:
// when two eligible accounts share the same upstream reset time, the rotator
// picks the one whose lastObservedAt is older so an account that has not been
// touched recently gets a turn.
func TestSelectActiveSlotTieBreaksByLeastRecentlyObserved(t *testing.T) {
	now := time.UnixMilli(310_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	// Both accounts share the same upstream reset time, but acct-b was
	// observed earlier than acct-a, so acct-b sorts first under the
	// least-recently-observed tie-break.
	reset := now.Add(2 * time.Hour)
	seedAllowedObservation(t, rot, prov, "acct-a", now, reset)
	seedAllowedObservation(t, rot, prov, "acct-b", now.Add(-time.Minute), reset)

	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if account != "acct-b" {
		t.Fatalf("account = %q, want acct-b (older lastObservedAt)", account)
	}
}

// TestSelectActiveSlotUnobservedAccountSortsLast pins the unobserved-account
// rule: an account with a real recorded reset wins over an account whose
// lastResetAt is the zero value, so a freshly added account does not displace
// an observed account until it has been used at least once.
func TestSelectActiveSlotUnobservedAccountSortsLast(t *testing.T) {
	now := time.UnixMilli(320_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-fresh", "acct-observed")
	// acct-observed has a real reset in the future. acct-fresh has no
	// observation at all (zero lastResetAt), so it sorts after acct-observed
	// even though it appears first in the stable account order.
	seedAllowedObservation(t, rot, prov, "acct-observed", now, now.Add(2*time.Hour))

	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if account != "acct-observed" {
		t.Fatalf("account = %q, want acct-observed (unobserved account sorts last)", account)
	}
}

// TestRejectedObservationSkipsWithinFreshness pins the selection contract:
// a rejected observation made 30s ago with reset 5min in the future keeps
// the slot out of rotation, so the next slot is selected.
func TestRejectedObservationSkipsWithinFreshness(t *testing.T) {
	now := time.UnixMilli(200_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	seedRejection(t, rot, prov, "acct-a", now.Add(-30*time.Second), now.Add(5*time.Minute))

	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if account != "acct-b" {
		t.Fatalf("account = %q, want acct-b (acct-a is fresh-rejected)", account)
	}
}

// TestRejectedObservationOutsideFreshness pins the other side: a rejected
// observation made 90s ago is past the 60s freshness ceiling, so the slot
// re-enters rotation even though Anthropic's reported reset is still in the
// future. The next live request finds out the truth.
func TestRejectedObservationOutsideFreshness(t *testing.T) {
	now := time.UnixMilli(210_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	seedRejection(t, rot, prov, "acct-a", now.Add(-90*time.Second), now.Add(5*time.Minute))

	_, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if account != "acct-a" {
		t.Fatalf("account = %q, want acct-a (stale rejection past freshness window)", account)
	}
}

// TestAllowedObservationClearsRejected pins the recovery contract: after a
// rejected observation, an allowed observation immediately makes the slot
// eligible again.
func TestAllowedObservationClearsRejected(t *testing.T) {
	now := time.UnixMilli(220_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")
	seedRejection(t, rot, prov, "acct-a", now, now.Add(5*time.Minute))

	// Selection should pick acct-b while acct-a is fresh-rejected.
	if _, account, err := rot.Token(ctx, prov.Name()); err != nil || account != "acct-b" {
		t.Fatalf("Token (rejected) = (%q, %v), want (acct-b, nil)", account, err)
	}

	// An allowed observation overrides the rejection.
	if err := rot.Observe(ctx, ratelimitsink.Signal{
		Provider:    string(prov.Name()),
		AccessToken: "acct-a:token",
		Status:      ratelimitsink.StatusAllowed,
		Claim:       ratelimitsink.ClaimFiveHour,
		ResetAt:     now.Add(5 * time.Minute),
		ObservedAt:  now,
		HTTPStatus:  200,
	}); err != nil {
		t.Fatalf("Observe (allowed): %v", err)
	}

	if _, account, err := rot.Token(ctx, prov.Name()); err != nil || account != "acct-a" {
		t.Fatalf("Token (allowed) = (%q, %v), want (acct-a, nil)", account, err)
	}
}

// TestFreshRotatorEveryAccountEligible pins the post-restart contract: a
// brand-new rotator has zero observation state, so every account selects as
// eligible.
func TestFreshRotatorEveryAccountEligible(t *testing.T) {
	now := time.UnixMilli(230_000_000)
	ctx := context.Background()
	rot, prov := seedRotator(t, now, "acct-a", "acct-b")

	if _, account, err := rot.Token(ctx, prov.Name()); err != nil || account != "acct-a" {
		t.Fatalf("Token (fresh) = (%q, %v), want (acct-a, nil)", account, err)
	}
	snapshots, err := rot.Accounts(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, snap := range snapshots {
		if snap.Throttled {
			t.Fatalf("snapshot for %q reports Throttled=true on a fresh rotator", snap.Account)
		}
	}
}

// TestStartupLoaderDeletesLegacyThrottle pins the on-disk migration: a
// pre-existing throttle.json (plus its .lock companion) is deleted on
// startup load, and the rotator emits the legacy_file_removed event so an
// operator can confirm the migration ran.
func TestStartupLoaderDeletesLegacyThrottle(t *testing.T) {
	now := time.UnixMilli(240_000_000)
	ctx := context.Background()
	prov := newFakeProvider("anthropic")
	prov.mirror = nil
	rot, logBuf := newLoadTestRotator(t, now, prov)

	dir := providerDir(prov.Name())
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		t.Fatalf("mkdir provider dir: %v", err)
	}
	legacyPath := filepath.Join(dir, legacyThrottleFileName)
	if err := os.WriteFile(legacyPath, []byte(`{"version":1,"entries":{}}`), credentialFileMode); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}
	legacyLockPath := legacyPath + ".lock"
	if err := os.WriteFile(legacyLockPath, []byte{}, credentialFileMode); err != nil {
		t.Fatalf("seed legacy lock: %v", err)
	}

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected legacy throttle file removed, stat err=%v", err)
	}
	if _, err := os.Stat(legacyLockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected legacy throttle lock removed, stat err=%v", err)
	}
	if got := countEventLines(logBuf, "oauthrotation.throttle.legacy_file_removed"); got < 1 {
		t.Fatalf("legacy_file_removed events = %d, want >= 1, logs=%s", got, logBuf.String())
	}
}

// TestStartupLoaderLegacyThrottleMissingIsNoop confirms the migration is
// silent when no legacy file exists on disk.
func TestStartupLoaderLegacyThrottleMissingIsNoop(t *testing.T) {
	now := time.UnixMilli(250_000_000)
	ctx := context.Background()
	prov := newFakeProvider("anthropic")
	prov.mirror = nil
	rot, logBuf := newLoadTestRotator(t, now, prov)

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := countEventLines(logBuf, "oauthrotation.throttle.legacy_file_removed"); got != 0 {
		t.Fatalf("legacy_file_removed events = %d, want 0 when nothing is on disk", got)
	}
}

func TestLoadMalformedSkipped(t *testing.T) {
	now := time.UnixMilli(90_000_000)
	ctx := context.Background()
	prov := newFakeProvider("anthropic")
	prov.mirror = nil
	rot, logBuf := newLoadTestRotator(t, now, prov)

	// One valid account dir.
	writeAccountDir(t, prov, prov.Name(), "acct-good", now.Add(time.Hour), "")
	// One dir with a malformed .credentials.json that fakeProvider.ParseStored
	// rejects (the fake decodes fakeStored JSON; a non-JSON blob fails the
	// json.Unmarshal in ParseStored).
	badDir := accountDir(prov.Name(), "acct-bad")
	if err := os.MkdirAll(badDir, storeDirMode); err != nil {
		t.Fatalf("mkdir bad: %v", err)
	}
	badPath := accountCredentialsPath(prov.Name(), "acct-bad")
	if err := os.WriteFile(badPath, []byte("not json"), credentialFileMode); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	if err := rot.Load(ctx, prov.Name()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	snapshots, err := rot.Accounts(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Account != "acct-good" {
		t.Fatalf("snapshots = %+v, want one acct-good", snapshots)
	}
	skipped := countEventLines(logBuf, "oauthrotation.startup.account_skipped")
	if skipped < 1 {
		t.Fatalf("oauthrotation.startup.account_skipped events = %d, want >= 1, logs=%s", skipped, logBuf.String())
	}
}
