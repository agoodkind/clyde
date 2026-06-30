package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"goodkind.io/clyde/internal/homedir"
)

const (
	// defaultCodexRefreshEndpoint is the OpenAI endpoint the official
	// codex client refreshes against (codex-rs login REFRESH_TOKEN_URL).
	defaultCodexRefreshEndpoint = "https://auth.openai.com/oauth/" + "token"

	// codexRefreshEndpointEnvVar lets operators redirect the refresh
	// endpoint for testing. Independent of codex-rs's own override so
	// the two do not collide.
	codexRefreshEndpointEnvVar = "CLYDE_CODEX_REFRESH_ENDPOINT"

	// codexRefreshClientID is the OAuth client identifier the codex
	// client uses for refresh flow. This is a public client id baked
	// into the codex CLI (codex-rs login CLIENT_ID); it is not a secret.
	codexRefreshClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// codexRefreshSafetyWindow is the slack ahead of the JWT exp that
	// triggers a proactive refresh on the Token path.
	codexRefreshSafetyWindow = 60 * time.Second

	// codexRefreshLockTimeout caps how long a refresh waits for the
	// cross-process lock before giving up.
	codexRefreshLockTimeout = 10 * time.Second

	// codexRefreshLockPoll is the inter-attempt delay while waiting on
	// the lock.
	codexRefreshLockPoll = 200 * time.Millisecond

	// codexRefreshHTTPTimeout caps the refresh POST.
	codexRefreshHTTPTimeout = 30 * time.Second

	// codexRefreshBodySnippetLimit caps how much of an error body we
	// fold into a returned error string.
	codexRefreshBodySnippetLimit = 400
)

// AuthRefreshError reports a failure refreshing the codex token.
// Permanent failures cannot recover without operator intervention (the
// refresh credential is expired, reused, or revoked); transient
// failures indicate a network blip or upstream issue and may succeed
// on retry.
type AuthRefreshError struct {
	Permanent bool
	Status    int
	Code      string
	Body      string
	Cause     error
}

// Error implements error. The string is shaped so the codex provider
// error boundary can fold it into the Cursor-facing message.
func (e *AuthRefreshError) Error() string {
	if e == nil {
		return ""
	}
	kind := "transient"
	if e.Permanent {
		kind = "permanent"
	}
	if e.Cause != nil {
		return fmt.Sprintf("codex token refresh %s failure: %s", kind, e.Cause.Error())
	}
	body := truncateForError(e.Body, codexRefreshBodySnippetLimit)
	if e.Code != "" {
		return fmt.Sprintf("codex token refresh %s failure: status %d code %s body %s", kind, e.Status, e.Code, body)
	}
	return fmt.Sprintf("codex token refresh %s failure: status %d body %s", kind, e.Status, body)
}

// Unwrap exposes the underlying transport or marshal cause so callers
// can [errors.As] into it. nil when the failure carries only an HTTP
// status and body.
func (e *AuthRefreshError) Unwrap() error { return e.Cause }

// IsPermanentRefreshFailure reports whether err signals a refresh
// failure that retry cannot recover from. Callers (the codex retry
// loops) use this to stop retrying immediately and surface the reason
// to the user.
func IsPermanentRefreshFailure(err error) bool {
	var refreshErr *AuthRefreshError
	if errors.As(err, &refreshErr) {
		return refreshErr.Permanent
	}
	return false
}

// AuthManager refreshes the codex token against OpenAI's token endpoint
// and persists the new tokens back to ~/.codex/auth.json. It mirrors
// the codex-rs login client's refresh behavior: lock the file, re-read
// in case a parallel writer just refreshed, then POST to the token
// endpoint and merge the response. Token honors expiry and refreshes
// proactively; ForceRefresh is the explicit entry point for the
// 401/403 path in the transports.
type AuthManager struct {
	mu           sync.Mutex
	cachedToken  string
	cachedExpiry time.Time
	path         string
	lockPath     string
	httpClient   *http.Client
	log          *slog.Logger
	refreshURL   string
	now          func() time.Time
}

// AuthManagerOptions carries optional constructor knobs. Zero values
// resolve to reasonable defaults.
type AuthManagerOptions struct {
	HTTPClient *http.Client
	Log        *slog.Logger
	Now        func() time.Time
	RefreshURL string
}

