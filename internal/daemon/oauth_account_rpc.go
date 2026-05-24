package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/oauthrotation"
	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// defaultOAuthProvider is the provider name the CLI defaults to when a caller
// supplies no explicit provider. It matches the only provider the daemon
// registers today; the handlers stay provider-neutral and accept any
// registered name.
const defaultOAuthProvider = "anthropic"

// OAuthAccountList returns rotation-store account snapshots. An empty provider
// lists accounts across every registered provider; a named provider lists only
// that provider's accounts. It is read-only and delegates to the daemon-owned
// rotator.
func (s *Server) OAuthAccountList(ctx context.Context, req *clydev1.OAuthAccountListRequest) (*clydev1.OAuthAccountListResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	rotator := s.OAuthRotator()
	if rotator == nil {
		return nil, status.Error(codes.FailedPrecondition, "oauth rotation is not enabled; set [adapter].direct_oauth = true")
	}
	names := oauthProviderSelection(rotator, req.GetProvider())
	out := make([]*clydev1.OAuthAccountSnapshot, 0, len(names))
	for _, name := range names {
		snapshots, err := rotator.Accounts(ctx, name)
		if err != nil {
			s.log.WarnContext(ctx, "daemon.oauth_account.list_provider_failed",
				"component", "daemon",
				"subcomponent", "oauth_rotation",
				"provider", string(name),
				"err", err,
			)
			return nil, status.Errorf(codes.Internal, "list accounts for provider %q: %v", name, err)
		}
		for _, snapshot := range snapshots {
			out = append(out, protoOAuthAccountSnapshot(name, snapshot))
		}
	}
	s.log.InfoContext(ctx, "daemon.oauth_account.listed",
		"component", "daemon",
		"subcomponent", "oauth_rotation",
		"peer_addr", daemonPeerAddr(peerInfo),
		"providers", len(names),
		"accounts", len(out),
	)
	return &clydev1.OAuthAccountListResponse{Accounts: out}, nil
}

// OAuthAccountLogin drives an interactive login through the daemon-owned
// rotator and streams two events: an authorize event carrying the URL the
// operator must visit, then a result event carrying the account that the login
// produced. The stream registers with the RPC livetrack registry like the other
// daemon streams so reload drain accounts for it.
func (s *Server) OAuthAccountLogin(req *clydev1.OAuthAccountLoginRequest, stream clydev1.ClydeService_OAuthAccountLoginServer) error {
	streamCtx, streamCancel := context.WithCancel(stream.Context())
	defer streamCancel()
	draining := &atomic.Bool{}
	if s.RPCs != nil {
		peerInfo, _ := peer.FromContext(streamCtx)
		corr := correlation.FromContext(streamCtx)
		rpcSess, err := s.RPCs.Register(streamCtx, "daemon.rpc.oauth_account_login", RPCMeta{
			Method:     "/clyde.v1.ClydeService/OAuthAccountLogin",
			Direction:  "inbound",
			PeerActor:  daemonPeerAddr(peerInfo),
			RequestID:  corr.RequestID,
			TraceID:    string(corr.TraceID),
			LeaseToken: "",
		}, rpcCloser{cancel: streamCancel, draining: draining})
		if err != nil {
			if errors.Is(err, livetrack.ErrRegistryClosed) {
				return status.Error(codes.Unavailable, "daemon streaming RPC registry is draining")
			}
			return status.Errorf(codes.Internal, "register oauth login stream RPC: %v", err)
		}
		defer s.RPCs.Release(streamCtx, rpcSess, "oauth_account_login.done")
	}

	rotator := s.OAuthRotator()
	if rotator == nil {
		return status.Error(codes.FailedPrecondition, "oauth rotation is not enabled; set [adapter].direct_oauth = true")
	}
	name := provider.Name(oauthProviderOrDefault(req.GetProvider()))
	opts := provider.LoginOptions{
		Email: strings.TrimSpace(req.GetEmail()),
		Label: strings.TrimSpace(req.GetLabel()),
	}

	var sendErr error
	result, loginErr := rotator.Login(streamCtx, name, opts, func(authorizeURL string) {
		event := &clydev1.OAuthAccountLoginEvent{
			Event: &clydev1.OAuthAccountLoginEvent_Authorize{
				Authorize: &clydev1.OAuthAccountLoginAuthorize{AuthorizeUrl: authorizeURL},
			},
		}
		if err := stream.Send(event); err != nil {
			sendErr = err
		}
	})
	if sendErr != nil {
		return status.Errorf(codes.Internal, "send authorize url event: %v", sendErr)
	}
	if loginErr != nil {
		s.log.WarnContext(streamCtx, "daemon.oauth_account.login_failed",
			"component", "daemon",
			"subcomponent", "oauth_rotation",
			"provider", string(name),
			"err", loginErr,
		)
		return status.Errorf(codes.Internal, "oauth login for provider %q: %v", name, loginErr)
	}

	snapshot, err := oauthAccountSnapshotByID(streamCtx, s.log, rotator, name, result.Account)
	if err != nil {
		return status.Errorf(codes.Internal, "load logged-in account snapshot: %v", err)
	}
	resultEvent := &clydev1.OAuthAccountLoginEvent{
		Event: &clydev1.OAuthAccountLoginEvent_Result{
			Result: &clydev1.OAuthAccountLoginResult{Account: snapshot},
		},
	}
	if err := stream.Send(resultEvent); err != nil {
		return status.Errorf(codes.Internal, "send login result event: %v", err)
	}
	s.log.InfoContext(streamCtx, "daemon.oauth_account.login_completed",
		"component", "daemon",
		"subcomponent", "oauth_rotation",
		"provider", string(name),
		"account", string(result.Account),
		"label", result.Label,
	)
	return nil
}

