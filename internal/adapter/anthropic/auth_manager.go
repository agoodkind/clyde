package anthropic

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

const authRefreshSafetyWindow = 30 * time.Second

// AuthManager reads the operator's normal Claude login credentials. On macOS
// it reads the Claude Code keychain item, and on other platforms it reads the
// `.credentials.json` file under the Claude credentials directory.
type AuthManager struct {
	mu             sync.Mutex
	cached         *oauthcredentials.Tokens
	snapshot       authCredentialSnapshot
	credentialsDir string
	oauthCfg       config.AdapterOAuth
	platform       string
	now            func() time.Time
}

type authCredentialSnapshot struct {
	Source              oauthcredentials.Source
	Fingerprint         string
	ExpiresAt           int64
	RefreshTokenPresent bool
	FileMtime           int64
}

type selectedAuthCredential struct {
	Source   oauthcredentials.Source
	Tokens   *oauthcredentials.Tokens
	Metadata oauthcredentials.Metadata
	Results  []oauthcredentials.ReadResult
}

// AuthCredentialError describes an unusable local Claude login credential set.
type AuthCredentialError struct {
	Message string
	Summary []string
}

// Error returns the safe credential error message.
func (e *AuthCredentialError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Summary) == 0 {
		return e.Message
	}
	return e.Message + ": " + strings.Join(e.Summary, "; ")
}

// NewAuthManager builds the Claude auth manager used by the Anthropic adapter
// provider. credentialsDir defaults to `$HOME/.claude` when empty.
func NewAuthManager(oauthCfg config.AdapterOAuth, credentialsDir string) *AuthManager {
	if credentialsDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			credentialsDir = filepath.Join(home, ".claude")
		}
	}
	return &AuthManager{
		mu:             sync.Mutex{},
		cached:         nil,
		snapshot:       emptyAuthCredentialSnapshot(),
		credentialsDir: credentialsDir,
		oauthCfg:       oauthCfg,
		platform:       runtime.GOOS,
		now:            time.Now,
	}
}

// Token returns the current Claude access token from the authoritative local
// credential store. It re-reads the store before serving a cached token so a
// manual `claude auth login` is picked up without restarting Clyde.
func (m *AuthManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	selected, err := m.selectedCredential(ctx)
	if err != nil {
		return "", err
	}
	if tokenExpired(selected.Tokens, m.now().Add(authRefreshSafetyWindow)) {
		m.cached = nil
		m.snapshot = emptyAuthCredentialSnapshot()
		return "", &AuthCredentialError{
			Message: "Claude login access token is expired; run `claude auth login`",
			Summary: authResultsSummary(selected.Results),
		}
	}
	return selected.Tokens.AccessToken, nil
}

// ForceRefresh clears the cache and re-reads the normal Claude credential
// store. The caller compares the returned token with the rejected token and
// skips the retry when manual login has not changed the credential yet.
func (m *AuthManager) ForceRefresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cached = nil
	m.snapshot = emptyAuthCredentialSnapshot()
	selected, err := m.reselectCredential(ctx)
	if err != nil {
		return "", err
	}
	if tokenExpired(selected.Tokens, m.now().Add(authRefreshSafetyWindow)) {
		return "", &AuthCredentialError{
			Message: "Claude login access token is expired; run `claude auth login`",
			Summary: authResultsSummary(selected.Results),
		}
	}
	return selected.Tokens.AccessToken, nil
}

func (m *AuthManager) selectedCredential(ctx context.Context) (*selectedAuthCredential, error) {
	if m.cached == nil {
		return m.reselectCredential(ctx)
	}
	current, err := m.readSelectedCredential(ctx)
	if err != nil {
		m.invalidateCache(ctx, err)
		return nil, err
	}
	if reason := m.cacheInvalidationReason(current); reason != "" {
		m.logCacheInvalidated(ctx, reason, current, nil)
		m.cacheSelectedCredential(current)
		return current, nil
	}
	if tokenExpired(m.cached, m.now().Add(authRefreshSafetyWindow)) {
		return current, nil
	}
	return &selectedAuthCredential{
		Source:   m.snapshot.Source,
		Tokens:   m.cached.Clone(),
		Metadata: oauthcredentials.NewMetadata(m.cached, m.now(), m.snapshot.FileMtime),
		Results:  current.Results,
	}, nil
}

func (m *AuthManager) reselectCredential(ctx context.Context) (*selectedAuthCredential, error) {
	selected, err := m.readSelectedCredential(ctx)
	if err != nil {
		return nil, err
	}
	m.cacheSelectedCredential(selected)
	authLog().InfoContext(ctx, "adapter.claude.auth.credentials_selected", "concern", "adapter.providers.anthropic.oauth", "component", "adapter",
		"subcomponent", "anthropic_auth",
		"store_kind", selected.Source,
		"expires_at_ms", selected.Metadata.ExpiresAt,
		"fingerprint", selected.Metadata.Fingerprint,
		"access_token_present", selected.Metadata.AccessTokenPresent,
		"refresh_token_present", selected.Metadata.RefreshTokenPresent,
		"store_kind_summaries", authResultsSummary(selected.Results),
	)
	return selected, nil
}

func (m *AuthManager) readSelectedCredential(ctx context.Context) (*selectedAuthCredential, error) {
	results := oauthcredentials.ReadCandidates(ctx, m.readOptions())
	return selectAuthCredential(results)
}

func (m *AuthManager) readOptions() oauthcredentials.ReadOptions {
	keychainService := strings.TrimSpace(m.oauthCfg.KeychainService)
	if keychainService == "" {
		keychainService = oauthcredentials.DefaultKeychainService
	}
	return oauthcredentials.ReadOptions{
		CredentialsDir:  m.credentialsDir,
		KeychainService: keychainService,
		Platform:        m.platform,
		Now:             m.now(),
	}
}