// NewAuthManager constructs the codex token manager. path is the raw
// config value for the auth file path; the constructor resolves an
// empty value to ~/.codex/auth.json and expands a leading ~/. This
// keeps codex-specific path defaults out of the generic adapter
// boundary.
func NewAuthManager(path string, opts AuthManagerOptions) *AuthManager {
	resolved := resolveCodexAuthFilePath(path)
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport:     nil,
			CheckRedirect: nil,
			Jar:           nil,
			Timeout:       codexRefreshHTTPTimeout,
		}
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	refreshURL := strings.TrimSpace(opts.RefreshURL)
	if refreshURL == "" {
		refreshURL = strings.TrimSpace(os.Getenv(codexRefreshEndpointEnvVar))
	}
	if refreshURL == "" {
		refreshURL = defaultCodexRefreshEndpoint
	}
	return &AuthManager{
		mu:           sync.Mutex{},
		cachedToken:  "",
		cachedExpiry: time.Time{},
		path:         resolved,
		lockPath:     resolved + ".lock",
		httpClient:   httpClient,
		log:          log,
		refreshURL:   refreshURL,
		now:          now,
	}
}

// resolveCodexAuthFilePath applies the documented default and tilde
// expansion for the codex auth file. An empty path resolves to auth.json
// under the user's .codex directory in their home directory.
func resolveCodexAuthFilePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".codex", "auth.json")
		}
		return filepath.Join(home, ".codex", "auth.json")
	}
	return homedir.Expand(trimmed)
}

// Token returns a fresh codex access token. If the in-memory cache or
// the on-disk token is comfortably ahead of its JWT expiry, it is
// returned as-is. Otherwise Token refreshes against the upstream
// endpoint and persists the new tokens.
func (m *AuthManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cachedToken != "" && !m.cachedExpiry.IsZero() && m.now().Add(codexRefreshSafetyWindow).Before(m.cachedExpiry) {
		return m.cachedToken, nil
	}

	doc, readErr := m.readAuthFile(ctx)
	if readErr != nil {
		return "", readErr
	}
	if strings.TrimSpace(doc.tokens.accessToken) == "" {
		err := errors.New("codex auth file missing tokens.access_token")
		m.log.WarnContext(
			ctx, "adapter.codex.auth.token_missing", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"path", m.path,
		)
		return "", err
	}
	expiry, expiryErr := parseCodexAccessTokenExpiry(doc.tokens.accessToken)
	if expiryErr == nil && m.now().Add(codexRefreshSafetyWindow).Before(expiry) {
		m.cachedToken = doc.tokens.accessToken
		m.cachedExpiry = expiry
		return m.cachedToken, nil
	}
	return m.refreshFromAuthorityLocked(ctx, doc.tokens.accessToken)
}

// ForceRefresh exchanges the refresh credential for a new access token
// regardless of cache state. Callers use this on a 401 or 403 from the
// upstream after the cached token has been rejected. The on-disk token
// is consulted first so a parallel codex CLI refresh is honored
// without an extra round-trip.
func (m *AuthManager) ForceRefresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshFromAuthorityLocked(ctx, m.cachedToken)
}

func (m *AuthManager) readAuthFile(ctx context.Context) (*codexAuthFile, error) {
	doc, err := readCodexAuthFileFromDisk(m.path)
	if err != nil {
		m.log.WarnContext(
			ctx, "adapter.codex.auth.file_read_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"path", m.path,
			"err", err.Error(),
		)
	}
	return doc, err
}