// OAuthAccountForget removes one account from a provider by account id or label
// and reports whether a match was removed.
func (s *Server) OAuthAccountForget(ctx context.Context, req *clydev1.OAuthAccountForgetRequest) (*clydev1.OAuthAccountForgetResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	rotator := s.OAuthRotator()
	if rotator == nil {
		return nil, status.Error(codes.FailedPrecondition, "oauth rotation is not enabled; set [adapter].direct_oauth = true")
	}
	idOrLabel := strings.TrimSpace(req.GetIdOrLabel())
	if idOrLabel == "" {
		return nil, status.Error(codes.InvalidArgument, "id_or_label is required")
	}
	name := provider.Name(oauthProviderOrDefault(req.GetProvider()))
	result, err := rotator.Forget(ctx, name, idOrLabel)
	if err != nil {
		s.log.WarnContext(ctx, "daemon.oauth_account.forget_failed",
			"component", "daemon",
			"subcomponent", "oauth_rotation",
			"provider", string(name),
			"id_or_label", idOrLabel,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "forget account %q for provider %q: %v", idOrLabel, name, err)
	}
	s.log.InfoContext(ctx, "daemon.oauth_account.forget",
		"component", "daemon",
		"subcomponent", "oauth_rotation",
		"peer_addr", daemonPeerAddr(peerInfo),
		"provider", string(name),
		"id_or_label", idOrLabel,
		"forgotten", result.Forgotten,
		"account", string(result.Account),
	)
	return &clydev1.OAuthAccountForgetResponse{
		Forgotten: result.Forgotten,
		AccountId: string(result.Account),
	}, nil
}

// oauthProviderSelection resolves the requested provider filter to the list of
// provider names to enumerate. An empty filter selects every registered
// provider; a named filter selects exactly that one.
func oauthProviderSelection(rotator *oauthrotation.Rotator, requested string) []provider.Name {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return rotator.ProviderNames()
	}
	return []provider.Name{provider.Name(trimmed)}
}

// oauthProviderOrDefault returns the requested provider name or the default
// when the caller supplied none.
func oauthProviderOrDefault(requested string) string {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return defaultOAuthProvider
	}
	return trimmed
}

// oauthAccountSnapshotByID returns the snapshot for one account after a login,
// so the result event carries the same typed fields a list returns.
func oauthAccountSnapshotByID(ctx context.Context, log *slog.Logger, rotator *oauthrotation.Rotator, name provider.Name, account provider.AccountID) (*clydev1.OAuthAccountSnapshot, error) {
	snapshots, err := rotator.Accounts(ctx, name)
	if err != nil {
		log.ErrorContext(ctx, "daemon.oauth_account.snapshot_load_failed",
			"component", "daemon",
			"subcomponent", "oauth_rotation",
			"provider", string(name),
			"account", string(account),
			"err", err,
		)
		return nil, fmt.Errorf("load accounts for provider %q: %w", name, err)
	}
	for _, snapshot := range snapshots {
		if snapshot.Account == account {
			return protoOAuthAccountSnapshot(name, snapshot), nil
		}
	}
	// The account was logged in but is not yet visible; return a minimal typed
	// snapshot rather than failing the whole login.
	return &clydev1.OAuthAccountSnapshot{
		Provider:            string(name),
		AccountId:           string(account),
		Label:               "",
		ExpiresAtMs:         0,
		RefreshTokenPresent: false,
		ThrottledUntilMs:    0,
		ThrottledClaim:      "",
		NeedsReauth:         false,
	}, nil
}

// protoOAuthAccountSnapshot maps a rotator snapshot to its proto shape.
func protoOAuthAccountSnapshot(name provider.Name, snapshot oauthrotation.AccountSnapshot) *clydev1.OAuthAccountSnapshot {
	throttledUntilMS := int64(0)
	if snapshot.Throttled && !snapshot.ThrottledTo.IsZero() {
		throttledUntilMS = snapshot.ThrottledTo.UnixMilli()
	}
	expiresAtMS := int64(0)
	if !snapshot.ExpiresAt.IsZero() {
		expiresAtMS = snapshot.ExpiresAt.UnixMilli()
	}
	return &clydev1.OAuthAccountSnapshot{
		Provider:            string(name),
		AccountId:           string(snapshot.Account),
		Label:               snapshot.Label,
		ExpiresAtMs:         expiresAtMS,
		RefreshTokenPresent: snapshot.RefreshTokenPresent,
		ThrottledUntilMs:    throttledUntilMS,
		ThrottledClaim:      snapshot.Claim,
		NeedsReauth:         snapshot.NeedsReauth,
	}
}
