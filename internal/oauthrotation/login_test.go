package oauthrotation

import (
	"context"
	"os"
	"testing"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// loginFakeProvider extends the behavior the rotator's Login path needs: a
// SpawnLogin that returns a fixed authorize URL and a CompleteLogin that yields
// a credential for a configured account. It embeds the shared fakeProvider so
// AccountID, ParseStored, EncodeStored, and Refresh behave identically.
type loginFakeProvider struct {
	*fakeProvider
	authorizeURL string
	loginAccount string
	spawnErr     error
	completeErr  error
}

func newLoginFakeProvider(name provider.Name) *loginFakeProvider {
	return &loginFakeProvider{
		fakeProvider: newFakeProvider(name),
		authorizeURL: "https://example.test/authorize?code=stub",
		loginAccount: "acct-login",
		spawnErr:     nil,
		completeErr:  nil,
	}
}

func (p *loginFakeProvider) SpawnLogin(_ context.Context, _ provider.LoginOptions) (provider.LoginSession, error) {
	if p.spawnErr != nil {
		return provider.LoginSession{Handle: "", AuthorizeURL: "", ScratchDir: ""}, p.spawnErr
	}
	return provider.LoginSession{
		Handle:       "handle-1",
		AuthorizeURL: p.authorizeURL,
		ScratchDir:   "",
	}, nil
}

func (p *loginFakeProvider) CompleteLogin(_ context.Context, _ provider.LoginSession) (provider.Credentials, error) {
	if p.completeErr != nil {
		return provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}, p.completeErr
	}
	return credFor(p.loginAccount, time.UnixMilli(10_000_000).Add(time.Hour)), nil
}

func newLoginRotator(t *testing.T, now time.Time, prov *loginFakeProvider) *Rotator {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	rot := NewRotator(nil)
	rot.now = fixedClock(now)
	rot.Register(prov)
	return rot
}

func TestRotatorLoginAddsAccount(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	ctx := context.Background()
	prov := newLoginFakeProvider("anthropic")
	rot := newLoginRotator(t, now, prov)

	var gotURL string
	result, err := rot.Login(ctx, prov.Name(), provider.LoginOptions{Email: "a@example.test", Label: "primary"}, func(authorizeURL string) {
		gotURL = authorizeURL
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotURL != prov.authorizeURL {
		t.Fatalf("authorize url = %q, want %q", gotURL, prov.authorizeURL)
	}
	if result.Account != provider.AccountID(prov.loginAccount) {
		t.Fatalf("account = %q, want %q", result.Account, prov.loginAccount)
	}
	if result.Label != "primary" {
		t.Fatalf("label = %q, want primary", result.Label)
	}

	// Token must now serve the just-logged-in account.
	token, account, err := rot.Token(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Token after login: %v", err)
	}
	if account != provider.AccountID(prov.loginAccount) {
		t.Fatalf("token account = %q, want %q", account, prov.loginAccount)
	}
	if token != prov.loginAccount+":token" {
		t.Fatalf("token = %q, want %q", token, prov.loginAccount+":token")
	}

	// The snapshot must carry the label and a present refresh token.
	snapshots, err := rot.Accounts(ctx, prov.Name())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots))
	}
	if snapshots[0].Label != "primary" {
		t.Fatalf("snapshot label = %q, want primary", snapshots[0].Label)
	}
	if !snapshots[0].RefreshTokenPresent {
		t.Fatalf("snapshot refresh token absent, want present")
	}

	// The label file must be persisted on disk so a later harvest reloads it.
	labelPath := accountLabelPath(prov.Name(), result.Account)
	raw, readErr := os.ReadFile(labelPath)
	if readErr != nil {
		t.Fatalf("read label file %s: %v", labelPath, readErr)
	}
	if string(raw) != "primary" {
		t.Fatalf("label file = %q, want primary", string(raw))
	}
}

func TestRotatorLoginSurfacesAuthorizeURLBeforeError(t *testing.T) {
	now := time.UnixMilli(11_000_000)
	ctx := context.Background()
	prov := newLoginFakeProvider("anthropic")
	prov.completeErr = errStubCompleteFailed
	rot := newLoginRotator(t, now, prov)

	urlSurfaced := false
	_, err := rot.Login(ctx, prov.Name(), provider.LoginOptions{Email: "", Label: ""}, func(_ string) {
		urlSurfaced = true
	})
	if err == nil {
		t.Fatal("Login: want error from CompleteLogin, got nil")
	}
	if !urlSurfaced {
		t.Fatal("authorize url callback was not invoked before the completion error")
	}
}

func TestRotatorForgetByLabelRemovesAccount(t *testing.T) {
	now := time.UnixMilli(12_000_000)
	ctx := context.Background()
	prov := newLoginFakeProvider("anthropic")
	rot := newLoginRotator(t, now, prov)

	result, err := rot.Login(ctx, prov.Name(), provider.LoginOptions{Email: "", Label: "work"}, nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	accountDirPath := accountDir(prov.Name(), result.Account)
	if _, statErr := os.Stat(accountDirPath); statErr != nil {
		t.Fatalf("account dir missing after login: %v", statErr)
	}

	forget, err := rot.Forget(ctx, prov.Name(), "work")
	if err != nil {
		t.Fatalf("Forget by label: %v", err)
	}
	if !forget.Forgotten {
		t.Fatal("Forget: want Forgotten=true")
	}
	if forget.Account != result.Account {
		t.Fatalf("forgot account = %q, want %q", forget.Account, result.Account)
	}
	if _, statErr := os.Stat(accountDirPath); !os.IsNotExist(statErr) {
		t.Fatalf("account dir still present after forget: %v", statErr)
	}

	// Token must report no account after the only account is forgotten.
	if _, _, tokenErr := rot.Token(ctx, prov.Name()); tokenErr == nil {
		t.Fatal("Token after forget: want error, got nil")
	}
}

func TestRotatorForgetByIDRemovesAccount(t *testing.T) {
	now := time.UnixMilli(13_000_000)
	ctx := context.Background()
	prov := newLoginFakeProvider("anthropic")
	rot := newLoginRotator(t, now, prov)

	result, err := rot.Login(ctx, prov.Name(), provider.LoginOptions{Email: "", Label: ""}, nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	forget, err := rot.Forget(ctx, prov.Name(), string(result.Account))
	if err != nil {
		t.Fatalf("Forget by id: %v", err)
	}
	if !forget.Forgotten {
		t.Fatal("Forget: want Forgotten=true")
	}
}

func TestRotatorForgetNoMatch(t *testing.T) {
	now := time.UnixMilli(14_000_000)
	ctx := context.Background()
	prov := newLoginFakeProvider("anthropic")
	rot := newLoginRotator(t, now, prov)

	forget, err := rot.Forget(ctx, prov.Name(), "ghost")
	if err != nil {
		t.Fatalf("Forget no match: %v", err)
	}
	if forget.Forgotten {
		t.Fatal("Forget: want Forgotten=false for unknown account")
	}
}

func TestRotatorProviderNames(t *testing.T) {
	now := time.UnixMilli(15_000_000)
	prov := newLoginFakeProvider("anthropic")
	rot := newLoginRotator(t, now, prov)
	names := rot.ProviderNames()
	if len(names) != 1 || names[0] != provider.Name("anthropic") {
		t.Fatalf("ProviderNames = %v, want [anthropic]", names)
	}
}

// errStubCompleteFailed is the sentinel CompleteLogin failure used to assert the
// authorize URL is streamed before a completion error.
var errStubCompleteFailed = stubError("complete login failed")

type stubError string

func (e stubError) Error() string { return string(e) }
