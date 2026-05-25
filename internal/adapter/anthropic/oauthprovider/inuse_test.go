package oauthprovider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// fakeKeychain returns a configured credential and tracks how many times
// Read was invoked so cache tests can assert the keychain is only re-read
// on a cache miss.
type fakeKeychain struct {
	cred  KeychainCredential
	err   error
	calls atomic.Int64
}

func (f *fakeKeychain) read(_ context.Context) (KeychainCredential, error) {
	f.calls.Add(1)
	if f.err != nil {
		return KeychainCredential{Tokens: nil, Present: false}, f.err
	}
	return f.cred, nil
}

// fakeProcessScanner returns a fixed PID list (or an error) so tests can
// model "external session present", "no external session", and "scan
// failure" without spawning real processes. The call counter lets cache
// tests verify scans are skipped within the window.
type fakeProcessScanner struct {
	pids  []int
	err   error
	calls atomic.Int64
}

func (s *fakeProcessScanner) scan(_ context.Context) ([]int, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	out := append([]int(nil), s.pids...)
	return out, nil
}

func tokensFor(access, refresh string) *oauthcredentials.Tokens {
	return &oauthcredentials.Tokens{
		AccessToken:      access,
		RefreshToken:     refresh,
		ExpiresAt:        time.Now().Add(time.Hour).UnixMilli(),
		Scopes:           []string{"user:inference"},
		SubscriptionType: "max",
		RateLimitTier:    "tier1",
	}
}

type detectorFixture struct {
	det       *InUseDetector
	keychain  *fakeKeychain
	scanner   *fakeProcessScanner
	accounts  []AccountFingerprint
	clockTime time.Time
}

// newDetectorFixture builds an InUseDetector backed by deterministic fakes.
// The caller sets keychain credentials, scanner PIDs, and the account
// fingerprint snapshot before calling det.Detect.
func newDetectorFixture(t *testing.T) *detectorFixture {
	t.Helper()
	fx := &detectorFixture{
		det:       nil,
		keychain:  &fakeKeychain{cred: KeychainCredential{Tokens: nil, Present: false}, err: nil, calls: atomic.Int64{}},
		scanner:   &fakeProcessScanner{pids: nil, err: nil, calls: atomic.Int64{}},
		accounts:  nil,
		clockTime: time.UnixMilli(1_000_000),
	}
	fx.det = newInUseDetectorWithScanner(InUseDeps{
		KeychainReader:   fx.keychain.read,
		Fingerprint:      oauthcredentials.Fingerprint,
		DaemonPIDs:       func() []int { return nil },
		AccountSnapshots: func() []AccountFingerprint { return fx.accounts },
		Now:              func() time.Time { return fx.clockTime },
		Logger:           nil,
	}, fx.scanner.scan)
	return fx
}

func TestDetectMatchedFingerprintMarksAccount(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-match", "rt-match")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	matched := oauthcredentials.Fingerprint(tokens)
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: "other-fp"},
		{Account: provider.AccountID("acct-b"), Fingerprint: matched},
	}
	fx.scanner.pids = []int{1234}

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !set.Contains("acct-b") {
		t.Fatalf("expected acct-b in set, got len=%d", set.Len())
	}
	if set.Contains("acct-a") {
		t.Fatalf("did not expect acct-a in set")
	}
}

func TestDetectFingerprintMissReturnsEmpty(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-miss", "rt-miss")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: "other-fingerprint"},
	}
	fx.scanner.pids = []int{1234}

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("fingerprint miss expected empty set, got len=%d", set.Len())
	}
}

func TestDetectNoExternalClaudeReturnsEmpty(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-noproc", "rt-noproc")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: oauthcredentials.Fingerprint(tokens)},
	}
	fx.scanner.pids = nil

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("no external process expected empty set, got len=%d", set.Len())
	}
}

func TestDetectKeychainReadErrorReturnsEmpty(t *testing.T) {
	fx := newDetectorFixture(t)
	fx.keychain.err = errors.New("keychain blew up")

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect must not return keychain error to caller, got %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("keychain error expected empty set, got len=%d", set.Len())
	}
	if fx.keychain.calls.Load() != 1 {
		t.Fatalf("keychain calls = %d, want 1", fx.keychain.calls.Load())
	}
	if fx.scanner.calls.Load() != 0 {
		t.Fatalf("scanner should not be invoked after keychain error, got %d calls", fx.scanner.calls.Load())
	}
}

