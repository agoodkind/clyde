package contextcount

import (
	"context"
	"errors"
	"testing"
)

type fakeCredentials struct {
	value string
}

func (f fakeCredentials) Secret(context.Context) (string, error) {
	return f.value, nil
}

type fakeCounter struct {
	source CounterSource
	tokens int
}

func (f fakeCounter) Count(context.Context, Transcript) (int, error) {
	return f.tokens, nil
}

func (f fakeCounter) Source() CounterSource {
	return f.source
}

func TestRegistryRegistersAndLooksUpCounter(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	source := CounterSource("unit")
	err := registry.Register(source, func(Deps) (Counter, error) {
		return fakeCounter{source: source, tokens: 123}, nil
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	counter, err := registry.NewCounter(source, Deps{})
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}
	if counter.Source() != source {
		t.Fatalf("Source() = %q, want %q", counter.Source(), source)
	}
}

func TestRegistryDefaultsEmptySource(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	err := registry.Register(CounterSourceAPI, func(Deps) (Counter, error) {
		return fakeCounter{source: CounterSourceAPI, tokens: 321}, nil
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	counter, err := registry.NewCounter("", Deps{})
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}
	if counter.Source() != CounterSourceAPI {
		t.Fatalf("Source() = %q, want %q", counter.Source(), CounterSourceAPI)
	}
}

func TestRegistryMissingSourceError(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	_, err := registry.NewCounter(CounterSource("missing"), Deps{})
	if !errors.Is(err, ErrCounterSourceMissing) {
		t.Fatalf("NewCounter() error = %v, want ErrCounterSourceMissing", err)
	}
}

func TestRegistryDuplicateRegistrationError(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	source := CounterSource("dup")
	factory := func(Deps) (Counter, error) {
		return fakeCounter{source: source, tokens: 1}, nil
	}
	if err := registry.Register(source, factory); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	err := registry.Register(source, factory)
	if !errors.Is(err, ErrCounterSourceRegistered) {
		t.Fatalf("Register() error = %v, want ErrCounterSourceRegistered", err)
	}
}

func TestRegistryFakeCounterRoundTrip(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	source := CounterSourceProbe
	err := registry.Register(source, func(deps Deps) (Counter, error) {
		secret, secretErr := deps.Access.Secret(context.Background())
		if secretErr != nil {
			return nil, secretErr
		}
		return fakeCounter{source: source, tokens: len(secret)}, nil
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	credentials := fakeCredentials{value: "abcd"}
	counter, err := registry.NewCounter(source, Deps{Access: credentials})
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}
	tokens, err := counter.Count(context.Background(), Transcript{Model: "test"})
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if tokens != 4 {
		t.Fatalf("Count() = %d, want 4", tokens)
	}
}
