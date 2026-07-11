package adapter

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// preparedResponsesProvider retains the exact concrete provider request that
// succeeded before the Responses writer commits headers.
type preparedResponsesProvider struct {
	provider  adapterresolver.ProviderID
	codex     *adaptercodex.PreparedRequest
	anthropic *anthropic.PreparedRequest
}

func responsesProviderContext(ctx context.Context, resolved adapterresolver.ResolvedRequest) context.Context {
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
			return preparedResponsesProvider{}, s.responsesProviderError(ctx, "prepare", resolved.Provider, adaptercodex.ErrCodexProviderNotConfigured)
		}
		prepared, err := s.codexProvider.Prepare(resolved)
		if err != nil {
			return preparedResponsesProvider{}, s.responsesProviderError(ctx, "prepare", resolved.Provider, err)
		}
		return preparedResponsesProvider{
			provider: adapterresolver.ProviderCodex, codex: &prepared, anthropic: nil,
		}, nil
	case adapterresolver.ProviderAnthropic:
		if s.anthropicProvider == nil {
			return preparedResponsesProvider{}, fmt.Errorf("prepare Anthropic Responses request: provider is not configured")
		}
		prepared, err := s.anthropicProvider.Prepare(ctx, resolved)
		if err != nil {
			return preparedResponsesProvider{}, s.responsesProviderError(ctx, "prepare", resolved.Provider, err)
		}
		return preparedResponsesProvider{
			provider: adapterresolver.ProviderAnthropic, codex: nil, anthropic: &prepared,
		}, nil
	default:
		return preparedResponsesProvider{}, fmt.Errorf("prepare Responses request: unsupported provider %q", resolved.Provider)
	}
}

// Execute runs the concrete request selected by prepareResponsesProvider.
func (p preparedResponsesProvider) Execute(ctx context.Context, w adapterprovider.EventWriter, s *Server) (adapterprovider.Result, error) {
	switch p.provider {
	case adapterresolver.ProviderCodex:
		if p.codex == nil || s.codexProvider == nil {
			return adapterprovider.Result{}, fmt.Errorf("execute prepared Codex Responses request: provider is not configured")
		}
		result, err := s.codexProvider.ExecutePrepared(ctx, *p.codex, w)
		if err != nil {
			return adapterprovider.Result{}, s.responsesProviderError(ctx, "execute", p.provider, err)
		}
		return result, nil
	case adapterresolver.ProviderAnthropic:
		if p.anthropic == nil || s.anthropicProvider == nil {
			return adapterprovider.Result{}, fmt.Errorf("execute prepared Anthropic Responses request: provider is not configured")
		}
		result, err := s.anthropicProvider.ExecutePrepared(ctx, *p.anthropic, w)
		if err != nil {
			return adapterprovider.Result{}, s.responsesProviderError(ctx, "execute", p.provider, err)
		}
		return result, nil
	default:
		return adapterprovider.Result{}, fmt.Errorf("execute prepared Responses request: unsupported provider %q", p.provider)
	}
}

func (s *Server) responsesProviderError(ctx context.Context, stage string, providerID adapterresolver.ProviderID, err error) error {
	wrapped := fmt.Errorf("%s %s Responses request: %w", stage, providerID, err)
	if s.log != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.responses.provider_failed", slog.String("concern", "adapter.providers.responses"),
			slog.String("stage", stage),
			slog.String("provider", providerID.String()),
			slog.Any("err", wrapped),
		)
	}
	return wrapped
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
	aerr := mapUpstreamForFamily(adapterRouteOpenAI, providerID.String(), 0, upstreamClassServerError, "", err.Error())
	aerr.Backend = resolved.Provider.String()
	aerr.ModelAlias = alias
	aerr.ResolvedModelName = resolved.Model
	aerr.Cause = err
	return aerr
}