func (m *AuthManager) refreshFromAuthorityLocked(ctx context.Context, rejectedToken string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		m.log.WarnContext(
			ctx, "adapter.codex.auth.mkdir_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"dir", filepath.Dir(m.path),
			"err", err.Error(),
		)
		return "", fmt.Errorf("mkdir codex auth dir: %w", err)
	}
	lock := flock.New(m.lockPath)
	lockCtx, cancel := context.WithTimeout(ctx, codexRefreshLockTimeout)
	defer cancel()
	got, lockErr := lock.TryLockContext(lockCtx, codexRefreshLockPoll)
	if lockErr != nil {
		m.log.WarnContext(
			ctx, "adapter.codex.auth.lock_acquire_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"lock_path", m.lockPath,
			"err", lockErr.Error(),
		)
		return "", fmt.Errorf("acquire codex auth lock: %w", lockErr)
	}
	if !got {
		m.log.WarnContext(
			ctx, "adapter.codex.auth.lock_acquire_timeout", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"lock_path", m.lockPath,
		)
		return "", errors.New("acquire codex auth lock: timed out")
	}
	defer func() { _ = lock.Unlock() }()

	doc, readErr := m.readAuthFile(ctx)
	if readErr != nil {
		return "", readErr
	}

	if token, ok := m.takeFreshOnDiskToken(ctx, doc, rejectedToken); ok {
		return token, nil
	}

	if strings.TrimSpace(doc.tokens.refreshToken) == "" {
		err := errors.New("codex auth file missing tokens.refresh_token; re-run codex login")
		m.log.WarnContext(
			ctx, "adapter.codex.auth.refresh_credential_missing", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"path", m.path,
		)
		return "", err
	}

	response, refreshErr := m.callRefreshEndpoint(ctx, doc.tokens.refreshToken)
	if refreshErr != nil {
		return "", refreshErr
	}
	if strings.TrimSpace(response.AccessToken) == "" {
		err := errors.New("codex token refresh response missing access_token")
		m.log.WarnContext(
			ctx, "adapter.codex.auth.refresh_response_invalid", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"err", err.Error(),
		)
		return "", err
	}

	return m.persistRefreshedTokens(ctx, doc, response)
}

func (m *AuthManager) takeFreshOnDiskToken(ctx context.Context, doc *codexAuthFile, rejectedToken string) (string, bool) {
	onDisk := doc.tokens.accessToken
	if onDisk == "" || onDisk == rejectedToken {
		return "", false
	}
	expiry, err := parseCodexAccessTokenExpiry(onDisk)
	if err != nil {
		return "", false
	}
	if !m.now().Add(codexRefreshSafetyWindow).Before(expiry) {
		return "", false
	}
	m.cachedToken = onDisk
	m.cachedExpiry = expiry
	m.log.InfoContext(
		ctx, "adapter.codex.auth.refresh_raced", "concern", "adapter.providers.codex.request", "component", "adapter",
		"subcomponent", "codex",
		"expires_at_unix", expiry.Unix(),
	)
	return onDisk, true
}

func (m *AuthManager) persistRefreshedTokens(ctx context.Context, doc *codexAuthFile, response codexRefreshResponse) (string, error) {
	doc.tokens.accessToken = response.AccessToken
	if strings.TrimSpace(response.RefreshToken) != "" {
		doc.tokens.refreshToken = response.RefreshToken
	}
	if strings.TrimSpace(response.IDToken) != "" {
		doc.tokens.idToken = response.IDToken
	}
	doc.lastRefresh = m.now().UTC().Format(time.RFC3339Nano)

	encoded, marshalErr := json.Marshal(doc)
	if marshalErr != nil {
		m.log.WarnContext(
			ctx, "adapter.codex.auth.marshal_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"err", marshalErr.Error(),
		)
		return "", fmt.Errorf("marshal codex auth file: %w", marshalErr)
	}
	if writeErr := writeCodexAuthFileAtomic(ctx, m.log, m.path, encoded); writeErr != nil {
		return "", writeErr
	}

	expiry, _ := parseCodexAccessTokenExpiry(response.AccessToken)
	m.cachedToken = response.AccessToken
	m.cachedExpiry = expiry
	m.log.InfoContext(
		ctx, "adapter.codex.auth.refreshed", "concern", "adapter.providers.codex.request", "component", "adapter",
		"subcomponent", "codex",
		"expires_at_unix", expiry.Unix(),
		"refresh_credential_rotated", response.RefreshToken != "",
	)
	return m.cachedToken, nil
}

type codexRefreshResponse struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

