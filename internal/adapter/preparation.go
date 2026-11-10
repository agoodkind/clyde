package adapter

import (
	"context"
	"errors"
	"net/http"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/gklog/correlation"
)

func (s *Server) prepareChatDispatch(
	ctx context.Context,
	request *http.Request,
	corr correlation.Context,
	reqID string,
	chatReq *ChatRequest,
	ingressCtx ingresscontract.IngressContext,
	ingress ingresscontract.IngressContract,
	recorder *logevent.Recorder,
	resolvedOutput *adapterresolver.ResolvedRequest,
	bodyFacets []logevent.Facet,
	body []byte,
	discovery RequestDiscovery,
) (string, error) {
	var resolverErr error

	// The resolver is the authoritative single resolution path. It maps
	// the alias and reasoning effort to a typed ResolvedRequest carrying
	// provider identity, effort, budget, and the per-provider knobs the
	// dispatcher and backends consume directly.
	*resolvedOutput, resolverErr = resolveCursorChatRequest(
		openAIIngressSurface(ctx),
		*chatReq,
		adapterresolver.NewModelRegistryAdapter(s.modelRegistry()),
	)
	if resolverErr != nil {
		s.logChatResolveFailed(ctx, corr, reqID, *chatReq, ingressCtx, ingress, resolverErr)
		recorder.EmitError(ctx, "model_resolve_failed", resolverErr.Error())
		if invalidRequestErr, ok := errors.AsType[*adapterresolver.InvalidRequestError](
			resolverErr,
		); ok {
			return "", adapterErrInvalidRequest(invalidRequestErr.Error(), resolverErr)
		}

		return "", adapterErrModelNotFound(resolverErr.Error())
	}

	resolvedOutput.RequestID = reqID
	resolvedOutput.Correlation = corr
	effort := resolvedOutput.Effort.String()
	s.logChatResolved(ctx, corr, reqID, *chatReq, ingressCtx, ingress, resolvedOutput, effort)
	s.emitChatModelResolveLeg(ctx, recorder, corr, resolvedOutput, effort, bodyFacets)
	s.logResolverOutcome(ctx, corr, reqID, *chatReq, ingressCtx, resolvedOutput, nil)

	if err := s.applyBackendOverride(request, *chatReq, resolvedOutput, reqID); err != nil {
		recorder.EmitError(ctx, "backend_override_failed", err.Error())

		return effort, err
	}

	if contractErr := prepareDocumentedChat(
		ctx,
		recorder,
		body,
		chatReq,
		discovery,
		resolvedOutput,
	); contractErr != nil {
		return effort, contractErr
	}

	return effort, nil
}