func TestDetectProcessScanErrorReturnsEmpty(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-scan", "rt-scan")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: oauthcredentials.Fingerprint(tokens)},
	}
	fx.scanner.err = errors.New("ps failed")

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect must not surface scan error to caller, got %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("scan error expected empty set, got len=%d", set.Len())
	}
}

func TestDetectCachesWithinTTL(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-cache", "rt-cache")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: oauthcredentials.Fingerprint(tokens)},
	}
	fx.scanner.pids = []int{1234}

	first, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("first Detect: %v", err)
	}
	if !first.Contains("acct-a") {
		t.Fatalf("first detect missed acct-a")
	}
	scansAfterFirst := fx.scanner.calls.Load()
	if scansAfterFirst != 1 {
		t.Fatalf("scanner calls after first detect = %d, want 1", scansAfterFirst)
	}

	// Second call inside the cache window must reuse the cached result and
	// not re-run the process scan.
	second, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("second Detect: %v", err)
	}
	if !second.Contains("acct-a") {
		t.Fatalf("second detect missed acct-a")
	}
	if fx.scanner.calls.Load() != scansAfterFirst {
		t.Fatalf("scanner re-ran inside cache window: calls=%d, want %d", fx.scanner.calls.Load(), scansAfterFirst)
	}

	// Advance the clock past the TTL: the third call must re-scan.
	fx.clockTime = fx.clockTime.Add(inUseCacheTTL + time.Second)
	third, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("third Detect: %v", err)
	}
	if !third.Contains("acct-a") {
		t.Fatalf("post-cache-window detect missed acct-a")
	}
	if fx.scanner.calls.Load() != scansAfterFirst+1 {
		t.Fatalf("scanner calls after TTL = %d, want %d", fx.scanner.calls.Load(), scansAfterFirst+1)
	}
}

func TestExternalClaudePIDsExcludesDaemonPIDs(t *testing.T) {
	det := newInUseDetectorWithScanner(InUseDeps{
		KeychainReader:   nil,
		Fingerprint:      oauthcredentials.Fingerprint,
		DaemonPIDs:       func() []int { return []int{202} },
		AccountSnapshots: nil,
		Now:              nil,
		Logger:           nil,
	}, func(_ context.Context) ([]int, error) { return []int{101, 202, 303}, nil })

	external, err := det.externalClaudePIDs(context.Background())
	if err != nil {
		t.Fatalf("externalClaudePIDs: %v", err)
	}
	if len(external) != 2 {
		t.Fatalf("external len = %d, want 2 (excluded daemon PID 202)", len(external))
	}
	if external[0] != 101 || external[1] != 303 {
		t.Fatalf("external = %v, want [101 303]", external)
	}
}

func TestParseClaudePIDsFiltersComm(t *testing.T) {
	psOutput := "  PID COMM\n" +
		"  101 claude\n" +
		"  202 /opt/homebrew/bin/claude\n" +
		"  303 codex\n" +
		"  404 claude-helper\n"
	pids := parseClaudePIDs(psOutput)
	want := []int{101, 202}
	if len(pids) != len(want) {
		t.Fatalf("parseClaudePIDs len = %d (%v), want %d (%v)", len(pids), pids, len(want), want)
	}
	for i := range want {
		if pids[i] != want[i] {
			t.Fatalf("pid[%d] = %d, want %d", i, pids[i], want[i])
		}
	}
}

func TestDetectNilDetectorReturnsEmpty(t *testing.T) {
	var det *InUseDetector
	set, err := det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil detector Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("nil detector expected empty set, got len=%d", set.Len())
	}
}

func TestNewInUseDetectorInstallsDefaultScanner(t *testing.T) {
	// The production constructor installs the `ps` scanner. We verify the
	// field is wired so a production caller will not panic; the scanner
	// itself is exercised at runtime (covered by integration / live
	// verification).
	det := NewInUseDetector(InUseDeps{
		KeychainReader:   nil,
		Fingerprint:      oauthcredentials.Fingerprint,
		DaemonPIDs:       nil,
		AccountSnapshots: nil,
		Now:              nil,
		Logger:           nil,
	})
	if det.scanProcs == nil {
		t.Fatalf("NewInUseDetector did not install default scanner")
	}
}
