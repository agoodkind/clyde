package codex

import adapterresolver "goodkind.io/clyde/internal/adapter/resolver"

// PreparedRequest retains the pure Codex transport payload selected before
// response headers commit. Runtime execution may enrich a copy with local
// installation and workspace metadata, but it never rebuilds this payload.
type PreparedRequest struct {
	Transport HTTPTransportRequest
	Resolved  adapterresolver.ResolvedRequest
}

// Prepare projects a resolved adapter request into the Codex transport payload
// without authentication, network, capture, retry, livetrack, or statistics
// work.
func (p *Provider) Prepare(req adapterresolver.ResolvedRequest) (PreparedRequest, error) {
	if p == nil {
		return PreparedRequest{}, ErrCodexProviderNotConfigured
	}
	resolved := req
	return PreparedRequest{
		Transport: BuildRequestWithConfig(req.OpenAI, &resolved, req.ProviderEffort().String(), RequestBuilderConfig{
			ReasoningSummary:               p.cfg.ReasoningSummary,
			InboundThinkingMaterialization: codexSummaryRenderStrategy(p.cfg.Reasoning.ResolvedRoundTripSummary()),
			RoundTripEncrypted:             RoundTripEncrypted(p.cfg.Reasoning.ResolvedRoundTripEncrypted()),
			RoundTripSummary:               RoundTripSummary(p.cfg.Reasoning.ResolvedRoundTripSummary()),
		}),
		Resolved: resolved,
	}, nil
}
