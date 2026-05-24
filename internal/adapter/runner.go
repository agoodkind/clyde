package adapter

import (
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/oauthrotation"
)

// Deps are the host hooks the adapter needs from the daemon process.
// The daemon owns the real implementations (findRealClaude and the
// scratch directory helper); the adapter accepts them as fields so
// the package stays testable without pulling the daemon in.
type Deps struct {
	// ResolveClaude returns the path to the real claude binary.
	ResolveClaude func() (string, error)
	// ScratchDir returns a clyde owned cwd for the subprocess.
	// Empty string is tolerated; the runner falls back to the
	// current working directory.
	ScratchDir func() string
	// RequestEvents receives normalized adapter request lifecycle
	// updates so the daemon can aggregate live provider stats.
	RequestEvents adapterruntime.RequestEventSink
	// RuntimeLogging carries logging settings that can be refreshed by
	// the daemon without reconstructing the adapter server.
	RuntimeLogging *RuntimeLogging
	// AnthropicMessagesURLOverride, when non-empty, replaces the
	// configured /v1/messages URL on the Anthropic client so its
	// outbound HTTP rides through the local MITM capture proxy.
	// The daemon populates this when [mitm].enabled_default is
	// true and the provider list includes "claude". The adapter
	// otherwise sends directly to api.anthropic.com.
	AnthropicMessagesURLOverride string
	// OAuthRotator is the single, daemon-owned OAuth rotation layer the
	// adapter shares with the daemon's periodic harvest-and-refresh loop.
	// When set, registerAnthropicProvider injects it instead of building
	// a per-server instance, so a token the refresh loop renews on disk
	// is reflected in the adapter's in-memory slot immediately rather
	// than only on the next re-import. When nil (tests, or when the
	// daemon has not built one), the adapter falls back to building its
	// own instance via buildAnthropicRotator.
	OAuthRotator *oauthrotation.Rotator
}
