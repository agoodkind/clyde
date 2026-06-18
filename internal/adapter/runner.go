package adapter

import (
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm/capture"
)

// Deps are the host hooks the adapter needs from the daemon process.
// The daemon owns the real implementations; the adapter accepts them
// as fields so the package stays testable without pulling the daemon in.
type Deps struct {
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
	// GetAuth returns the auth source for a provider when the host wants
	// to inject one. A nil result lets the adapter build the provider's
	// default auth manager.
	GetAuth func(adapterresolver.ProviderID) adapterprovider.AuthLookup
	// CaptureStore is the daemon's shared SQLite capture store. The Anthropic
	// client and Codex provider read their wire baseline (current baseline +
	// updated-at) from it at request time to project their outbound wire
	// identity, and when set the adapter records each outbound provider
	// exchange (BYOK egress) to it tagged by an adapter.<provider> client id
	// so the egress lands in mitm/capture.db without routing through the MITM
	// proxy. A nil store disables both baseline-driven identity and egress
	// recording; the Anthropic path then fails /v1/messages with an
	// operator-actionable HTTP 503, while the codex path falls back to its
	// compiled-in identity constants.
	CaptureStore *capture.Store
	// Group is the daemon lifecycle group the adapter attaches its ingress,
	// egress, and codex websocket session registries to. The daemon sets the
	// real group; when nil (isolated adapter tests), New constructs a throwaway
	// group so Attach stays the only registry-construction path.
	Group *livetrack.Group
}

func (d Deps) authForProvider(id adapterresolver.ProviderID) adapterprovider.AuthLookup {
	if d.GetAuth == nil {
		return nil
	}
	return d.GetAuth(id)
}
