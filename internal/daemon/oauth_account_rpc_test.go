package daemon

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/daemonclient"
	"goodkind.io/clyde/internal/oauthrotation"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/slogger"
)

// oauthRPCFakeProvider is a minimal provider.Provider for the daemon
// round-trip test. It encodes the account into the access token as
// "<account>:token", supports a stub interactive login, and round-trips a JSON
// credential document.
type oauthRPCFakeProvider struct {
	name         provider.Name
	authorizeURL string
	loginAccount string
}

type oauthRPCStored struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
	Fingerprint  string `json:"fingerprint"`
}

func (p *oauthRPCFakeProvider) Name() provider.Name { return p.name }

func (p *oauthRPCFakeProvider) AccountID(accessToken string) (provider.AccountID, error) {
	for i := 0; i < len(accessToken); i++ {
		if accessToken[i] == ':' {
			return provider.AccountID(accessToken[:i]), nil
		}
	}
	return provider.AccountID(accessToken), nil
}

func (p *oauthRPCFakeProvider) Refresh(_ context.Context, current provider.Credentials) (provider.Credentials, error) {
	return current, nil
}

func (p *oauthRPCFakeProvider) MirrorSources(_ context.Context) ([]provider.MirrorSource, error) {
	return nil, nil
}

func (p *oauthRPCFakeProvider) ReadMirror(_ context.Context, _ provider.MirrorSource) (provider.Credentials, bool, error) {
	return provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}, false, nil
}

func (p *oauthRPCFakeProvider) SpawnLogin(_ context.Context, _ provider.LoginOptions) (provider.LoginSession, error) {
	return provider.LoginSession{Handle: "h", AuthorizeURL: p.authorizeURL, ScratchDir: ""}, nil
}

func (p *oauthRPCFakeProvider) CompleteLogin(_ context.Context, _ provider.LoginSession) (provider.Credentials, error) {
	cred := provider.Credentials{
		AccessToken:  p.loginAccount + ":token",
		RefreshToken: p.loginAccount + ":refresh",
		ExpiresAt:    time.UnixMilli(20_000_000).Add(time.Hour),
		Raw:          nil,
		Fingerprint:  p.loginAccount + "-fp",
	}
	raw, _ := p.EncodeStored(cred)
	cred.Raw = raw
	return cred, nil
}

func (p *oauthRPCFakeProvider) ParseStored(raw []byte) (provider.Credentials, error) {
	var stored oauthRPCStored
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

func (p *oauthRPCFakeProvider) EncodeStored(c provider.Credentials) ([]byte, error) {
	return json.Marshal(oauthRPCStored{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		ExpiresAtMS:  c.ExpiresAt.UnixMilli(),
		Fingerprint:  c.Fingerprint,
	})
}

// startOAuthRPCServer serves a real daemon Server backed by a rotator over an
// in-process unix socket and returns a connected typed OAuth client.
func startOAuthRPCServer(t *testing.T, rotator *oauthrotation.Rotator) *daemonclient.OAuthAccountClient {
	t.Helper()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	srv := newServerState(slogger.Concern(slogger.ConcernProcessDaemonLifecycle).Logger(), watcher)
	srv.SetOAuthRotatorAccessor(func() *oauthrotation.Rotator { return rotator })

	socketPath := t.TempDir() + "/daemon.sock"
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	clydev1.RegisterClydeServiceServer(grpcServer, srv)
	serveDone := make(chan error, 1)
	go func() { serveDone <- grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		<-serveDone
	})

	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return daemonclient.NewOAuthAccountClient(conn)
}

func TestOAuthAccountRPCRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	rotator := oauthrotation.NewRotator(nil)
	rotator.Register(&oauthRPCFakeProvider{
		name:         "anthropic",
		authorizeURL: "https://example.test/authorize",
		loginAccount: "acct-rpc",
	})
	client := startOAuthRPCServer(t, rotator)

	// Login streams an authorize URL then a result account.
	var gotURL string
	var gotAccount *daemonclient.OAuthAccount
	if err := client.Login(ctx, "anthropic", "user@example.test", "primary", func(update daemonclient.OAuthLoginUpdate) {
		if update.AuthorizeURL != "" {
			gotURL = update.AuthorizeURL
		}
		if update.Account != nil {
			gotAccount = update.Account
		}
	}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotURL != "https://example.test/authorize" {
		t.Fatalf("authorize url = %q", gotURL)
	}
	if gotAccount == nil || gotAccount.AccountID != "acct-rpc" {
		t.Fatalf("result account = %+v, want acct-rpc", gotAccount)
	}
	if gotAccount.Label != "primary" {
		t.Fatalf("result label = %q, want primary", gotAccount.Label)
	}

	// List returns the logged-in account with its label and refresh flag.
	accounts, err := client.List(ctx, "anthropic")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0].AccountID != "acct-rpc" || accounts[0].Label != "primary" {
		t.Fatalf("account = %+v", accounts[0])
	}
	if !accounts[0].RefreshTokenPresent {
		t.Fatalf("refresh token absent, want present")
	}

	// List with empty provider enumerates all providers.
	allAccounts, err := client.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(allAccounts) != 1 {
		t.Fatalf("all accounts = %d, want 1", len(allAccounts))
	}

	// Forget by label removes it.
	forgotten, accountID, err := client.Forget(ctx, "anthropic", "primary")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !forgotten || accountID != "acct-rpc" {
		t.Fatalf("forget = (%v, %q), want (true, acct-rpc)", forgotten, accountID)
	}

	after, err := client.List(ctx, "anthropic")
	if err != nil {
		t.Fatalf("List after forget: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("accounts after forget = %d, want 0", len(after))
	}
}

func TestOAuthAccountListRequiresRotator(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	srv := newServerState(slogger.Concern(slogger.ConcernProcessDaemonLifecycle).Logger(), watcher)
	// No rotator accessor set: handler must report FailedPrecondition.
	_, err = srv.OAuthAccountList(ctx, &clydev1.OAuthAccountListRequest{Provider: ""})
	if err == nil {
		t.Fatal("OAuthAccountList without rotator: want error, got nil")
	}
}
