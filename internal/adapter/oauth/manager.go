// Package oauth manages adapter OAuth token flows and persistence.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// Manager is goroutine-safe. One instance per daemon is enough.
type Manager struct {
	mu             sync.Mutex
	cached         *Tokens
	snapshot       credentialSnapshot
	httpClient     *http.Client
	credentialsDir string
	oauthCfg       config.AdapterOAuth
	relogin        reloginState
	platform       string
	securityBinary string
}

// NewManager builds a Manager. oauthCfg supplies token URL, client id,
// scopes, and keychain service name. credentialsDir defaults to
// $HOME/.claude when empty; the lock file lives directly in that
// directory.
func NewManager(oauthCfg config.AdapterOAuth, credentialsDir string) *Manager {
	if credentialsDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			credentialsDir = filepath.Join(home, ".claude")
		}
	}
	return &Manager{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		credentialsDir: credentialsDir,
		oauthCfg:       oauthCfg,
		platform:       runtime.GOOS,
		securityBinary: "security",
	}
}

// Token returns a non-expired access token, refreshing if needed.
// Concurrent callers share one in-flight refresh thanks to the
// per-process mutex; cross-process races are handled by the file
// lock and post-lock re-read.
func (m *Manager) Token(ctx context.Context) (string, error) {
	log := oauthLog.Logger()
	m.mu.Lock()
	defer m.mu.Unlock()

	selected, err := m.selectedCredential(ctx)
	if err != nil {
		return "", err
	}
	tokens := selected.Tokens
	if !isExpired(tokens) {
		log.DebugContext(ctx, "oauth.auth.cache_hit",
			"subcomponent", "oauth",
			"store_kind", selected.Source,
			"expires_at_ms", tokens.ExpiresAt,
			"fingerprint", selected.Metadata.Fingerprint,
			"access_token_present", selected.Metadata.AccessTokenPresent,
			"refresh_token_present", selected.Metadata.RefreshTokenPresent,
		)
		return tokens.AccessToken, nil
	}

	refreshStarted := oauthClock.Now()
	refreshable, refreshableErr := m.ensureRefreshableCandidate(ctx, selected)
	if refreshableErr != nil {
		log.ErrorContext(ctx, "oauth.store.unrefreshable",
			"subcomponent", "oauth",
			"duration_ms", time.Since(refreshStarted).Milliseconds(),
			"store_kind_summaries", summariesAsStrings(selected.Summaries),
			"err", refreshableErr,
		)
		return "", refreshableErr
	}
	refreshed, err := m.refreshLocked(ctx, refreshable)
	if err != nil {
		log.ErrorContext(ctx, "oauth.auth.refresh_failed",
			"subcomponent", "oauth",
			"duration_ms", time.Since(refreshStarted).Milliseconds(),
			"store_kind", refreshable.Source,
			"err", err,
		)
		if isInvalidGrant(err) {
			return m.tokenAfterInvalidGrant(ctx, refreshStarted, err)
		}
		return "", err
	}
	m.cacheTokens(refreshable.Source, refreshed)
	log.InfoContext(ctx, "oauth.auth.refreshed",
		"subcomponent", "oauth",
		"duration_ms", time.Since(refreshStarted).Milliseconds(),
		"store_kind", refreshable.Source,
		"expires_at_ms", refreshed.ExpiresAt,
		"fingerprint", oauthcredentials.Fingerprint(refreshed),
		"scopes", strings.Join(refreshed.Scopes, " "),
	)
	return refreshed.AccessToken, nil
}

func (m *Manager) tokenAfterInvalidGrant(ctx context.Context, refreshStarted time.Time, refreshErr error) (string, error) {
	log := oauthLog.Logger()
	log.InfoContext(ctx, "oauth.refresh.invalid_grant_detected",
		"subcomponent", "oauth",
	)
	if reErr := m.autoRelogin(ctx, refreshErr); reErr != nil {
		return "", reErr
	}
	fresh, readErr := m.reselectCredential(ctx)
	if readErr != nil {
		log.WarnContext(ctx, "oauth.store.post_relogin_read_failed",
			"subcomponent", "oauth",
			"store_dir", m.credentialsDir,
			"keychain_service_present", m.oauthCfg.KeychainService != "",
			"err", readErr.Error(),
		)
		return "", fmt.Errorf("post-relogin read credentials: %w", readErr)
	}
	if !isExpired(fresh.Tokens) {
		log.InfoContext(ctx, "oauth.auth.refreshed_via_relogin",
			"subcomponent", "oauth",
			"duration_ms", time.Since(refreshStarted).Milliseconds(),
			"store_kind", fresh.Source,
			"expires_at_ms", fresh.Tokens.ExpiresAt,
			"fingerprint", fresh.Metadata.Fingerprint,
		)
		return fresh.Tokens.AccessToken, nil
	}
	return m.refreshAfterRelogin(ctx, refreshStarted, fresh)
}

