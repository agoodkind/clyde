package contextcount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// CounterSource identifies a registered counting strategy.
type CounterSource string

const (
	// CounterSourceAPI selects a remote counting service.
	CounterSourceAPI CounterSource = "api"
	// CounterSourceProbe selects a local runtime probe.
	CounterSourceProbe CounterSource = "probe"
)

// Credentials provides a provider-owned secret to a counter factory.
type Credentials interface {
	Secret(ctx context.Context) (string, error)
}

// Clock supplies wall-clock timestamps for counters that log durations.
type Clock interface {
	Now() time.Time
}

// Deps carries provider-neutral dependencies for registered factories.
type Deps struct {
	Access      Credentials
	HomeDir     string
	ProjectPath string
	WorkDir     string
	Version     string
	Timeout     time.Duration
	Clock       Clock
}

// CounterFactory builds a configured counter from provider-neutral deps.
type CounterFactory func(Deps) (Counter, error)

type (
	registerFunc           func(CounterSource, CounterFactory) error
	newCounterFunc         func(CounterSource, Deps) (Counter, error)
	registryRegisterFunc   func(*Registry, CounterSource, CounterFactory) error
	registryNewCounterFunc func(*Registry, CounterSource, Deps) (Counter, error)
)

var (
	registerAnchor           registerFunc
	newCounterAnchor         newCounterFunc
	registryRegisterAnchor   registryRegisterFunc
	registryNewCounterAnchor registryNewCounterFunc
)

func init() {
	registerAnchor = Register
	newCounterAnchor = NewCounter
	registryRegisterAnchor = (*Registry).Register
	registryNewCounterAnchor = (*Registry).NewCounter
	if registerAnchor == nil ||
		newCounterAnchor == nil ||
		registryRegisterAnchor == nil ||
		registryNewCounterAnchor == nil {
		panic("context counter registry anchors are nil")
	}
}

var (
	// ErrCounterSourceMissing is returned when no factory exists for a source.
	ErrCounterSourceMissing = errors.New("context counter source is not registered")
	// ErrCounterSourceRegistered is returned when a source already has a factory.
	ErrCounterSourceRegistered = errors.New("context counter source is already registered")
)

type counterSourceError struct {
	cause  error
	source CounterSource
}

func (err counterSourceError) Error() string {
	return fmt.Sprintf("%s: %s", err.cause, err.source)
}

func (err counterSourceError) Unwrap() error {
	return err.cause
}

// Registry maps source names to counter factories.
type Registry struct {
	mu        sync.RWMutex
	factories map[CounterSource]CounterFactory
}

// NewRegistry creates an empty counter registry.
func NewRegistry() *Registry {
	return &Registry{
		mu:        sync.RWMutex{},
		factories: map[CounterSource]CounterFactory{},
	}
}

var defaultRegistry = NewRegistry()

// Register adds a counter factory to the process registry.
func Register(source CounterSource, factory CounterFactory) error {
	return defaultRegistry.Register(source, factory)
}

// Register adds a counter factory to the registry.
func (r *Registry) Register(source CounterSource, factory CounterFactory) error {
	normalizedSource := normalizeSource(source)
	if factory == nil {
		return fmt.Errorf("context counter factory is nil: %s", normalizedSource)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[normalizedSource]; exists {
		return counterSourceError{
			cause:  ErrCounterSourceRegistered,
			source: normalizedSource,
		}
	}
	r.factories[normalizedSource] = factory
	return nil
}

// NewCounter returns the counter registered for source.
func NewCounter(source CounterSource, deps Deps) (Counter, error) {
	return defaultRegistry.NewCounter(source, deps)
}

// NewCounter returns the counter registered for source.
func (r *Registry) NewCounter(source CounterSource, deps Deps) (Counter, error) {
	normalizedSource := normalizeSource(source)
	r.mu.RLock()
	factory, exists := r.factories[normalizedSource]
	r.mu.RUnlock()
	if !exists {
		return nil, counterSourceError{
			cause:  ErrCounterSourceMissing,
			source: normalizedSource,
		}
	}
	counter, err := factory(deps)
	if err != nil {
		slog.Warn("contextcount.registry.counter_create_failed",
			"component", "contextcount",
			"subcomponent", "registry",
			"source", normalizedSource,
			"err", err,
		)
		return nil, fmt.Errorf("create context counter %s: %w", normalizedSource, err)
	}
	return counter, nil
}

func normalizeSource(source CounterSource) CounterSource {
	trimmed := CounterSource(strings.TrimSpace(string(source)))
	if trimmed == "" {
		return CounterSourceAPI
	}
	return trimmed
}
