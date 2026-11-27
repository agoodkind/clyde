package antigravity

import (
	"log/slog"

	"goodkind.io/clyde/internal/adapter/backendfacet"
	"goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/logevent"
)

type requestFacet struct {
	model  string
	effort string
}

func (facet requestFacet) FacetKey() string {
	return model.BackendAntigravity.String()
}

func (facet requestFacet) FacetAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("model", facet.model),
		slog.String("effort", facet.effort),
	}
}

func (facet requestFacet) SinkHints() logevent.SinkHints {
	return logevent.SinkHints{NeedsProviderSidecar: false}
}

// RegisterBackendFacet registers Antigravity request metadata.
//
// The adapter supplies the resolved model and reasoning effort.
//
//	RegisterBackendFacet(registry)
func RegisterBackendFacet(registry backendfacet.Registrar) {
	registry.Register(
		model.BackendAntigravity.String(),
		backendfacet.FactoryFunc(func(input backendfacet.Input) logevent.Facet {
			return requestFacet{model: input.Model, effort: input.Effort}
		}),
	)
}