func (m *Manager) refreshAfterRelogin(ctx context.Context, refreshStarted time.Time, fresh *selectedCredential) (string, error) {
	log := oauthLog.Logger()
	retryCredential, retrySelectErr := m.ensureRefreshableCandidate(ctx, fresh)
	if retrySelectErr != nil {
		return "", fmt.Errorf("post-relogin select refreshable credentials: %w", retrySelectErr)
	}
	retried, retryErr := m.refreshLocked(ctx, retryCredential)
	if retryErr != nil {
		log.ErrorContext(ctx, "oauth.auth.post_relogin_refresh_failed",
			"subcomponent", "oauth",
			"duration_ms", time.Since(refreshStarted).Milliseconds(),
			"store_kind", retryCredential.Source,
			"err", retryErr.Error(),
		)
		return "", fmt.Errorf("post-relogin refresh: %w", retryErr)
	}
	m.cacheTokens(retryCredential.Source, retried)
	log.InfoContext(ctx, "oauth.auth.refreshed_via_relogin",
		"subcomponent", "oauth",
		"duration_ms", time.Since(refreshStarted).Milliseconds(),
		"store_kind", retryCredential.Source,
		"expires_at_ms", retried.ExpiresAt,
		"fingerprint", oauthcredentials.Fingerprint(retried),
	)
	return retried.AccessToken, nil
}

func (m *Manager) selectedCredential(ctx context.Context) (*selectedCredential, error) {
	if m.cached == nil {
		return m.reselectCredential(ctx)
	}
	current, err := m.readSelectedCredential(ctx)
	if err != nil {
		m.invalidateCacheForReadError(ctx, err)
		return nil, err
	}
	if reason := m.cacheInvalidationReason(current); reason != "" {
		m.logCacheInvalidated(ctx, reason, current)
		m.cacheSelectedCredential(current)
		return current, nil
	}
	if isExpired(m.cached) {
		return current, nil
	}
	metadata := oauthcredentials.NewMetadata(m.cached, oauthClock.Now(), m.snapshot.FileMtime)
	summaries := []oauthcredentials.Summary(nil)
	if current != nil {
		summaries = current.Summaries
	}
	return &selectedCredential{
		Source:    m.snapshot.Source,
		Tokens:    m.cached.Clone(),
		Metadata:  metadata,
		Summaries: summaries,
	}, nil
}

func (m *Manager) reselectCredential(ctx context.Context) (*selectedCredential, error) {
	selected, err := m.readSelectedCredential(ctx)
	if err != nil {
		return nil, err
	}
	m.cacheSelectedCredential(selected)
	oauthLog.Logger().InfoContext(ctx, "oauth.credentials.selected",
		"subcomponent", "oauth",
		"store_kind", selected.Source,
		"expires_at_ms", selected.Metadata.ExpiresAt,
		"fingerprint", selected.Metadata.Fingerprint,
		"access_token_present", selected.Metadata.AccessTokenPresent,
		"refresh_token_present", selected.Metadata.RefreshTokenPresent,
		"store_kind_summaries", summariesAsStrings(selected.Summaries),
	)
	return selected, nil
}

func (m *Manager) readSelectedCredential(ctx context.Context) (*selectedCredential, error) {
	results := readCredentialCandidates(ctx, m.readOptions())
	return selectCredentialCandidate(results)
}

func (m *Manager) readOptions() oauthcredentials.ReadOptions {
	return oauthcredentials.ReadOptions{
		CredentialsDir:  m.credentialsDir,
		KeychainService: m.oauthCfg.KeychainService,
		SecurityBinary:  m.securityBinary,
		Platform:        m.platform,
		Now:             oauthClock.Now(),
	}
}

func (m *Manager) cacheInvalidationReason(current *selectedCredential) string {
	if current == nil || current.Tokens == nil {
		return "authoritative_store_missing"
	}
	if m.snapshot.Source != current.Source {
		return "authoritative_store_source_changed"
	}
	if m.snapshot.Fingerprint != "" && current.Metadata.Fingerprint != m.snapshot.Fingerprint {
		return "authoritative_store_fingerprint_changed"
	}
	if current.Source == oauthcredentials.SourceFile &&
		m.snapshot.FileMtime != 0 &&
		current.Metadata.FileMtime != 0 &&
		current.Metadata.FileMtime != m.snapshot.FileMtime {
		return "authoritative_store_rewritten"
	}
	if m.cached.RefreshToken == "" && current.Tokens.RefreshToken != "" {
		return "cached_token_missing_refresh_token"
	}
	return ""
}