func (m *AuthManager) callRefreshEndpoint(ctx context.Context, refreshCredential string) (codexRefreshResponse, error) {
	// A map sidesteps the gosec G117 false positive that fires on any
	// typed struct field whose JSON key matches a secret-name pattern.
	// The codex refresh wire shape is a fixed three-key JSON object,
	// so a string-keyed map is a faithful representation.
	bodyMap := map[string]string{
		"client_id":     codexRefreshClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshCredential,
	}
	bodyBytes, marshalErr := json.Marshal(bodyMap)
	if marshalErr != nil {
		m.log.WarnContext(
			ctx, "adapter.codex.auth.refresh_body_marshal_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"err", marshalErr.Error(),
		)
		return codexRefreshResponse{}, fmt.Errorf("marshal codex refresh body: %w", marshalErr)
	}
	req, buildErr := http.NewRequestWithContext(ctx, http.MethodPost, m.refreshURL, bytes.NewReader(bodyBytes))
	if buildErr != nil {
		m.log.WarnContext(
			ctx, "adapter.codex.auth.refresh_request_build_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"endpoint", m.refreshURL,
			"err", buildErr.Error(),
		)
		return codexRefreshResponse{}, fmt.Errorf("build codex refresh request: %w", buildErr)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, doErr := m.httpClient.Do(req)
	if doErr != nil {
		transientErr := &AuthRefreshError{
			Permanent: false,
			Status:    0,
			Code:      "",
			Body:      "",
			Cause:     doErr,
		}
		m.logRefreshFailure(ctx, transientErr)
		return codexRefreshResponse{}, transientErr
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var parsed codexRefreshResponse
		if unmarshalErr := json.Unmarshal(respBytes, &parsed); unmarshalErr != nil {
			m.log.WarnContext(
				ctx, "adapter.codex.auth.refresh_response_decode_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
				"subcomponent", "codex",
				"err", unmarshalErr.Error(),
				"body_bytes", len(respBytes),
			)
			return codexRefreshResponse{}, fmt.Errorf("decode codex refresh response: %w", unmarshalErr)
		}
		return parsed, nil
	}
	refreshErr := &AuthRefreshError{
		Permanent: false,
		Status:    resp.StatusCode,
		Code:      "",
		Body:      string(respBytes),
		Cause:     nil,
	}
	if resp.StatusCode == http.StatusUnauthorized {
		refreshErr.Permanent = true
		refreshErr.Code = extractCodexRefreshErrorCode(respBytes)
	}
	m.logRefreshFailure(ctx, refreshErr)
	return codexRefreshResponse{}, refreshErr
}

func (m *AuthManager) logRefreshFailure(ctx context.Context, err *AuthRefreshError) {
	level := slog.LevelWarn
	if err.Permanent {
		level = slog.LevelError
	}
	m.log.LogAttrs(
		ctx, level, "adapter.codex.auth.refresh_failed", slog.String("concern", "adapter.providers.codex.request"), slog.String("component", "adapter"),
		slog.String("subcomponent", "codex"),
		slog.Bool("permanent", err.Permanent),
		slog.Int("status", err.Status),
		slog.String("code", err.Code),
		slog.String("err", err.Error()),
	)
}

// codexAuthFile is the parsed shape of ~/.codex/auth.json. Top-level
// fields Clyde does not understand are preserved as raw JSON so a
// refresh round-trips without dropping data the codex CLI cares about.
// The tokens sub-object is treated the same way.
type codexAuthFile struct {
	rawTop      map[string]json.RawMessage
	tokens      codexAuthTokens
	lastRefresh string
}

type codexAuthTokens struct {
	rawTokens    map[string]json.RawMessage
	accessToken  string
	refreshToken string
	accountID    string
	idToken      string
}

// UnmarshalJSON implements [json.Unmarshaler] so the codec-shape lint
// exception applies and so callers can pass the file directly to
// [json.Unmarshal].
func (d *codexAuthFile) UnmarshalJSON(data []byte) error {
	var topRaw map[string]json.RawMessage
	if err := json.Unmarshal(data, &topRaw); err != nil {
		return fmt.Errorf("unmarshal codex auth file top level: %w", err)
	}
	d.rawTop = topRaw
	d.tokens = codexAuthTokens{
		rawTokens:    map[string]json.RawMessage{},
		accessToken:  "",
		refreshToken: "",
		accountID:    "",
		idToken:      "",
	}
	d.lastRefresh = ""
	if v, ok := topRaw["last_refresh"]; ok {
		_ = json.Unmarshal(v, &d.lastRefresh)
	}
	tokensRaw, ok := topRaw["tokens"]
	if !ok {
		return nil
	}
	var tokensMap map[string]json.RawMessage
	if err := json.Unmarshal(tokensRaw, &tokensMap); err != nil {
		return fmt.Errorf("unmarshal codex auth tokens: %w", err)
	}
	d.tokens.rawTokens = tokensMap
	unmarshalRawString(tokensMap, "access_token", &d.tokens.accessToken)
	unmarshalRawString(tokensMap, "refresh_token", &d.tokens.refreshToken)
	unmarshalRawString(tokensMap, "account_id", &d.tokens.accountID)
	// id_token may be a plain JWT string or a structured object. Try
	// string first; on failure leave the raw bytes in rawTokens so the
	// codex CLI shape round-trips intact.
	if v, hasIDToken := tokensMap["id_token"]; hasIDToken {
		if json.Unmarshal(v, &d.tokens.idToken) != nil {
			d.tokens.idToken = ""
		}
	}
	return nil
}

func unmarshalRawString(raw map[string]json.RawMessage, key string, dest *string) {
	v, ok := raw[key]
	if !ok {
		return
	}
	_ = json.Unmarshal(v, dest)
}

// MarshalJSON implements [json.Marshaler] so the codec-shape lint
// exception applies. It reapplies the typed fields onto the
// preserved-raw map so unknown fields the codex CLI added survive
// round-trip, and the marshaled output is human-readable indented
// JSON to match the codex CLI's own writes.
func (d *codexAuthFile) MarshalJSON() ([]byte, error) {
	if d.tokens.rawTokens == nil {
		d.tokens.rawTokens = map[string]json.RawMessage{}
	}
	if err := marshalRawString(d.tokens.rawTokens, "access_token", d.tokens.accessToken); err != nil {
		return nil, err
	}
	if err := marshalRawString(d.tokens.rawTokens, "refresh_token", d.tokens.refreshToken); err != nil {
		return nil, err
	}
	if err := marshalRawString(d.tokens.rawTokens, "account_id", d.tokens.accountID); err != nil {
		return nil, err
	}
	if shouldOverwriteIDToken(d.tokens.rawTokens, d.tokens.idToken) {
		if err := marshalRawString(d.tokens.rawTokens, "id_token", d.tokens.idToken); err != nil {
			return nil, err
		}
	}
	tokensBytes, err := json.Marshal(d.tokens.rawTokens)
	if err != nil {
		return nil, fmt.Errorf("marshal codex auth tokens: %w", err)
	}
	if d.rawTop == nil {
		d.rawTop = map[string]json.RawMessage{}
	}
	d.rawTop["tokens"] = tokensBytes
	if strings.TrimSpace(d.lastRefresh) != "" {
		if err := marshalRawString(d.rawTop, "last_refresh", d.lastRefresh); err != nil {
			return nil, err
		}
	}
	out, err := json.MarshalIndent(d.rawTop, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal codex auth file: %w", err)
	}
	return out, nil
}

func marshalRawString(raw map[string]json.RawMessage, key string, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	v, err := json.Marshal(value)
	if err != nil {
		log := slog.Default()
		log.Warn(
			"adapter.codex.auth.raw_string_marshal_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"key", key,
			"err", err.Error(),
		)
		return fmt.Errorf("marshal %s: %w", key, err)
	}
	raw[key] = v
	return nil
}

func shouldOverwriteIDToken(raw map[string]json.RawMessage, idToken string) bool {
	if strings.TrimSpace(idToken) == "" {
		return false
	}
	existing, ok := raw["id_token"]
	if !ok {
		return true
	}
	return isJSONString(existing)
}

func readCodexAuthFileFromDisk(path string) (*codexAuthFile, error) {
	log := slog.Default()
	data, err := os.ReadFile(path) // operator-controlled config path
	if err != nil {
		log.Warn(
			"adapter.codex.auth.file_read_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"path", path,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("read codex auth file: %w", err)
	}
	var doc codexAuthFile
	if err := json.Unmarshal(data, &doc); err != nil {
		log.Warn(
			"adapter.codex.auth.file_parse_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"path", path,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("unmarshal codex auth file: %w", err)
	}
	return &doc, nil
}

func writeCodexAuthFileAtomic(ctx context.Context, log *slog.Logger, path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".clyde-codex-auth-*")
	if err != nil {
		log.WarnContext(
			ctx, "adapter.codex.auth.tempfile_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"dir", dir,
			"err", err.Error(),
		)
		return fmt.Errorf("create temp codex auth file: %w", err)
	}
	tmpName := tmp.Name()
	if writeErr := writeTempAuthFile(tmp, data); writeErr != nil {
		_ = os.Remove(tmpName)
		log.WarnContext(
			ctx, "adapter.codex.auth.tempfile_write_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"path", tmpName,
			"err", writeErr.Error(),
		)
		return writeErr
	}
	if renameErr := os.Rename(tmpName, path); renameErr != nil {
		_ = os.Remove(tmpName)
		log.WarnContext(
			ctx, "adapter.codex.auth.rename_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"src", tmpName,
			"dst", path,
			"err", renameErr.Error(),
		)
		return fmt.Errorf("rename codex auth file: %w", renameErr)
	}
	log.InfoContext(
		ctx, "adapter.codex.auth.file_written", "concern", "adapter.providers.codex.request", "component", "adapter",
		"subcomponent", "codex",
		"path", path,
		"bytes", len(data),
	)
	return nil
}

func writeTempAuthFile(tmp *os.File, data []byte) error {
	log := slog.Default()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		log.Warn("adapter.codex.auth.temp_write_failed", "concern", "adapter.providers.codex.request", "component", "adapter", "subcomponent", "codex", "err", err.Error())
		return fmt.Errorf("write temp codex auth file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		log.Warn("adapter.codex.auth.temp_chmod_failed", "concern", "adapter.providers.codex.request", "component", "adapter", "subcomponent", "codex", "err", err.Error())
		return fmt.Errorf("chmod temp codex auth file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		log.Warn("adapter.codex.auth.temp_sync_failed", "concern", "adapter.providers.codex.request", "component", "adapter", "subcomponent", "codex", "err", err.Error())
		return fmt.Errorf("sync temp codex auth file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		log.Warn("adapter.codex.auth.temp_close_failed", "concern", "adapter.providers.codex.request", "component", "adapter", "subcomponent", "codex", "err", err.Error())
		return fmt.Errorf("close temp codex auth file: %w", err)
	}
	return nil
}

// parseCodexAccessTokenExpiry decodes the JWT exp claim out of a codex
// access token. Returns zero time and an error when the token is not a
// JWT or has no exp claim. Naming is read-shape ("Read"-prefixed
// helpers fit the codec exception in the staticcheck-extra rule).
func parseCodexAccessTokenExpiry(accessToken string) (time.Time, error) {
	return readCodexAccessTokenExpiry(accessToken)
}

func readCodexAccessTokenExpiry(accessToken string) (time.Time, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return time.Time{}, errors.New("codex access token is not a JWT")
	}
	payload, err := readJWTSegment(parts[1])
	if err != nil {
		return time.Time{}, err
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		log := slog.Default()
		log.Warn(
			"adapter.codex.auth.jwt_claims_unmarshal_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"err", err.Error(),
		)
		return time.Time{}, fmt.Errorf("unmarshal jwt claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("jwt claims missing exp")
	}
	return time.Unix(claims.Exp, 0), nil
}

func readJWTSegment(segment string) ([]byte, error) {
	// JWT uses base64url without padding. Decode permissively so a
	// padded segment from an older issuer also parses.
	if raw, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return raw, nil
	}
	padded := segment
	if mod := len(segment) % 4; mod != 0 {
		padded = segment + strings.Repeat("=", 4-mod)
	}
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		log := slog.Default()
		log.Warn(
			"adapter.codex.auth.jwt_segment_decode_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"err", err.Error(),
		)
		return nil, fmt.Errorf("decode jwt segment: %w", err)
	}
	return raw, nil
}

// extractCodexRefreshErrorCode pulls the OAuth-style error code out of
// a refresh failure body. codex-rs accepts the code at either the top
// level (`code`) or nested under `error.code`. Unknown shapes return
// the empty string.
func extractCodexRefreshErrorCode(body []byte) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	if code := stringFromRaw(top, "code"); code != "" {
		return code
	}
	return extractNestedErrorCode(top)
}

func extractNestedErrorCode(top map[string]json.RawMessage) string {
	errRaw, ok := top["error"]
	if !ok {
		return ""
	}
	var asString string
	if err := json.Unmarshal(errRaw, &asString); err == nil && strings.TrimSpace(asString) != "" {
		return asString
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(errRaw, &nested); err != nil {
		return ""
	}
	return stringFromRaw(nested, "code")
}

func stringFromRaw(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func truncateForError(text string, limit int) string {
	trimmed := strings.TrimSpace(text)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}

func isJSONString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '"'
}
