package adapter

import (
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
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
	// AnthropicWireBaselinePath is the absolute path to the daemon-owned
	// MITM v2 baseline (reference-v2.toml) the Anthropic client reads at
	// request time to project its outbound wire identity. The daemon
	// resolves it from [mitm].drift.upstreams["claude-code"].reference,
	// falling back to the default baseline root. There is no compiled-in
	// flavor data; a missing or invalid file makes /v1/messages fail with
	// an operator-actionable HTTP 503.
	AnthropicWireBaselinePath string
	// CodexWireBaselinePath is the absolute path to the daemon-owned MITM
	// v2 baseline (reference-v2.toml) the Codex provider reads at request
	// time to project its outbound wire identity (originator, openai-beta,
	// user-agent, beta-features, attestation). The daemon resolves it from
	// [mitm].drift.upstreams["codex-cli"].reference, falling back to the
	// default baseline root. Unlike the Anthropic path, a missing or
	// invalid file is NOT fatal: the codex egress falls back to its
	// compiled-in identity constants so a cold-start codex still works.
	CodexWireBaselinePath string
}

func (d Deps) authForProvider(id adapterresolver.ProviderID) adapterprovider.AuthLookup {
	if d.GetAuth == nil {
		return nil
	}
	return d.GetAuth(id)
}