func (m *Manager) invalidateCacheForReadError(ctx context.Context, err error) {
	if m.cached == nil {
		return
	}
	reason := "authoritative_store_unreadable"
	summaries := []oauthcredentials.Summary(nil)
	var credentialErr *OAuthCredentialError
	if errors.As(err, &credentialErr) {
		summaries = credentialErr.Summaries
		if authoritativeStoreMissing(summaries) {
			reason = "authoritative_store_missing"
		}
	}
	m.logCacheInvalidatedWithSummaries(ctx, reason, nil, summaries)
	m.cached = nil
	m.snapshot = emptyCredentialSnapshot()
}

func authoritativeStoreMissing(summaries []oauthcredentials.Summary) bool {
	if len(summaries) == 0 {
		return false
	}
	for _, summary := range summaries {
		if summary.Present || summary.ParseError != "" {
			return false
		}
	}
	return true
}

func (m *Manager) logCacheInvalidated(ctx context.Context, reason string, current *selectedCredential) {
	summaries := []oauthcredentials.Summary(nil)
	if current != nil {
		summaries = current.Summaries
	}
	m.logCacheInvalidatedWithSummaries(ctx, reason, current, summaries)
}

func (m *Manager) logCacheInvalidatedWithSummaries(ctx context.Context, reason string, current *selectedCredential, summaries []oauthcredentials.Summary) {
	storeKind := m.snapshot.Source
	expiresAt := int64(0)
	fingerprint := ""
	accessTokenPresent := false
	refreshTokenPresent := false
	if current != nil {
		storeKind = current.Source
		expiresAt = current.Metadata.ExpiresAt
		fingerprint = current.Metadata.Fingerprint
		accessTokenPresent = current.Metadata.AccessTokenPresent
		refreshTokenPresent = current.Metadata.RefreshTokenPresent
	}
	oauthLog.Logger().InfoContext(ctx, "oauth.credentials.cache_invalidated",
		"subcomponent", "oauth",
		"reason", reason,
		"store_kind", storeKind,
		"previous_store_kind", m.snapshot.Source,
		"previous_fingerprint", m.snapshot.Fingerprint,
		"expires_at_ms", expiresAt,
		"fingerprint", fingerprint,
		"access_token_present", accessTokenPresent,
		"refresh_token_present", refreshTokenPresent,
		"store_kind_summaries", summariesAsStrings(summaries),
	)
}

func (m *Manager) cacheSelectedCredential(selected *selectedCredential) {
	m.cached = selected.Tokens.Clone()
	m.snapshot = snapshotForCredential(selected)
}

func emptyCredentialSnapshot() credentialSnapshot {
	return credentialSnapshot{
		Source:              "",
		Fingerprint:         "",
		ExpiresAt:           0,
		RefreshTokenPresent: false,
		FileMtime:           0,
	}
}

func (m *Manager) ensureRefreshableCandidate(ctx context.Context, selected *selectedCredential) (*selectedCredential, error) {
	if selected != nil && selected.Tokens != nil && selected.Tokens.RefreshToken != "" {
		return selected, nil
	}
	results := readCredentialCandidates(ctx, m.readOptions())
	refreshable, err := selectRefreshableCredential(results)
	if err != nil {
		return nil, err
	}
	m.cacheSelectedCredential(refreshable)
	oauthLog.Logger().InfoContext(ctx, "oauth.credentials.cache_invalidated",
		"subcomponent", "oauth",
		"reason", "cached_token_missing_refresh_token",
		"store_kind", refreshable.Source,
		"store_kind_summaries", summariesAsStrings(refreshable.Summaries),
	)
	return refreshable, nil
}

func (m *Manager) cacheTokens(source oauthcredentials.Source, tokens *Tokens) {
	m.cached = tokens.Clone()
	metadata := oauthcredentials.NewMetadata(tokens, oauthClock.Now(), m.fileMtimeForSource(source))
	m.snapshot = credentialSnapshot{
		Source:              source,
		Fingerprint:         metadata.Fingerprint,
		ExpiresAt:           metadata.ExpiresAt,
		RefreshTokenPresent: metadata.RefreshTokenPresent,
		FileMtime:           metadata.FileMtime,
	}
}

func (m *Manager) fileMtimeForSource(source oauthcredentials.Source) int64 {
	if source != oauthcredentials.SourceFile {
		return 0
	}
	return credentialsFileMtime(m.credentialsDir)
}

func credentialsFileMtime(credentialsDir string) int64 {
	info, err := os.Stat(filepath.Join(credentialsDir, ".credentials.json"))
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

// isExpired returns true when the token is past its expiry minus the
// safety window. Tokens with ExpiresAt == 0 (env-var inference-only
// tokens that the CLI synthesizes) are treated as never-expiring.
func isExpired(t *Tokens) bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	if t.ExpiresAt == 0 {
		return false
	}
	expiresAt := time.UnixMilli(t.ExpiresAt)
	return oauthClock.Now().Add(refreshSafetyWindow).After(expiresAt)
}
