package mirror

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

type fakeStored struct {
	AccessToken string `json:"access_token"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
	Fingerprint string `json:"fingerprint"`
}

// fakeProvider parses a JSON stored encoding and derives the account from the
// token prefix before the colon.
type fakeProvider struct {
	name    provider.Name
	sources []provider.MirrorSource
	// keychainCreds, when set, is returned by ReadMirror for any keychain
	// source so a keychain mirror can be exercised without an on-disk file.
	keychainCreds *provider.Credentials
}

func (f *fakeProvider) Name() provider.Name { return f.name }

func (f *fakeProvider) AccountID(accessToken string) (provider.AccountID, error) {
	for i := 0; i < len(accessToken); i++ {
		if accessToken[i] == ':' {
			return provider.AccountID(accessToken[:i]), nil
		}
	}
	return provider.AccountID(accessToken), nil
}

func (f *fakeProvider) Refresh(_ context.Context, c provider.Credentials) (provider.Credentials, error) {
	return c, nil
}

func (f *fakeProvider) MirrorSources(_ context.Context) ([]provider.MirrorSource, error) {
	return f.sources, nil
}

// ReadMirror reads a "file" source from disk and a "keychain" source from the
// injected keychainCreds, so a keychain mirror can be tested without a backing
// file. A missing file returns present=false with no error.
func (f *fakeProvider) ReadMirror(_ context.Context, src provider.MirrorSource) (provider.Credentials, bool, error) {
	if src.Kind == provider.MirrorSourceKindKeychain {
		if f.keychainCreds == nil {
			return provider.Credentials{}, false, nil
		}
		return *f.keychainCreds, true, nil
	}
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
	return provider.LoginSession{Handle: "", AuthorizeURL: "", ScratchDir: ""}, nil
}

func (f *fakeProvider) CompleteLogin(_ context.Context, _ provider.LoginSession) (provider.Credentials, error) {
	return provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}, nil
}

func (f *fakeProvider) ParseStored(raw []byte) (provider.Credentials, error) {
	var stored fakeStored
	if err := json.Unmarshal(raw, &stored); err != nil {
		return provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}, err
	}
	return provider.Credentials{
		AccessToken:  stored.AccessToken,
		RefreshToken: "",
		ExpiresAt:    time.UnixMilli(stored.ExpiresAtMS),
		Raw:          raw,
		Fingerprint:  stored.Fingerprint,
	}, nil
}

func (f *fakeProvider) EncodeStored(c provider.Credentials) ([]byte, error) {
	return json.Marshal(fakeStored{AccessToken: c.AccessToken, ExpiresAtMS: c.ExpiresAt.UnixMilli(), Fingerprint: c.Fingerprint})
}

func writeStoredFile(t *testing.T, path, accessToken string, expiresAt time.Time, fingerprint string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(fakeStored{AccessToken: accessToken, ExpiresAtMS: expiresAt.UnixMilli(), Fingerprint: fingerprint})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func pathsFor(root string) Paths {
	return Paths{
		AccountDir: func(account provider.AccountID) string {
			return filepath.Join(root, "accounts", string(account))
		},
		CredentialsFile: func(account provider.AccountID) string {
			return filepath.Join(root, "accounts", string(account), ".credentials.json")
		},
	}
}

func TestSyncImportsFreshUpstream(t *testing.T) {
	root := t.TempDir()
	now := time.UnixMilli(1_000_000)
	upstreamPath := filepath.Join(root, "upstream.json")
	writeStoredFile(t, upstreamPath, "acct-a:tok", now.Add(time.Hour), "fp-1")

	prov := &fakeProvider{
		name:    "anthropic",
		sources: []provider.MirrorSource{{Kind: provider.MirrorSourceKindFile, Location: upstreamPath}},
	}
	paths := pathsFor(root)
	syncer := NewSyncer(prov, paths, Modes{FileMode: 0o600, DirMode: 0o700}, nil)

	result, err := syncer.Sync(context.Background(), now)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Imported) != 1 || result.Imported[0] != "acct-a" {
		t.Fatalf("imported = %v, want [acct-a]", result.Imported)
	}
	if _, statErr := os.Stat(paths.CredentialsFile("acct-a")); statErr != nil {
		t.Fatalf("expected account dir written: %v", statErr)
	}
}

func TestSyncStaleUpstreamDoesNotOverwriteFresherCopy(t *testing.T) {
	root := t.TempDir()
	now := time.UnixMilli(2_000_000)
	upstreamPath := filepath.Join(root, "upstream.json")
	// Upstream expires sooner than the already-stored Clyde copy.
	writeStoredFile(t, upstreamPath, "acct-a:old", now.Add(time.Hour), "fp-old")

	paths := pathsFor(root)
	// Pre-seed a fresher Clyde copy.
	writeStoredFile(t, paths.CredentialsFile("acct-a"), "acct-a:new", now.Add(10*time.Hour), "fp-new")

	prov := &fakeProvider{
		name:    "anthropic",
		sources: []provider.MirrorSource{{Kind: provider.MirrorSourceKindFile, Location: upstreamPath}},
	}
	syncer := NewSyncer(prov, paths, Modes{FileMode: 0o600, DirMode: 0o700}, nil)

	result, err := syncer.Sync(context.Background(), now)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "acct-a" {
		t.Fatalf("skipped = %v, want [acct-a]", result.Skipped)
	}
	stored, err := os.ReadFile(paths.CredentialsFile("acct-a"))
	if err != nil {
		t.Fatalf("read stored: %v", err)
	}
	parsed, err := prov.ParseStored(stored)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	if parsed.AccessToken != "acct-a:new" {
		t.Fatalf("stored token = %q, want acct-a:new (fresher copy must survive)", parsed.AccessToken)
	}
}

func TestSyncKeepsPreviouslySeenAccountWhenKeychainChanges(t *testing.T) {
	root := t.TempDir()
	now := time.UnixMilli(3_000_000)
	paths := pathsFor(root)

	// A previously-seen account already lives in the store.
	writeStoredFile(t, paths.CredentialsFile("acct-old"), "acct-old:tok", now.Add(time.Hour), "fp-old")

	// The keychain now holds a different account. A keychain source is read
	// through the provider, not from a file, so the new account is injected.
	keychainCred := provider.Credentials{
		AccessToken:  "acct-new:tok",
		RefreshToken: "",
		ExpiresAt:    now.Add(time.Hour),
		Raw:          nil,
		Fingerprint:  "fp-new",
	}
	prov := &fakeProvider{
		name:          "anthropic",
		sources:       []provider.MirrorSource{{Kind: provider.MirrorSourceKindKeychain, Location: "anthropic-service"}},
		keychainCreds: &keychainCred,
	}
	syncer := NewSyncer(prov, paths, Modes{FileMode: 0o600, DirMode: 0o700}, nil)

	if _, err := syncer.Sync(context.Background(), now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Both the old and the new account dirs must exist; the old one is kept.
	if _, err := os.Stat(paths.CredentialsFile("acct-old")); err != nil {
		t.Fatalf("previously-seen account dropped: %v", err)
	}
	if _, err := os.Stat(paths.CredentialsFile("acct-new")); err != nil {
		t.Fatalf("new keychain account not imported: %v", err)
	}
}

func TestSyncImportsKeychainSourceThroughProvider(t *testing.T) {
	root := t.TempDir()
	now := time.UnixMilli(4_000_000)
	paths := pathsFor(root)

	// The provider returns credentials for a keychain source with no backing
	// file, exercising the ReadMirror path that shells the keychain.
	keychainCred := provider.Credentials{
		AccessToken:  "acct-kc:tok",
		RefreshToken: "",
		ExpiresAt:    now.Add(time.Hour),
		Raw:          nil,
		Fingerprint:  "fp-kc",
	}
	prov := &fakeProvider{
		name:          "anthropic",
		sources:       []provider.MirrorSource{{Kind: provider.MirrorSourceKindKeychain, Location: "anthropic-service"}},
		keychainCreds: &keychainCred,
	}
	syncer := NewSyncer(prov, paths, Modes{FileMode: 0o600, DirMode: 0o700}, nil)

	result, err := syncer.Sync(context.Background(), now)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Imported) != 1 || result.Imported[0] != "acct-kc" {
		t.Fatalf("imported = %v, want [acct-kc]", result.Imported)
	}
	if _, statErr := os.Stat(paths.CredentialsFile("acct-kc")); statErr != nil {
		t.Fatalf("expected keychain account written: %v", statErr)
	}
}

func TestSyncSkipsAbsentKeychainSource(t *testing.T) {
	root := t.TempDir()
	now := time.UnixMilli(5_000_000)
	paths := pathsFor(root)

	// An empty keychain item yields present=false; Sync must not error or write.
	prov := &fakeProvider{
		name:          "anthropic",
		sources:       []provider.MirrorSource{{Kind: provider.MirrorSourceKindKeychain, Location: "anthropic-service"}},
		keychainCreds: nil,
	}
	syncer := NewSyncer(prov, paths, Modes{FileMode: 0o600, DirMode: 0o700}, nil)

	result, err := syncer.Sync(context.Background(), now)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Imported) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("imported = %v, skipped = %v, want both empty", result.Imported, result.Skipped)
	}
}
