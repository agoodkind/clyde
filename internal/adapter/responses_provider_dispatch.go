package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// preparedResponsesProvider retains provider preparation required before
// the Responses writer commits headers.
type preparedResponsesProvider struct {
	provider    adapterresolver.ProviderID
	codex       *adaptercodex.PreparedRequest
	anthropic   *anthropic.PreparedRequest
	antigravity *adapterresolver.ResolvedRequest
}

func responsesProviderPath(provider adapterresolver.ProviderID) string {
	if provider == adapterresolver.ProviderAnthropic ||
		provider == adapterresolver.ProviderAntigravity {
		return "oauth"
	}

	return "direct"
}

func responsesProviderContext(
	ctx context.Context,
	resolved adapterresolver.ResolvedRequest,
) context.Context {
	if resolved.Provider == adapterresolver.ProviderAnthropic {
		return anthropic.WithRequestID(ctx, resolved.RequestID)
	}

	return ctx
}

// prepareResponsesProvider keeps the Responses-only concrete provider switch
// separate from the generic provider.Provider registry used by Chat ingress.
func (s *Server) prepareResponsesProvider(
	ctx context.Context,
	resolved adapterresolver.ResolvedRequest,
) (preparedResponsesProvider, error) {
	switch resolved.Provider {
	case adapterresolver.ProviderCodex:
		if s.codexProvider == nil {
			return preparedResponsesProvider{}, s.responsesProviderError(
				ctx,
				"prepare",
				resolved.Provider,
				adaptercodex.ErrCodexProviderNotConfigured,
			)
		}

		prepared, err := s.codexProvider.Prepare(resolved)
		if err != nil {
			return preparedResponsesProvider{}, s.responsesProviderError(
				ctx,
				"prepare",
				resolved.Provider,
				err,
			)
		}

		return preparedResponsesProvider{
			provider:    adapterresolver.ProviderCodex,
			codex:       &prepared,
			anthropic:   nil,
			antigravity: nil,
		}, nil
	case adapterresolver.ProviderAnthropic:
		if s.anthropicProvider == nil {
			return preparedResponsesProvider{}, fmt.Errorf(
				"prepare Anthropic Responses request: provider is not configured",
			)
		}

		prepared, err := s.anthropicProvider.Prepare(ctx, resolved)
		if err != nil {
			return preparedResponsesProvider{}, s.responsesProviderError(
				ctx,
				"prepare",
				resolved.Provider,
				err,
			)
		}

		return preparedResponsesProvider{
			provider:    adapterresolver.ProviderAnthropic,
			codex:       nil,
			anthropic:   &prepared,
			antigravity: nil,
		}, nil
	case adapterresolver.ProviderAntigravity:
		if s.antigravityProvider == nil {
			return preparedResponsesProvider{}, upstreamUnavailableForProvider(
				resolved.Provider,
				&resolved,
				resolvedRequestAlias(&resolved),
			)
		}

		return preparedResponsesProvider{
			provider:    resolved.Provider,
			codex:       nil,
			anthropic:   nil,
			antigravity: &resolved,
		}, nil
	case BackendClaude, BackendPassthroughOverride:
		fallthrough
	default:
		return preparedResponsesProvider{}, fmt.Errorf(
			"prepare Responses request: unsupported provider %q",
			resolved.Provider,
		)
	}
}

// Execute runs the request selected by prepareResponsesProvider.
//
// The adapter selects the writer for a streaming or collected response.
//
//	result, err := prepared.Execute(ctx, writer, server)
func (p preparedResponsesProvider) Execute(
	ctx context.Context,
	w adapterprovider.EventWriter,
	s *Server,
) (adapterprovider.Result, error) {
	switch p.provider {
	case adapterresolver.ProviderCodex:
		if p.codex == nil || s.codexProvider == nil {
			return adapterprovider.Result{}, fmt.Errorf(
				"execute prepared Codex Responses request: provider is not configured",
			)
		}

		egressCtx, releaseEgress := s.codexEgressContext(
			ctx,
			p.codex.Resolved.RequestID,
			adaptercodex.WebsocketURL(""),
		)
		defer releaseEgress("codex.responses.done")
		result, err := s.codexProvider.ExecutePrepared(egressCtx, *p.codex, w)
		if err != nil {
			return adapterprovider.Result{}, s.responsesProviderError(
				ctx,
				"execute",
				p.provider,
				err,
			)
		}

		return result, nil
	case adapterresolver.ProviderAnthropic:
		if p.anthropic == nil || s.anthropicProvider == nil {
			return adapterprovider.Result{}, fmt.Errorf(
				"execute prepared Anthropic Responses request: provider is not configured",
			)
		}

		result, err := s.anthropicProvider.ExecutePrepared(ctx, *p.anthropic, w)
		if err != nil {
			return adapterprovider.Result{}, s.responsesProviderError(
				ctx,
				"execute",
				p.provider,
				err,
			)
		}

		return result, nil
	case adapterresolver.ProviderAntigravity:
		return s.executeAntigravityProvider(ctx, *p.antigravity, s.antigravityProvider, w)
	case BackendClaude, BackendPassthroughOverride:
		fallthrough
	default:
		return adapterprovider.Result{}, fmt.Errorf(
			"execute prepared Responses request: unsupported provider %q",
			p.provider,
		)
	}
}

func (s *Server) responsesProviderError(
	ctx context.Context,
	stage string,
	providerID adapterresolver.ProviderID,
	err error,
) error {
	if s.log != nil {
		s.log.LogAttrs(
			ctx,
			slog.LevelWarn,
			"adapter.responses.provider_failed",
			slog.String("concern", "adapter.providers.responses"),
			slog.String("stage", stage),
			slog.String("provider", providerID.String()),
			slog.String("error_type", fmt.Sprintf("%T", err)),
		)
	}

	return err
}

func responsesPreparedProviderError(
	providerID adapterresolver.ProviderID,
	alias string,
	resolved adapterresolver.ResolvedRequest,
	err error,
) *adapterError {
	if providerID == adapterresolver.ProviderAnthropic {
		return anthropicProviderAdapterError(err)
	}

	if providerID == adapterresolver.ProviderAntigravity {
		if mapped, ok := errors.AsType[*adapterError](err); ok {
			return mapped
		}

		return antigravityDispatchError(resolved, err)
	}

	var aerr *adapterError
	if providerID == adapterresolver.ProviderCodex {
		aerr = codexProviderAdapterError(err)
	} else {
		aerr = mapUpstreamForFamily(
			adapterRouteOpenAI,
			providerID.String(),
			0,
			upstreamClassServerError,
			"",
			err.Error(),
		)
	}

	aerr.Backend = resolved.Provider.String()
	aerr.ModelAlias = alias
	aerr.ResolvedModelName = resolved.Model
	aerr.Cause = err

	return aerr
}
