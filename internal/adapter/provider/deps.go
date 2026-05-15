package provider

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/clyde/internal/config"
)

// AuthLookup returns the bearer token the provider should attach to
// outbound requests. Implementations are expected to refresh the
// token silently when it is close to expiry. Returning an error
// causes the provider to fail the request before any wire call.
type AuthLookup interface {
	Token(ctx context.Context) (string, error)
}

// AuthRefresher extends AuthLookup with an explicit refresh entry
// point for providers that need to force a token rotation when the
// upstream rejects a still-cached token (HTTP 401 or 403). Returning
// an error means the refresh itself failed; the caller propagates it
// to the user. Implementations that cannot refresh should still
// satisfy AuthLookup and let the provider fall back to retry-without-
// refresh.
type AuthRefresher interface {
	AuthLookup
	ForceRefresh(ctx context.Context) (string, error)
}

// TelemetrySink is the typed surface a provider uses to emit
// per-request lifecycle events that should land in the daemon's
// structured request log (today: providerStats). The interface is
// narrow on purpose. Providers should not log directly to the daemon
// store; they go through this sink so the implementation can fan out
// to in-memory subscribers.
type TelemetrySink interface {
	RecordRequestStarted(event RequestStartedEvent)
	RecordRequestCompleted(event RequestCompletedEvent)
}

// RequestStartedEvent is the typed payload for the per-request
// lifecycle "started" record. It is intentionally upstream-agnostic.
type RequestStartedEvent struct {
	RequestID string
	Provider  string
	Alias     string
	Model     string
	Effort    string
	Stream    bool
	StartedAt time.Time
}

// RequestCompletedEvent is the typed payload for the per-request
// lifecycle "completed" record.
type RequestCompletedEvent struct {
	RequestID    string
	Provider     string
	Alias        string
	Model        string
	FinishReason string
	DurationMS   int64
	Result       Result
}

// Deps is the typed dependency container a Provider is constructed
// with at daemon startup. The dispatcher does not pass per-call
// dependencies; the provider closes over what it needs.
type Deps struct {
	Config     config.AdapterConfig
	Auth       AuthLookup
	Logger     *slog.Logger
	HTTPClient *http.Client
	Telemetry  TelemetrySink
	Now        func() time.Time
}
