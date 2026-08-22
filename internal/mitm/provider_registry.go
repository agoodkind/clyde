package mitm

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/providerid"
)

// ProviderID identifies an MITM-aware provider package.
type ProviderID providerid.Provider

const (
	// ProviderIDUnknown is the zero value for an unclaimed MITM provider.
	ProviderIDUnknown ProviderID = ProviderID(providerid.ProviderUnspecified)
	// ProviderIDClaude identifies Claude/Anthropic MITM traffic.
	ProviderIDClaude ProviderID = ProviderID(providerid.ProviderClaude)
	// ProviderIDCodex identifies Codex/OpenAI MITM traffic.
	ProviderIDCodex ProviderID = ProviderID(providerid.ProviderCodex)
	// ProviderIDCursor identifies Cursor MITM traffic.
	ProviderIDCursor ProviderID = ProviderID(providerid.ProviderCursor)
	// ProviderIDConductor identifies Conductor MITM traffic.
	ProviderIDConductor ProviderID = ProviderID(providerid.ProviderConductor)
)

// String returns the stable provider label used in capture logs.
func (id ProviderID) String() string {
	return providerid.Provider(id).String()
}

// LogValue emits the stable provider label in structured logs.
func (id ProviderID) LogValue() slog.Value {
	return slog.StringValue(id.String())
}

// ConnectClaim is a Provider's claim of a CONNECT target for TLS
// interception. When Claimed is true the generic CONNECT handler
// terminates the inbound TLS handshake against the configured MITM
// CA and serves the inner HTTP request loop instead of opaque-byte
// splicing. The Host field is the lowercased CONNECT host without
// the port; ProviderID identifies which registered provider owns the
// intercepted exchange.
type ConnectClaim struct {
	Claimed    bool
	Host       string
	ProviderID ProviderID
}

// PlainRouteClaim is a Provider's claim of a plain-HTTP forward-proxy
// request by URL path prefix. The generic proxy concatenates the
// request URI verbatim onto UpstreamURL. Provider is the typed
// provider name (used for logging concern keys); the value must
// equal the registered ProviderID.
type PlainRouteClaim struct {
	Claimed     bool
	Provider    string
	UpstreamURL string
}

// IdentityContribution carries the typed result of a provider's
// header-based identity extraction. PreferredRequestID and
// PreferredUpstreamRequestID feed the generic Identity fields when
// the correlation context did not already carry them. Facet is the
// provider-owned [logevent.Facet] attached to emitted events; the
// generic layer flushes it via the [logevent.Facets] bundle and
// never branches on its FacetKey. SessionID is the generic
// [logevent.Identity.SessionID] when a provider can recover a
// session identifier from headers. ConversationID is the native
// provider conversation or thread id used to derive the Clyde
// conversation_id stored on capture rows. ConversationSource tags
// how ConversationID was recovered.
type IdentityContribution struct {
	PreferredRequestID         string
	PreferredUpstreamRequestID string
	SessionID                  string
	ConversationID             string
	ConversationSource         string
	Facet                      logevent.Facet
}

// ExchangeDiagnostic is the typed per-intercepted-request input the
// optional [ExchangeDiagnostician] hook consumes. The generic MITM
// intercept loop populates every field from the completed exchange
// before invoking the diagnostician; implementations must not retain
// references to DecodedRequestBody beyond the call return. The struct
// carries only the fields a provider-specific diagnostic needs and is
// distinct from the unified capture leg, which the generic layer owns.
type ExchangeDiagnostic struct {
	RequestHeader      http.Header
	DecodedRequestBody []byte
	Method             string
	Path               string
	Host               string
	Concern            string
	HookName           string
}

// ExchangeDiagnostician is an optional extension a [Provider] may
// implement when it decodes provider-specific diagnostics from an
// intercepted exchange. The generic forward path type-asserts the
// claiming provider to this interface after the capture leg is
// recorded and, when present, invokes DiagnoseExchange so the provider
// can emit its own structured diagnostic on its wire concern log; it
// only logs. Providers that decode nothing (Claude, Codex) do not
// implement it.
type ExchangeDiagnostician interface {
	DiagnoseExchange(ctx context.Context, log *slog.Logger, exchange ExchangeDiagnostic)
}

// CaptureDecoder is an optional provider extension that creates a durable
// decoded representation for a raw request body captured by the shared store.
// It must return false for requests outside its supported wire contract.
type CaptureDecoder interface {
	DecodeCaptureRequest(exchange ExchangeDiagnostic) (capture.DecodedBody, bool)
}

