package adapter

import (
	"context"
	"net/http"

	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/logevent"
)

func logEventIdentityFromCorrelation(corr correlation.Context) logevent.Identity {
	return logevent.Identity{
		TraceID:              string(corr.TraceID),
		SpanID:               string(corr.SpanID),
		ParentSpanID:         string(corr.ParentSpanID),
		RequestID:            corr.RequestID,
		CursorRequestID:      corr.CursorRequestID,
		CursorConversationID: corr.CursorConversationID,
		CursorGenerationID:   corr.CursorGenerationID,
		UpstreamRequestID:    corr.UpstreamRequestID,
		UpstreamResponseID:   corr.UpstreamResponseID,
		ChatKey:              corr.ChatKey,
		ChatKeySource:        corr.ChatKeySource,
		ChatRootKey:          corr.ChatRootKey,
		ChatBranchKey:        corr.ChatBranchKey,
		SessionID:            "",
	}
}

func (s *Server) beginChatLogRecorder(r *http.Request, corr correlation.Context) *logevent.Recorder {
	emitter := s.requestLog
	if emitter == nil {
		return nil
	}
	var path logevent.Path
	path.Surface = logevent.SurfaceAdapterChat
	path.RouteFamily = logevent.RouteFamilyOpenAICompatible
	path.Path = r.URL.Path
	path.Method = r.Method
	path.Host = r.Host
	return emitter.Begin(logEventIdentityFromCorrelation(corr), path)
}

func (s *Server) emitChatRequestLeg(ctx context.Context, recorder *logevent.Recorder, leg logevent.Leg, phase logevent.Phase, status logevent.Status) {
	if recorder == nil {
		return
	}
	var event logevent.Event
	event.Path.Leg = leg
	event.Path.Phase = phase
	event.Outcome.Status = status
	event.Outcome.Duration = recorder.Duration()
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

func (s *Server) emitChatCursorMetadataLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	var event logevent.Event
	event.Path.Leg = logevent.LegAdapterCursorMetadata
	event.Path.Phase = logevent.PhaseCompleted
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.Duration = recorder.Duration()
	recorder.Emit(ctx, event)
}

func (s *Server) emitChatModelResolveLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, model ResolvedModel, effort string) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	var providerFacet logevent.ProviderFacets
	if model.Backend == BackendCodex {
		var codexFacet logevent.CodexFacet
		codexFacet.Model = model.ClaudeModel
		codexFacet.Effort = effort
		providerFacet.Codex = &codexFacet
	}
	if model.Backend == BackendAnthropic {
		var anthropicFacet logevent.AnthropicFacet
		anthropicFacet.Model = model.ClaudeModel
		providerFacet.Anthropic = &anthropicFacet
	}
	var event logevent.Event
	event.Path.Leg = logevent.LegAdapterModelResolve
	event.Path.Phase = logevent.PhaseCompleted
	event.Path.Backend = model.Backend
	event.Path.Provider = model.Backend
	event.Outcome.Status = logevent.StatusOK
	event.Outcome.Duration = recorder.Duration()
	event.Facets = providerFacet
	recorder.Emit(ctx, event)
}

func (s *Server) emitChatProviderSendStartedLeg(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, req ChatRequest, model ResolvedModel, effort string) {
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
	event.Facets = providerFacetsForChat(model, effort, req)
	recorder.Emit(ctx, event)
}

func (s *Server) completeChatDispatchLegs(ctx context.Context, recorder *logevent.Recorder, corr correlation.Context, req ChatRequest, model ResolvedModel, effort string) {
	if recorder == nil {
		return
	}
	recorder.UpdateIdentity(logEventIdentityFromCorrelation(corr))
	providerFacet := providerFacetsForChat(model, effort, req)
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
		event.Facets = providerFacet
		recorder.Emit(ctx, event)
	}
}

func providerFacetsForChat(model ResolvedModel, effort string, req ChatRequest) logevent.ProviderFacets {
	var providerFacet logevent.ProviderFacets
	if model.Backend == BackendCodex {
		var codexFacet logevent.CodexFacet
		codexFacet.Model = model.ClaudeModel
		codexFacet.Effort = effort
		if req.Reasoning != nil {
			codexFacet.ReasoningSummary = req.Reasoning.Summary
		}
		codexFacet.InputCount = len(req.Messages)
		codexFacet.ToolCount = len(req.Tools) + len(req.Functions)
		codexFacet.ServiceTier = req.ServiceTier
		providerFacet.Codex = &codexFacet
	}
	if model.Backend == BackendAnthropic {
		var anthropicFacet logevent.AnthropicFacet
		anthropicFacet.Model = model.ClaudeModel
		anthropicFacet.ThinkingEnabled = req.Reasoning != nil
		providerFacet.Anthropic = &anthropicFacet
	}
	return providerFacet
}

func (s *Server) completeChatLogRecorder(ctx context.Context, recorder *logevent.Recorder) {
	if recorder == nil {
		return
	}
	recorder.Complete(ctx)
}
