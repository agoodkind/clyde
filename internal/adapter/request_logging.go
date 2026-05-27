package adapter

import (
	"context"
	"log/slog"
	"net/http"

	"goodkind.io/clyde/internal/adapter/backendfacet"
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

func (s *Server) emitChatPayloadLeg(ctx context.Context, recorder *logevent.Recorder, body []byte, contentType string, parseErr error) {
	if recorder == nil {
		return
	}
	payload := logevent.FilterPayload(body, contentType)
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
	event.Payload = &payload
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

func (s *Server) emitChatModelResolveLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, model ResolvedModel, effort string, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	var event logevent.Event
	event.Path.Leg = logevent.LegAdapterModelResolve
	event.Path.Phase = logevent.PhaseCompleted
	event.Path.Backend = model.Backend
	event.Path.Provider = model.Backend
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.Duration = recorder.Duration()
	attachBackendFacet(&event, model, effort, nil)
	attachFacets(&event, facets)
	recorder.Emit(ctx, event)
}

func (s *Server) emitChatProviderSendStartedLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, req ChatRequest, model ResolvedModel, effort string, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	var event logevent.Event
	event.Path.Leg = logevent.LegProviderSendStarted
	event.Path.Phase = logevent.PhaseStarted
	event.Path.Backend = model.Backend
	event.Path.Provider = model.Backend
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.Duration = recorder.Duration()
	attachBackendFacet(&event, model, effort, &req)
	attachFacets(&event, facets)
	recorder.Emit(ctx, event)
}

func (s *Server) completeChatDispatchLegs(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, req ChatRequest, model ResolvedModel, effort string, facets []logevent.Facet) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
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
		event.Path.Backend = model.Backend
		event.Path.Provider = model.Backend
		event.Outcome.Status = logevent.StatusOK
		event.Outcome.Duration = recorder.Duration()
		attachBackendFacet(&event, model, effort, &req)
		attachFacets(&event, facets)
		recorder.Emit(ctx, event)
	}
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
func attachBackendFacet(event *logevent.Event, model ResolvedModel, effort string, req *ChatRequest) {
	if event == nil {
		return
	}
	input := backendFacetInput(model, effort, req)
	facet := defaultBackendFacetRegistry.requestFacet(model.Backend, input)
	if facet != nil {
		event.Facets.Set(facet)
	}
}

func backendFacetInput(model ResolvedModel, effort string, req *ChatRequest) backendfacet.Input {
	input := backendfacet.Input{
		Backend:          model.Backend,
		Model:            model.ClaudeModel,
		Effort:           effort,
		ServiceTier:      "",
		ReasoningSummary: "",
		ThinkingEnabled:  false,
		InputCount:       0,
		ToolCount:        0,
		StreamEventCount: 0,
		RetryAttempt:     0,
	}
	if req == nil {
		return input
	}
	input.ServiceTier = req.ServiceTier
	input.ThinkingEnabled = req.Reasoning != nil
	input.InputCount = len(req.Messages)
	input.ToolCount = len(req.Tools) + len(req.Functions)
	if req.Reasoning != nil {
		input.ReasoningSummary = req.Reasoning.Summary
	}
	return input
}

func (s *Server) completeChatLogRecorder(ctx context.Context, recorder *logevent.Recorder) {
	if recorder == nil {
		return
	}
	recorder.Complete(ctx)
}
