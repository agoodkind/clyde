// Package backendfacet declares the adapter logging extension point
// for backend-owned request-story facets.
package backendfacet

import "goodkind.io/clyde/internal/logevent"

// Input carries the primitive request metadata a backend facet
// factory may use. The generic adapter builds this value and reads
// only the returned logevent.Facet interface.
type Input struct {
	Backend          string
	Model            string
	Effort           string
	ServiceTier      string
	ReasoningSummary string
	ThinkingEnabled  bool
	InputCount       int
	ToolCount        int
	StreamEventCount int
	RetryAttempt     int
}

// Factory builds one backend-owned facet from primitive request
// metadata.
type Factory interface {
	RequestFacet(input Input) logevent.Facet
}

// FactoryFunc adapts a function into a Factory.
type FactoryFunc func(input Input) logevent.Facet

// RequestFacet calls f and returns nil when f is nil.
func (f FactoryFunc) RequestFacet(input Input) logevent.Facet {
	if f == nil {
		return nil
	}
	return f(input)
}

// Registrar is the adapter-owned registration surface provider
// packages call from the adapter composition root.
type Registrar interface {
	Register(backend string, factory Factory)
}
