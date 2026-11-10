// Package responsehook invokes provider-independent actions with registered
// provider response adapters.
package responsehook

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/mitm"
)

// Hook selects a registered provider response adapter for one action.
type Hook struct {
	action mitm.ResponseAction
}

// New declares a response hook for action.
func New(action mitm.ResponseAction) *Hook {
	return &Hook{action: action}
}

// MatchRequestResponse rejects compaction before provider response processing.
func (h *Hook) MatchRequestResponse(
	request mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	if h == nil || h.action == nil {
		return noMatch(), nil
	}
	if request.Purpose == mitm.RequestPurposeCompaction {
		return noMatch(), nil
	}
	adapter, ok := mitm.ResponseAdapterFor(request.Provider)
	if !ok || !adapter.MatchesResponse(request) {
		return noMatch(), nil
	}
	return mitm.RequestResponseHookMatch{
		Matched: true,
		Transformer: responseTransformer{
			request: request,
			adapter: adapter,
			action:  h.action,
		},
		RequestTransformer: nil,
		ContinueMatching:   false,
	}, nil
}

func noMatch() mitm.RequestResponseHookMatch {
	return mitm.RequestResponseHookMatch{
		Matched:            false,
		Transformer:        nil,
		RequestTransformer: nil,
		ContinueMatching:   false,
	}
}

type responseTransformer struct {
	request mitm.RequestResponseHookRequest
	adapter mitm.ProviderResponseAdapter
	action  mitm.ResponseAction
}

func (t responseTransformer) TransformResponse(
	ctx context.Context,
	response mitm.ResponseHookResponse,
) (mitm.ResponseHookResponse, error) {
	transformed, err := t.adapter.TransformResponse(ctx, t.request, response, t.action)
	if err != nil {
		slog.WarnContext(
			ctx,
			"mitm.response_action.transform_failed",
			"concern", "providers.mitm.wire",
			"provider", t.request.Provider,
			"err", err,
		)
		return transformed, fmt.Errorf("apply provider response adapter: %w", err)
	}
	return transformed, nil
}
