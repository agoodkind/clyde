// Package oauth manages adapter OAuth token flows and persistence.
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

type oauthRefreshRequest struct {
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
}

type oauthRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type oauthErrorResponse struct {
	Error string `json:"error"`
}

// refreshLocked performs the disk lock dance, double-checks for a
// concurrent refresh by another process, then calls the token
// endpoint and persists the new tokens. Caller must hold m.mu.
func (m *Manager) refreshLocked(ctx context.Context, current *selectedCredential) (*Tokens, error) {
	log := oauthLog.Logger()
	if current == nil || current.Tokens == nil || current.Tokens.RefreshToken == "" {
		return nil, errors.New("oauth token expired and no refresh_token available; re-run `claude /login`")
	}

	releaseLock, err := m.acquireRefreshLock(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseLock()

	if raced, racedErr := m.reselectCredential(ctx); racedErr == nil && raced != nil && !isExpired(raced.Tokens) {
		oauthLog.Logger().InfoContext(ctx, "oauth.token.refresh_raced",
			"subcomponent", "oauth",
			"store_kind", raced.Source,
			"expires_at_ms", raced.Tokens.ExpiresAt,
		)
		return raced.Tokens.Clone(), nil
	}

	refreshResp, err := m.postRefreshRequest(ctx, current)
	if err != nil {
		return nil, err
	}
	newTokens := tokensFromRefreshResponse(refreshResp, current.Tokens)
	if err := writeCredentials(ctx, m.readOptions(), current.Source, newTokens); err != nil {
		log.ErrorContext(ctx, "oauth.credentials.write_failed",
			"subcomponent", "oauth",
			"store_kind", current.Source,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("persist refreshed oauth credentials: %w", err)
	}
	return newTokens, nil
}

func (m *Manager) acquireRefreshLock(ctx context.Context) (func(), error) {
	log := oauthLog.Logger()
	if err := os.MkdirAll(m.credentialsDir, 0o700); err != nil {
		log.WarnContext(ctx, "oauth.store.mkdir_failed",
			"subcomponent", "oauth",
			"store_dir", m.credentialsDir,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("mkdir credentials dir: %w", err)
	}
	lockPath := filepath.Join(m.credentialsDir, ".clyde-oauth.lock")
	lock := flock.New(lockPath)

	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	got, err := lock.TryLockContext(lockCtx, 200*time.Millisecond)
	if err != nil {
		cancel()
		log.WarnContext(ctx, "oauth.lock.acquire_failed",
			"subcomponent", "oauth",
			"lock_path", lockPath,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("acquire oauth lock: %w", err)
	}
	if !got {
		cancel()
		log.WarnContext(ctx, "oauth.lock.acquire_timeout",
			"subcomponent", "oauth",
			"lock_path", lockPath,
		)
		return nil, errors.New("acquire oauth lock: timed out")
	}
	return func() {
		_ = lock.Unlock()
		cancel()
	}, nil
}

func (m *Manager) postRefreshRequest(ctx context.Context, current *selectedCredential) (oauthRefreshResponse, error) {
	log := oauthLog.Logger()
	requestPayload := oauthRefreshRequest{
		GrantType:    "refresh_token",
		RefreshToken: current.Tokens.RefreshToken,
		ClientID:     m.oauthCfg.ClientID,
		Scope:        strings.Join(m.oauthCfg.Scopes, " "),
	}
	body, err := json.Marshal(requestPayload)
	if err != nil {
		log.WarnContext(ctx, "oauth.refresh.body_marshal_failed",
			"subcomponent", "oauth",
			"scope_count", len(m.oauthCfg.Scopes),
			"client_id_present", m.oauthCfg.ClientID != "",
			"err", err.Error(),
		)
		return oauthRefreshResponse{}, fmt.Errorf("marshal refresh body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.oauthCfg.TokenURL, bytes.NewReader(body))
	if err != nil {
		log.WarnContext(ctx, "oauth.refresh.request_build_failed",
			"subcomponent", "oauth",
			"endpoint_url", m.oauthCfg.TokenURL,
			"body_bytes", len(body),
			"err", err.Error(),
		)
		return oauthRefreshResponse{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.WarnContext(ctx, "oauth.refresh.post_failed",
			"subcomponent", "oauth",
			"endpoint_url", m.oauthCfg.TokenURL,
			"err", err.Error(),
		)
		return oauthRefreshResponse{}, fmt.Errorf("post refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return oauthRefreshResponse{}, refreshStatusError(resp.Status, respBytes)
	}

	var refreshResp oauthRefreshResponse
	if err := json.Unmarshal(respBytes, &refreshResp); err != nil {
		log.WarnContext(ctx, "oauth.refresh.response_decode_failed",
			"subcomponent", "oauth",
			"status", resp.StatusCode,
			"body_bytes", len(respBytes),
			"err", err.Error(),
		)
		return oauthRefreshResponse{}, fmt.Errorf("decode refresh response: %w", err)
	}
	if refreshResp.AccessToken == "" {
		return oauthRefreshResponse{}, fmt.Errorf("refresh response missing access_token: status=%d body_bytes=%d", resp.StatusCode, len(respBytes))
	}
	return refreshResp, nil
}

func tokensFromRefreshResponse(refreshResp oauthRefreshResponse, current *Tokens) *Tokens {
	return &Tokens{
		AccessToken:      refreshResp.AccessToken,
		RefreshToken:     coalesce(refreshResp.RefreshToken, current.RefreshToken),
		ExpiresAt:        oauthClock.Now().UnixMilli() + refreshResp.ExpiresIn*1000,
		Scopes:           splitScopes(refreshResp.Scope, current.Scopes),
		SubscriptionType: current.SubscriptionType,
		RateLimitTier:    current.RateLimitTier,
	}
}

func refreshStatusError(status string, respBytes []byte) error {
	var errorResponse oauthErrorResponse
	oauthError := ""
	if err := json.Unmarshal(respBytes, &errorResponse); err == nil {
		oauthError = errorResponse.Error
	}
	if oauthError != "" {
		return fmt.Errorf("refresh failed: %s: oauth_error=%s body_bytes=%d", status, oauthError, len(respBytes))
	}
	return fmt.Errorf("refresh failed: %s: body_bytes=%d", status, len(respBytes))
}
