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
// Read was invoked so tests can assert the keychain is reread on each
// detection.
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

// fakeMITMTracker returns a fixed active-session count so tests can model
// "claude session active", "no session", and "many sessions" deterministically.
// The call counter lets tests verify the detector consulted the tracker.
type fakeMITMTracker struct {
	count int
	calls atomic.Int64
}

func (t *fakeMITMTracker) ClaudeAnthropicSessionCount() int {
	t.calls.Add(1)
	return t.count
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
	det      *InUseDetector
	keychain *fakeKeychain
	tracker  *fakeMITMTracker
	accounts []AccountFingerprint
}

// newDetectorFixture builds an InUseDetector backed by deterministic fakes.
// The caller sets keychain credentials, tracker count, and the account
// fingerprint snapshot before calling det.Detect.
func newDetectorFixture(t *testing.T) *detectorFixture {
	t.Helper()
	fx := &detectorFixture{
		det:      nil,
		keychain: &fakeKeychain{cred: KeychainCredential{Tokens: nil, Present: false}, err: nil, calls: atomic.Int64{}},
		tracker:  &fakeMITMTracker{count: 0, calls: atomic.Int64{}},
		accounts: nil,
	}
	fx.det = NewInUseDetector(InUseDeps{
		KeychainReader:   fx.keychain.read,
		Fingerprint:      oauthcredentials.Fingerprint,
		MITMTracker:      fx.tracker,
		AccountSnapshots: func() []AccountFingerprint { return fx.accounts },
		Logger:           nil,
	})
	return fx
}

func TestDetectMatchedFingerprintWithActiveSessionMarksAccount(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-match", "rt-match")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	matched := oauthcredentials.Fingerprint(tokens)
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: "other-fp"},
		{Account: provider.AccountID("acct-b"), Fingerprint: matched},
	}
	fx.tracker.count = 1

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
	if fx.tracker.calls.Load() != 1 {
		t.Fatalf("tracker calls = %d, want 1", fx.tracker.calls.Load())
	}
}

func TestDetectNoActiveSessionReturnsEmpty(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-noproc", "rt-noproc")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: oauthcredentials.Fingerprint(tokens)},
	}
	fx.tracker.count = 0

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("no active session expected empty set, got len=%d", set.Len())
	}
	if fx.tracker.calls.Load() != 1 {
		t.Fatalf("tracker calls = %d, want 1", fx.tracker.calls.Load())
	}
}

func TestDetectActiveSessionFingerprintMissReturnsEmpty(t *testing.T) {
	fx := newDetectorFixture(t)
	tokens := tokensFor("at-miss", "rt-miss")
	fx.keychain.cred = KeychainCredential{Tokens: tokens, Present: true}
	fx.accounts = []AccountFingerprint{
		{Account: provider.AccountID("acct-a"), Fingerprint: "other-fingerprint"},
	}
	fx.tracker.count = 1

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("fingerprint miss expected empty set, got len=%d", set.Len())
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
	if fx.tracker.calls.Load() != 0 {
		t.Fatalf("tracker should not be invoked after keychain error, got %d calls", fx.tracker.calls.Load())
	}
}

func TestDetectKeychainAbsentReturnsEmpty(t *testing.T) {
	fx := newDetectorFixture(t)
	fx.keychain.cred = KeychainCredential{Tokens: nil, Present: false}
	fx.tracker.count = 1

	set, err := fx.det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("absent keychain expected empty set, got len=%d", set.Len())
	}
	if fx.tracker.calls.Load() != 0 {
		t.Fatalf("tracker should not be consulted when keychain is absent, got %d calls", fx.tracker.calls.Load())
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

func TestNewInUseDetectorTreatsNilTrackerAsNoop(t *testing.T) {
	// A daemon that has not wired a MITM tracker yet must still return a
	// safe empty result. A nil tracker defaults to noopTracker which
	// always reports zero active sessions.
	tokens := tokensFor("at-niltrack", "rt-niltrack")
	det := NewInUseDetector(InUseDeps{
		KeychainReader: func(_ context.Context) (KeychainCredential, error) {
			return KeychainCredential{Tokens: tokens, Present: true}, nil
		},
		Fingerprint: oauthcredentials.Fingerprint,
		MITMTracker: nil,
		AccountSnapshots: func() []AccountFingerprint {
			return []AccountFingerprint{
				{Account: provider.AccountID("acct-a"), Fingerprint: oauthcredentials.Fingerprint(tokens)},
			}
		},
		Logger: nil,
	})
	set, err := det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("nil tracker expected empty set, got len=%d", set.Len())
	}
}
