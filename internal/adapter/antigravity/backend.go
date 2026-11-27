package antigravity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/mitm/capture"
)

var errMissingAuth = errors.New("antigravity requires an OAuth credential source")

// Provider executes Gemini requests and translates the upstream response.
type Provider struct {
	auth            provider.AuthLookup
	client          *Client
	projects        *ProjectManager
	now             func() time.Time
	userAgent       string
	includeThoughts bool
}

var _ provider.Provider = (*Provider)(nil)

// ProviderOption configures an optional Antigravity dependency.
type ProviderOption struct {
	capture *capture.Store
}

// WithCaptureStore enables generation HTTP capture when the store records.
//
// The capture redacts credentials before it submits an exchange to the store.
//
//	backend, err := NewProvider(deps, WithCaptureStore(store))
func WithCaptureStore(store *capture.Store) ProviderOption {
	return ProviderOption{capture: store}
}

// NewProvider constructs the Antigravity backend.
//
// The daemon supplies authentication. Client configuration selects the
// upstream origin and provides a version when the app is absent.
//
//	backend, err := NewProvider(deps)
func NewProvider(deps provider.Deps, options ...ProviderOption) (*Provider, error) {
	if deps.Auth == nil {
		return nil, errMissingAuth
	}

	settings := deps.Config.Antigravity
	var captureStore *capture.Store

	for _, option := range options {
		captureStore = option.capture
	}

	client, err := NewClient(ClientOptions{
		BaseURL:    settings.ResolvedBaseURL(),
		HTTPClient: deps.HTTPClient,
		Capture:    captureStore,
	})
	if err != nil {
		slog.Warn("adapter.antigravity.client_init_failed")

		return nil, fmt.Errorf("initialize Antigravity HTTP client: %w", err)
	}

	version, err := ResolveClientVersion(settings.ClientVersion)
	if err != nil {
		slog.Warn("adapter.antigravity.client_version_failed")

		return nil, fmt.Errorf("initialize Antigravity client version: %w", err)
	}

	now := deps.Now
	if now == nil {
		now = clock.Now
	}

	return &Provider{
		auth:   deps.Auth,
		client: client,
		projects: NewProjectManager(ProjectManagerOptions{
			HTTPClient: client.httpClient,
			BaseURL:    client.baseURL,
			Project:    settings.Project,
			Logger:     deps.Logger,
		}),
		now:             now,
		userAgent:       ClientUserAgent(version),
		includeThoughts: settings.Reasoning.IncludeThoughts,
	}, nil
}

// ID returns the resolver identity for this backend.
//
// The adapter uses it when selecting a registered provider.
//
//	id := backend.ID()
func (p *Provider) ID() resolver.ProviderID { return resolver.ProviderAntigravity }

// Execute sends a resolved Gemini request and emits normalized stream events.
//
// It validates the request before reading credentials or calling Code Assist.
//
//	result, err := backend.Execute(ctx, request, writer)
func (p *Provider) Execute(
	ctx context.Context,
	request resolver.ResolvedRequest,
	writer provider.EventWriter,
) (provider.Result, error) {
	var result provider.Result

	body, err := BuildGenerateContent(request, p.includeThoughts)
	if err != nil {
		return result, err
	}

	token, fingerprint, err := p.authorization(ctx)
	if err != nil {
		return result, err
	}

	project, err := p.projects.Project(
		ctx,
		Credential{
			AccessToken:  token,
			RefreshToken: "",
			ExpiresAt:    time.Time{},
			Fingerprint:  fingerprint,
		},
	)
	if err != nil {
		return result, err
	}

	conversationID := strings.TrimSpace(request.Cursor.ConversationID)
	if conversationID == "" {
		conversationID = clydeingress.ChatKey(request.Correlation)
	}

	if conversationID == "" {
		conversationID = request.RequestID
	}

	envelope, err := NewCodeAssistEnvelope(
		body,
		EnvelopeMetadata{
			Model:          request.Model,
			Project:        project,
			ConversationID: conversationID,
			SessionID:      conversationID,
			UnixMillis:     p.now().UnixMilli(),
			UUID:           "",
			Step:           0,
		},
	)
	if err != nil {
		return result, err
	}

	response, err := p.client.Stream(ctx, token, envelope, p.userAgent)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()

	return ReadStream(ctx, response.Body, writer)
}

func (p *Provider) authorization(ctx context.Context) (string, string, error) {
	if manager, ok := p.auth.(*AuthManager); ok {
		return manager.tokenForProject(ctx)
	}

	token, err := p.auth.Token(ctx)
	if err != nil {
		slog.WarnContext(ctx, "adapter.antigravity.token_unavailable", "provider", p.ID())

		return "", "", fmt.Errorf("get Antigravity access token: %w", err)
	}

	digest := sha256.Sum256([]byte(token))

	return token, hex.EncodeToString(digest[:]), nil
}
