package adapter

import (
	"context"
	"errors"
	"sync"
	"time"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/gklog/correlation"
)

type providerRequestLifecycle struct {
	server     *Server
	request    *adapterresolver.ResolvedRequest
	path       string
	requestID  string
	modelID    string
	stream     bool
	started    time.Time
	streamOnce sync.Once
}

func (s *Server) beginProviderRequestLifecycle(
	ctx context.Context,
	request *adapterresolver.ResolvedRequest,
	path string,
	requestID string,
	modelID string,
	stream bool,
) (context.Context, *providerRequestLifecycle) {
	if adapterruntime.ExecutionIDFromContext(ctx) == "" {
		ctx = adapterruntime.WithExecutionID(ctx, newExecutionID())
	}
	lifecycle := &providerRequestLifecycle{
		server: s, request: request, path: path, requestID: requestID,
		modelID: modelID, stream: stream, started: clock.Now(), streamOnce: sync.Once{},
	}
	s.emitRequestStarted(ctx, request, path, requestID, modelID, stream)
	return ctx, lifecycle
}

func (l *providerRequestLifecycle) streamOpened(ctx context.Context) {
	if l == nil || !l.stream {
		return
	}
	l.streamOnce.Do(func() {
		l.server.emitRequestStreamOpened(ctx, l.request, l.path, l.requestID, l.modelID)
	})
}

func (l *providerRequestLifecycle) terminal(ctx context.Context, result adapterprovider.Result, runErr error) {
	if l == nil {
		return
	}
	stage := adapterruntime.RequestStageCompleted
	errorMessage := ""
	if runErr != nil {
		stage = adapterruntime.RequestStageFailed
		errorMessage = runErr.Error()
		if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			stage = adapterruntime.RequestStageCancelled
		}
	}
	backend := ""
	if l.request != nil {
		backend = l.request.Provider.String()
	}
	adapterruntime.LogTerminal(l.server.log, ctx, l.server.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:                      stage,
		Provider:                   providerName(l.request, l.path),
		Backend:                    backend,
		RequestID:                  l.requestID,
		ExecutionID:                adapterruntime.ExecutionIDFromContext(ctx),
		Alias:                      resolvedRequestAlias(l.request),
		ModelID:                    l.modelID,
		Stream:                     l.stream,
		FinishReason:               result.FinishReason,
		TokensIn:                   result.Usage.PromptTokens,
		TokensOut:                  result.Usage.CompletionTokens,
		CacheReadTokens:            result.Usage.CachedTokens(),
		CacheCreationTokens:        result.Usage.CacheWriteTokens,
		DerivedCacheCreationTokens: result.DerivedCacheCreationTokens,
		ToolCallCount:              result.ToolCallCount,
		ToolCallNames:              result.ToolCallNames,
		HasSubagentToolCall:        result.HasSubagentToolCall,
		DurationMs:                 clock.Since(l.started).Milliseconds(),
		Err:                        errorMessage,
		Correlation:                correlation.Context{},
	})
}
