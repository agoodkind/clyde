package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
)

// OAuthAccount is a typed view of one rotation-store account for CLI callers.
// It mirrors the proto snapshot without leaking generated types past the
// daemonclient boundary.
type OAuthAccount struct {
	Provider            string
	AccountID           string
	Label               string
	ExpiresAtMS         int64
	RefreshTokenPresent bool
	ThrottledUntilMS    int64
	ThrottledClaim      string
	NeedsReauth         bool
}

// OAuthLoginUpdate is one phase of a streamed login. Exactly one of
// AuthorizeURL or Account is populated: AuthorizeURL on the authorize phase,
// Account on the final result phase.
type OAuthLoginUpdate struct {
	AuthorizeURL string
	Account      *OAuthAccount
}

// OAuthAccountClient wraps the OAuth-account RPCs over an existing daemon gRPC
// connection. It is the thin typed client the `clyde oauth` CLI uses; it does
// no filesystem access and owns no rotation state. The caller owns the
// connection lifecycle.
type OAuthAccountClient struct {
	rpc clydev1.ClydeServiceClient
}

// NewOAuthAccountClient wraps an existing daemon gRPC connection with the
// OAuth-account RPC helpers.
func NewOAuthAccountClient(conn *grpc.ClientConn) *OAuthAccountClient {
	return &OAuthAccountClient{
		rpc: clydev1.NewClydeServiceClient(conn),
	}
}

// List returns rotation-store account snapshots. An empty provider lists
// accounts across every registered provider.
func (c *OAuthAccountClient) List(ctx context.Context, providerName string) ([]OAuthAccount, error) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	log := clientLog(callCtx)
	log.DebugContext(callCtx, "daemon.client.oauth_account.list.begin", "provider", providerName)
	resp, err := c.rpc.OAuthAccountList(callCtx, &clydev1.OAuthAccountListRequest{Provider: providerName})
	if err != nil {
		log.ErrorContext(callCtx, "daemon.client.oauth_account.list.rpc_failed", "provider", providerName, "err", err)
		return nil, fmt.Errorf("oauth account list rpc: %w", err)
	}
	out := make([]OAuthAccount, 0, len(resp.GetAccounts()))
	for _, snapshot := range resp.GetAccounts() {
		out = append(out, oauthAccountFromProto(snapshot))
	}
	return out, nil
}

// Forget removes one account by id or label and reports the resolved account id
// when a match was removed.
func (c *OAuthAccountClient) Forget(ctx context.Context, providerName, idOrLabel string) (bool, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	log := clientLog(callCtx)
	log.DebugContext(callCtx, "daemon.client.oauth_account.forget.begin", "provider", providerName, "id_or_label", idOrLabel)
	resp, err := c.rpc.OAuthAccountForget(callCtx, &clydev1.OAuthAccountForgetRequest{
		Provider:  providerName,
		IdOrLabel: idOrLabel,
	})
	if err != nil {
		log.ErrorContext(callCtx, "daemon.client.oauth_account.forget.rpc_failed", "provider", providerName, "id_or_label", idOrLabel, "err", err)
		return false, "", fmt.Errorf("oauth account forget rpc: %w", err)
	}
	return resp.GetForgotten(), resp.GetAccountId(), nil
}

// Login drives an interactive login and invokes onUpdate once per streamed
// phase: first with the authorize URL, then with the resulting account. The
// caller blocks until the stream completes, so a generous context deadline is
// the caller's responsibility (the login child waits on browser interaction).
func (c *OAuthAccountClient) Login(ctx context.Context, providerName, email, label string, onUpdate func(update OAuthLoginUpdate)) error {
	log := clientLog(ctx)
	log.DebugContext(ctx, "daemon.client.oauth_account.login.begin", "provider", providerName, "email_present", email != "")
	stream, err := c.rpc.OAuthAccountLogin(ctx, &clydev1.OAuthAccountLoginRequest{
		Provider: providerName,
		Email:    email,
		Label:    label,
	})
	if err != nil {
		log.ErrorContext(ctx, "daemon.client.oauth_account.login.rpc_failed", "provider", providerName, "err", err)
		return fmt.Errorf("oauth account login rpc: %w", err)
	}
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				return nil
			}
			log.ErrorContext(ctx, "daemon.client.oauth_account.login.recv_failed", "provider", providerName, "err", recvErr)
			return fmt.Errorf("oauth account login stream: %w", recvErr)
		}
		if authorize := event.GetAuthorize(); authorize != nil {
			onUpdate(OAuthLoginUpdate{AuthorizeURL: authorize.GetAuthorizeUrl(), Account: nil})
			continue
		}
		if result := event.GetResult(); result != nil {
			account := oauthAccountFromProto(result.GetAccount())
			onUpdate(OAuthLoginUpdate{AuthorizeURL: "", Account: &account})
		}
	}
}

func oauthAccountFromProto(snapshot *clydev1.OAuthAccountSnapshot) OAuthAccount {
	if snapshot == nil {
		return OAuthAccount{
			Provider:            "",
			AccountID:           "",
			Label:               "",
			ExpiresAtMS:         0,
			RefreshTokenPresent: false,
			ThrottledUntilMS:    0,
			ThrottledClaim:      "",
			NeedsReauth:         false,
		}
	}
	return OAuthAccount{
		Provider:            snapshot.GetProvider(),
		AccountID:           snapshot.GetAccountId(),
		Label:               snapshot.GetLabel(),
		ExpiresAtMS:         snapshot.GetExpiresAtMs(),
		RefreshTokenPresent: snapshot.GetRefreshTokenPresent(),
		ThrottledUntilMS:    snapshot.GetThrottledUntilMs(),
		ThrottledClaim:      snapshot.GetThrottledClaim(),
		NeedsReauth:         snapshot.GetNeedsReauth(),
	}
}
