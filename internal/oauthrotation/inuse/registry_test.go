package inuse

import (
	"context"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// stubDetector returns a fixed Set so tests can verify Lookup returns the
// registered value rather than the noop default.
type stubDetector struct {
	marker provider.AccountID
}

func (s stubDetector) Detect(_ context.Context, _ []provider.AccountID) (Set, error) {
	return NewSet(s.marker), nil
}

func TestLookupReturnsNoopWhenUnregistered(t *testing.T) {
	det := Lookup(provider.Name("unregistered-provider"))
	if _, ok := det.(NoopDetector); !ok {
		t.Fatalf("Lookup of unregistered provider returned %T, want NoopDetector", det)
	}
	set, err := det.Detect(context.Background(), []provider.AccountID{"acct-a"})
	if err != nil {
		t.Fatalf("NoopDetector.Detect: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("NoopDetector returned set len %d, want 0", set.Len())
	}
}

func TestRegisterThenLookupReturnsRegistered(t *testing.T) {
	name := provider.Name("test-register-lookup")
	t.Cleanup(func() { Register(name, nil) })

	Register(name, stubDetector{marker: provider.AccountID("acct-stub")})
	det := Lookup(name)
	set, err := det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !set.Contains("acct-stub") {
		t.Fatalf("stub set missing acct-stub, len=%d", set.Len())
	}
}

func TestRegisterNilClearsEntry(t *testing.T) {
	name := provider.Name("test-register-nil")
	t.Cleanup(func() { Register(name, nil) })

	Register(name, stubDetector{marker: provider.AccountID("acct-a")})
	Register(name, nil)
	det := Lookup(name)
	if _, ok := det.(NoopDetector); !ok {
		t.Fatalf("after Register(nil) Lookup returned %T, want NoopDetector", det)
	}
}

func TestRegisterReplacesPriorDetector(t *testing.T) {
	name := provider.Name("test-register-replace")
	t.Cleanup(func() { Register(name, nil) })

	Register(name, stubDetector{marker: provider.AccountID("first")})
	Register(name, stubDetector{marker: provider.AccountID("second")})
	det := Lookup(name)
	set, err := det.Detect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if set.Contains("first") {
		t.Fatalf("expected first marker to be replaced, got it in the set")
	}
	if !set.Contains("second") {
		t.Fatalf("replacement marker missing, len=%d", set.Len())
	}
}

func TestConcurrentRegisterAndLookup(t *testing.T) {
	name := provider.Name("test-concurrent")
	t.Cleanup(func() { Register(name, nil) })

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			Register(name, stubDetector{marker: provider.AccountID("acct-x")})
		}()
		go func() {
			defer wg.Done()
			det := Lookup(name)
			_, _ = det.Detect(context.Background(), nil)
		}()
	}
	wg.Wait()
	// Final lookup must still resolve to a non-nil detector; either the
	// stub or the noop is acceptable depending on goroutine interleaving.
	if Lookup(name) == nil {
		t.Fatalf("Lookup returned nil after concurrent access")
	}
}

func TestSetZeroValueIsEmpty(t *testing.T) {
	var set Set
	if set.Len() != 0 {
		t.Fatalf("zero Set len = %d, want 0", set.Len())
	}
	if set.Contains("anything") {
		t.Fatalf("zero Set must not contain any account")
	}
}

func TestNewSetSeedsAccounts(t *testing.T) {
	set := NewSet(provider.AccountID("a"), provider.AccountID("b"))
	if set.Len() != 2 {
		t.Fatalf("len = %d, want 2", set.Len())
	}
	if !set.Contains("a") || !set.Contains("b") {
		t.Fatalf("expected a and b in set")
	}
	if set.Contains("c") {
		t.Fatalf("did not expect c in set")
	}
}