// BodyConversationIdentifier is an optional provider extension for wire
// contracts that carry the conversation id in the request body rather than in
// a header. The generic layer calls it at the capture site, where the decoded
// body already exists, and only when header extraction produced no
// conversation id, so a provider that answers from headers never pays for it.
//
// Implementations return the native provider conversation id, not the derived
// Clyde id; the generic layer derives that once in captureConversationFields.
// They must return false for any request outside their supported contract, and
// must not retain the body beyond the call.
type BodyConversationIdentifier interface {
	ConversationIDFromBody(exchange ExchangeDiagnostic) (string, bool)
}

func bodyConversationIdentifierFor(providerName string) (BodyConversationIdentifier, bool) {
	for _, provider := range defaultRegistry.snapshot() {
		identifier, ok := provider.(BodyConversationIdentifier)
		if !ok || provider.ID().String() != providerName {
			continue
		}
		return identifier, true
	}
	return nil, false
}

func captureDecoderFor(providerName string, host string) (CaptureDecoder, bool) {
	for _, provider := range defaultRegistry.snapshot() {
		decoder, ok := provider.(CaptureDecoder)
		if !ok || provider.ID().String() != providerName {
			continue
		}
		return decoder, true
	}
	for _, provider := range defaultRegistry.snapshot() {
		decoder, ok := provider.(CaptureDecoder)
		if !ok || !provider.ClassifyConnect(normalizeConnectHost(host)).Claimed {
			continue
		}
		return decoder, true
	}
	return nil, false
}

// Provider is the MITM-package facing contract for a single provider
// such as Cursor, Claude (Anthropic), or Codex (OpenAI). Provider
// implementations live in internal/providers/<id>/mitmcontrib and
// self-register via init() into the MITM registry. The MITM package
// never imports a provider package; the registry inverts the
// dependency.
type Provider interface {
	// ID returns the stable provider identifier.
	ID() ProviderID

	// ClassifyConnect decides whether this provider claims the
	// CONNECT target host for TLS interception. The host argument is
	// the lowercased CONNECT host without the port. Implementations
	// must not block.
	ClassifyConnect(host string) ConnectClaim

	// ClassifyPlain decides whether this provider claims an upstream
	// URL for a plain-HTTP MITM request by path prefix. Path is the
	// URL path only (no query). Implementations must not block.
	ClassifyPlain(path string) PlainRouteClaim

	// ExtractIdentity returns the typed identity contribution parsed
	// from request headers. Provider implementations may return a
	// zero IdentityContribution when no provider-specific fields are
	// present; the generic layer ignores nil facets.
	ExtractIdentity(headers http.Header) IdentityContribution
}

// registry holds the registered providers.
type registry struct {
	mu        sync.RWMutex
	providers []Provider
}

var defaultRegistry = &registry{
	mu:        sync.RWMutex{},
	providers: nil,
}

// RegisterProvider installs a Provider into the MITM registry.
// Provider packages call this exactly once from an init() function
// at package load so internal/mitm never imports a provider package
// directly. A second registration with the same ProviderID replaces
// the prior registration (test fixtures rely on this).
func RegisterProvider(p Provider) {
	if p == nil {
		return
	}
	defaultRegistry.register(p)
}

func (r *registry) register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := p.ID()
	for i, existing := range r.providers {
		if existing.ID() == id {
			r.providers[i] = p
			return
		}
	}
	r.providers = append(r.providers, p)
}

func (r *registry) snapshot() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.providers) == 0 {
		return nil
	}
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

// providerForConnect walks the registered providers in registration
// order and returns the first one that claims the supplied CONNECT
// target. host is the raw target string ("host:port" or "host"); the
// helper extracts the lowercase host for the per-provider claim
// call.
func providerForConnect(target string) (Provider, ConnectClaim, bool) {
	host := normalizeConnectHost(target)
	for _, p := range defaultRegistry.snapshot() {
		claim := p.ClassifyConnect(host)
		if claim.Claimed {
			claim.Host = host
			claim.ProviderID = p.ID()
			return p, claim, true
		}
	}
	return nil, ConnectClaim{Claimed: false, Host: "", ProviderID: ProviderIDUnknown}, false
}

// providerForPlain returns the first registered provider that claims
// the supplied plain-HTTP path.
func providerForPlain(path string) (Provider, PlainRouteClaim, bool) {
	for _, p := range defaultRegistry.snapshot() {
		claim := p.ClassifyPlain(path)
		if claim.Claimed {
			return p, claim, true
		}
	}
	return nil, PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}, false
}

func normalizeConnectHost(target string) string {
	host := strings.TrimSpace(target)
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		hostOnly := host[:idx]
		if hostOnly != "" {
			host = hostOnly
		}
	}
	host = strings.Trim(strings.ToLower(host), "[].")
	return host
}