func (m *AuthManager) cacheInvalidationReason(current *selectedAuthCredential) string {
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

func (m *AuthManager) invalidateCache(ctx context.Context, err error) {
	if m.cached == nil {
		return
	}
	m.cached = nil
	m.snapshot = emptyAuthCredentialSnapshot()
	m.logCacheInvalidated(ctx, "authoritative_store_unreadable", nil, err)
}

func (m *AuthManager) logCacheInvalidated(ctx context.Context, reason string, current *selectedAuthCredential, cause error) {
	storeKind := m.snapshot.Source
	expiresAt := int64(0)
	fingerprint := ""
	accessTokenPresent := false
	refreshTokenPresent := false
	results := []oauthcredentials.ReadResult(nil)
	if current != nil {
		storeKind = current.Source
		expiresAt = current.Metadata.ExpiresAt
		fingerprint = current.Metadata.Fingerprint
		accessTokenPresent = current.Metadata.AccessTokenPresent
		refreshTokenPresent = current.Metadata.RefreshTokenPresent
		results = current.Results
	}
	attrs := []slog.Attr{
		slog.String("component", "adapter"),
		slog.String("subcomponent", "anthropic_auth"),
		slog.String("reason", reason),
		slog.String("store_kind", string(storeKind)),
		slog.String("previous_store_kind", string(m.snapshot.Source)),
		slog.String("previous_fingerprint", m.snapshot.Fingerprint),
		slog.Int64("expires_at_ms", expiresAt),
		slog.String("fingerprint", fingerprint),
		slog.Bool("access_token_present", accessTokenPresent),
		slog.Bool("refresh_token_present", refreshTokenPresent),
		slog.Any("store_kind_summaries", authResultsSummary(results)),
	}
	if cause != nil {
		attrs = append(attrs, slog.String("err", cause.Error()))
	}
	authLog().LogAttrs(ctx, slog.LevelInfo, "adapter.claude.auth.cache_invalidated", append([]slog.Attr{slog.String("concern", "adapter.providers.anthropic.oauth")}, attrs...)...)
}

func (m *AuthManager) cacheSelectedCredential(selected *selectedAuthCredential) {
	m.cached = selected.Tokens.Clone()
	m.snapshot = authCredentialSnapshot{
		Source:              selected.Source,
		Fingerprint:         selected.Metadata.Fingerprint,
		ExpiresAt:           selected.Metadata.ExpiresAt,
		RefreshTokenPresent: selected.Metadata.RefreshTokenPresent,
		FileMtime:           selected.Metadata.FileMtime,
	}
}

func emptyAuthCredentialSnapshot() authCredentialSnapshot {
	return authCredentialSnapshot{
		Source:              "",
		Fingerprint:         "",
		ExpiresAt:           0,
		RefreshTokenPresent: false,
		FileMtime:           0,
	}
}

func selectAuthCredential(results []oauthcredentials.ReadResult) (*selectedAuthCredential, error) {
	var selected *oauthcredentials.ReadResult
	for i := range results {
		candidate := &results[i]
		if !authCandidateUsable(candidate) {
			continue
		}
		if selected == nil || authCandidateBetter(candidate, selected) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, &AuthCredentialError{
			Message: "no usable Claude login credentials found; run `claude auth login`",
			Summary: authResultsSummary(results),
		}
	}
	return &selectedAuthCredential{
		Source:   selected.Source,
		Tokens:   selected.Tokens.Clone(),
		Metadata: selected.Metadata,
		Results:  append([]oauthcredentials.ReadResult(nil), results...),
	}, nil
}

func authCandidateUsable(candidate *oauthcredentials.ReadResult) bool {
	return candidate != nil && candidate.Err == nil && candidate.Tokens != nil && candidate.Metadata.AccessTokenPresent
}

func authCandidateBetter(candidate *oauthcredentials.ReadResult, selected *oauthcredentials.ReadResult) bool {
	if candidate.Metadata.Expired != selected.Metadata.Expired {
		return !candidate.Metadata.Expired
	}
	if candidate.Metadata.RefreshTokenPresent != selected.Metadata.RefreshTokenPresent {
		return candidate.Metadata.RefreshTokenPresent
	}
	if candidate.Source != selected.Source {
		return candidate.Source == oauthcredentials.SourceKeychain
	}
	return candidate.Metadata.ExpiresAt > selected.Metadata.ExpiresAt
}

func authResultsSummary(results []oauthcredentials.ReadResult) []string {
	values := make([]string, 0, len(results))
	for _, result := range results {
		parseError := ""
		if result.Err != nil {
			parseError = result.Err.Error()
		}
		values = append(values, fmt.Sprintf("%s:present=%t:access=%t:refresh=%t:expired=%t:fingerprint=%s:error=%s",
			result.Source,
			result.Present,
			result.Metadata.AccessTokenPresent,
			result.Metadata.RefreshTokenPresent,
			result.Metadata.Expired,
			result.Metadata.Fingerprint,
			parseError,
		))
	}
	return values
}

func tokenExpired(tokens *oauthcredentials.Tokens, now time.Time) bool {
	if tokens == nil || tokens.AccessToken == "" {
		return true
	}
	if tokens.ExpiresAt == 0 {
		return false
	}
	return !now.Before(time.UnixMilli(tokens.ExpiresAt))
}

func authLog() *slog.Logger {
	return anthropicRequestLog.Logger()
}

var (
	_ OAuthRefresher                = (*AuthManager)(nil)
	_ adapterprovider.AuthRefresher = (*AuthManager)(nil)
)
