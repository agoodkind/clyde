package adapter

import (
	"context"
	"log/slog"
	"net/http"

	"goodkind.io/clyde/internal/adapter/backendfacet"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/gklog/correlation"
)

// logEventIdentityFromCorrelation builds the generic typed
// [logevent.Identity] from a correlation context. Provider-specific
// identity fields are attached separately via provider-owned
// [logevent.Facet] values returned by the ingress contract.
func logEventIdentityFromCorrelation(corr correlation.Context) logevent.Identity {
	return logevent.Identity{
		TraceID:            string(corr.TraceID),
		SpanID:             string(corr.SpanID),
		ParentSpanID:       string(corr.ParentSpanID),
		RequestID:          corr.RequestID,
		UpstreamRequestID:  clydeingress.UpstreamRequestID(corr),
		UpstreamResponseID: clydeingress.UpstreamResponseID(corr),
		ChatKey:            clydeingress.ChatKey(corr),
		ChatKeySource:      clydeingress.ChatKeySource(corr),
		ChatRootKey:        clydeingress.ChatRootKey(corr),
		ChatBranchKey:      clydeingress.ChatBranchKey(corr),
		SessionID:          "",
	}
}

func (s *Server) beginChatLogRecorder(r *http.Request, corr correlation.Context) *logevent.Recorder {
	emitter := s.requestLog
	if emitter == nil {
		return nil
	}
	var path logevent.Path
	path.Surface = logevent.SurfaceAdapterChat
	path.RouteFamily = logevent.RouteFamilyChatCompatible
	path.Path = r.URL.Path
	path.Method = r.Method
	path.Host = r.Host
	return emitter.Begin(logEventIdentityFromCorrelation(corr), path)
}

func (s *Server) emitChatRequestLeg(ctx context.Context, recorder *logevent.Recorder, leg logevent.Leg, phase logevent.Phase, status logevent.Status, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	var event logevent.Event
	event.Path.Leg = leg
	event.Path.Phase = phase
	event.Outcome.Status = status
	event.Outcome.Duration = recorder.Duration()
	attachFacets(&event, facets)
	recorder.Emit(ctx, event)
}

func (s *Server) emitChatPayloadLeg(ctx context.Context, recorder *logevent.Recorder, body []byte, parseErr error) {
	if recorder == nil {
		return
	}
	status := logevent.StatusOK
	errorMessage := ""
	if parseErr != nil {
		status = logevent.StatusError
		errorMessage = parseErr.Error()
	}
	var event logevent.Event
	event.Path.Leg = logevent.LegAdapterPayload
	event.Path.Phase = logevent.PhaseCompleted
	event.Outcome.Status = status
	event.Outcome.ErrorMessage = errorMessage
	event.Outcome.BytesIn = int64(len(body))
	event.Outcome.Duration = recorder.Duration()
	recorder.Emit(ctx, event)
}

func (s *Server) emitChatClientMetadataLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	var event logevent.Event
	event.Path.Leg = logevent.LegAdapterClientMetadata
	event.Path.Phase = logevent.PhaseCompleted
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.Duration = recorder.Duration()
	attachFacets(&event, facets)
	recorder.Emit(ctx, event)
}

func (s *Server) emitChatModelResolveLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, req *adapterresolver.ResolvedRequest, effort string, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	backend := resolvedRequestBackendName(req)
	var event logevent.Event
	event.Path.Leg = logevent.LegAdapterModelResolve
	event.Path.Phase = logevent.PhaseCompleted
	event.Path.Backend = backend
	event.Path.Provider = backend
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.Duration = recorder.Duration()
	attachBackendFacet(&event, req, effort, nil)
	attachFacets(&event, facets)
	recorder.Emit(ctx, event)
}

func (s *Server) emitChatProviderSendStartedLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, chatReq ChatRequest, req *adapterresolver.ResolvedRequest, effort string, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	backend := resolvedRequestBackendName(req)
	var event logevent.Event
	event.Path.Leg = logevent.LegProviderSendStarted
	event.Path.Phase = logevent.PhaseStarted
	event.Path.Backend = backend
	event.Path.Provider = backend
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.Duration = recorder.Duration()
	attachBackendFacet(&event, req, effort, &chatReq)
	attachFacets(&event, facets)
	recorder.Emit(ctx, event)
}

func (s *Server) completeChatDispatchLegs(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, chatReq ChatRequest, req *adapterresolver.ResolvedRequest, effort string, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	backend := resolvedRequestBackendName(req)
	legs := []logevent.Leg{
		logevent.LegProviderAccepted,
		logevent.LegProviderResponseStarted,
		logevent.LegProviderResponseDone,
		logevent.LegAdapterRender,
		logevent.LegAdapterClientEgress,
	}
	for _, leg := range legs {
		var event logevent.Event
		event.Path.Leg = leg
		event.Path.Phase = logevent.PhaseCompleted
		event.Path.Backend = backend
		event.Path.Provider = backend
		event.Outcome.Status = logevent.StatusOK
		event.Outcome.Duration = recorder.Duration()
		attachBackendFacet(&event, req, effort, &chatReq)
		attachFacets(&event, facets)
		recorder.Emit(ctx, event)
	}
}

// resolvedRequestBackendName returns the backend label for a resolved
// request, empty-safe for a nil request.
func resolvedRequestBackendName(req *adapterresolver.ResolvedRequest) string {
	if req == nil {
		return ""
	}
	return req.Provider.String()
}

func attachFacets(event *logevent.Event, facets []logevent.Facet) {
	if event == nil {
		return
	}
	for _, facet := range facets {
		event.Facets.Set(facet)
	}
}

func appendFacetSlogAttrs(attrs []slog.Attr, facets []logevent.Facet) []slog.Attr {
	for _, facet := range facets {
		if facet == nil {
			continue
		}
		facetAttrs := facet.FacetAttrs()
		if len(facetAttrs) == 0 {
			continue
		}
		attrs = append(attrs, slog.Attr{
			Key:   facet.FacetKey(),
			Value: slog.GroupValue(facetAttrs...),
		})
	}
	return attrs
}

// attachBackendFacet asks the registered backend factory for the
// provider-owned facet and attaches it through the logevent.Facet
// interface.
func attachBackendFacet(event *logevent.Event, req *adapterresolver.ResolvedRequest, effort string, chatReq *ChatRequest) {
	if event == nil {
		return
	}
	input := backendFacetInput(req, effort, chatReq)
	facet := defaultBackendFacetRegistry.requestFacet(resolvedRequestBackendName(req), input)
	if facet != nil {
		event.Facets.Set(facet)
	}
}

func backendFacetInput(req *adapterresolver.ResolvedRequest, effort string, chatReq *ChatRequest) backendfacet.Input {
	model := ""
	if req != nil {
		model = req.Model
	}
	input := backendfacet.Input{
		Backend:          resolvedRequestBackendName(req),
		Model:            model,
		Effort:           effort,
		ServiceTier:      "",
		ReasoningSummary: "",
		ThinkingEnabled:  false,
		InputCount:       0,
		ToolCount:        0,
		StreamEventCount: 0,
		RetryAttempt:     0,
	}
	if chatReq == nil {
		return input
	}
	input.ServiceTier = chatReq.ServiceTier
	input.ThinkingEnabled = chatReq.Reasoning != nil
	input.InputCount = len(chatReq.Messages)
	input.ToolCount = len(chatReq.Tools) + len(chatReq.Functions)
	if chatReq.Reasoning != nil {
		input.ReasoningSummary = chatReq.Reasoning.Summary
	}
	return input
}

func (s *Server) completeChatLogRecorder(ctx context.Context, recorder *logevent.Recorder) {
	if recorder == nil {
		return
	}
	recorder.Complete(ctx)
}
