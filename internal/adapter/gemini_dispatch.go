package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

const antigravityAssistantRole = "assistant"

func (s *Server) dispatchAntigravityProvider(
	responseWriter http.ResponseWriter,
	httpRequest *http.Request,
	reqID string,
	resolved adapterresolver.ResolvedRequest,
	backend adapterprovider.Provider,
) {
	replayCtx := s.prepareGenericAntigravityReplay(httpRequest.Context(), &resolved.OpenAI)
	resolved.RequestID = reqID
	if reply := currentReply(replayCtx); reply != nil && resolved.Cursor.ConversationID == "" {
		conversation := reply.ref.ConversationID
		resolved.Correlation = clydeingress.WithChatIdentity(
			correlation.FromContext(replayCtx),
			conversation,
			"reply",
			conversation,
			"",
		)
		replayCtx = correlation.WithContext(replayCtx, resolved.Correlation)
	}

	ctx, lifecycle := s.beginProviderRequestLifecycle(
		replayCtx,
		&resolved,
		"oauth",
		reqID,
		resolved.Model,
		resolved.OpenAI.Stream,
	)
	if resolved.OpenAI.Stream {
		s.dispatchAntigravityStream(ctx, responseWriter, httpRequest, resolved, backend, lifecycle)

		return
	}

	collector := newProviderCollectorWriter()
	result, err := s.executeAntigravityProvider(ctx, resolved, backend, collector)
	lifecycle.terminal(ctx, result, err)
	if err != nil {
		s.respondAdapterError(responseWriter, httpRequest, antigravityDispatchError(resolved, err))

		return
	}

	s.writeAntigravityCompletion(
		ctx,
		responseWriter,
		httpRequest,
		resolved,
		result,
		collector.events,
	)
}

func (s *Server) dispatchAntigravityStream(
	ctx context.Context,
	responseWriter http.ResponseWriter,
	httpRequest *http.Request,
	resolved adapterresolver.ResolvedRequest,
	backend adapterprovider.Provider,
	lifecycle *providerRequestLifecycle,
) {
	alias := resolvedRequestAlias(&resolved)
	writer, err := newProviderStreamWriter(
		ctx,
		s,
		responseWriter,
		resolved.RequestID,
		alias,
		backend.ID().String(),
	)
	if err != nil {
		var result adapterprovider.Result
		lifecycle.terminal(ctx, result, err)
		s.respondAdapterError(
			responseWriter,
			httpRequest,
			adapterErrInternal("create Antigravity stream writer", err),
		)

		return
	}

	includeUsage := resolved.OpenAI.StreamOptions != nil &&
		resolved.OpenAI.StreamOptions.IncludeUsage
	writer.configureStreamUsage(includeUsage)
	writer.onStreamOpened = func() { lifecycle.streamOpened(ctx) }
	result, runErr := s.executeAntigravityProvider(ctx, resolved, backend, writer)
	lifecycle.terminal(ctx, result, runErr)
	if runErr != nil {
		s.respondAntigravityStreamError(
			ctx,
			responseWriter,
			httpRequest,
			writer,
			antigravityDispatchError(resolved, runErr),
		)

		return
	}

	if err := writer.finalizeStream(ctx, result, includeUsage); err != nil {
		s.respondAntigravityStreamError(
			ctx,
			responseWriter,
			httpRequest,
			writer,
			adapterErrInternal("complete Antigravity stream", err),
		)
	}
}

func (s *Server) respondAntigravityStreamError(
	ctx context.Context,
	responseWriter http.ResponseWriter,
	httpRequest *http.Request,
	writer *providerStreamWriter,
	err *adapterError,
) {
	if writer.headersWritten {
		if writeErr := writer.writeStreamError(ctx, err); writeErr != nil {
			s.log.WarnContext(ctx, "adapter.antigravity.stream_error_write_failed", "err", writeErr)
		}

		return
	}

	s.respondAdapterError(responseWriter, httpRequest, err)
}

func (s *Server) executeAntigravityProvider(
	ctx context.Context,
	resolved adapterresolver.ResolvedRequest,
	backend adapterprovider.Provider,
	writer adapterprovider.EventWriter,
) (adapterprovider.Result, error) {
	egressCtx, _, release := registerEgress(
		ctx,
		s.egressRegistry,
		egressSessionKindHTTP,
		EgressMeta{
			Provider:          backend.ID().String(),
			UpstreamURL:       s.cfg.Antigravity.ResolvedBaseURL(),
			UpstreamRequestID: "",
			AttemptNo:         0,
			ParentRequestID:   resolved.RequestID,
		},
	)
	defer release("antigravity.egress.done")

	result, err := backend.Execute(egressCtx, resolved, writer)
	if err != nil {
		s.log.WarnContext(
			egressCtx,
			"adapter.antigravity.execution_failed",
			"request_id",
			resolved.RequestID,
			"error_type",
			fmt.Sprintf("%T", err),
		)

		return result, fmt.Errorf("execute Antigravity request: %w", err)
	}

	return result, nil
}

func (s *Server) writeAntigravityCompletion(
	ctx context.Context,
	responseWriter http.ResponseWriter,
	httpRequest *http.Request,
	resolved adapterresolver.ResolvedRequest,
	result adapterprovider.Result,
	events []adapterrender.Event,
) {
	collected := adapterrender.CollectMessageWithNativePatchRepresentation(events, "")
	var message ChatMessage
	message.Role = antigravityAssistantRole
	message.Content = json.RawMessage(strconv.Quote(collected.Text))
	message.ToolCalls = collected.ToolCalls
	message.Reasoning = collected.Reasoning
	message.ReasoningContent = collected.Reasoning
	message.Refusal = collected.Refusal
	response := ChatResponse{
		ID:                resolved.RequestID,
		Object:            "chat.completion",
		Created:           clock.Now().Unix(),
		Model:             resolvedRequestAlias(&resolved),
		SystemFingerprint: result.SystemFingerprint,
		Choices: []ChatChoice{
			{
				Index:        0,
				Message:      message,
				FinishReason: normalizedProviderFinishReason(result),
				Logprobs:     nil,
			},
		},
		Usage: &result.Usage,
	}
	if currentReply(ctx) != nil {
		if err := s.saveCollectedReply(
			ctx,
			resolved.Provider.String(),
			&response,
			events,
			nil,
		); err != nil {
			s.respondAdapterError(
				responseWriter,
				httpRequest,
				adapterErrInternal("save Antigravity reply", err),
			)

			return
		}
	}

	body, err := marshalChatResponseForListener(ctx, response)
	if err != nil {
		s.respondAdapterError(
			responseWriter,
			httpRequest,
			adapterErrInternal("marshal Antigravity completion", err),
		)

		return
	}

	writeJSON(responseWriter, body)
}

func antigravityDispatchError(resolved adapterresolver.ResolvedRequest, err error) *adapterError {
	mapped := antigravityProviderAdapterError(err)
	mapped.Backend = resolved.Provider.String()
	mapped.ModelAlias = resolvedRequestAlias(&resolved)
	mapped.ResolvedModelName = resolved.Model

	return mapped
}
