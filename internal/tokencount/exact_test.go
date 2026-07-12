package tokencount

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scriptedExact returns a scripted sequence of counts, optionally erroring at a
// given call index, and records how many times it was called.
type scriptedExact struct {
	counts []int
	errAt  int
	calls  int
}

func (s *scriptedExact) Count(context.Context, string, string) (int, error) {
	i := s.calls
	s.calls++
	if s.errAt == i {
		return 0, errors.New("count failed")
	}
	if i < len(s.counts) {
		return s.counts[i], nil
	}
	return s.counts[len(s.counts)-1], nil
}

func lineBody(lines int) string {
	var b strings.Builder
	for range lines {
		b.WriteString("xx\n") // 3 bytes -> 3 tokens at cpt=1
	}
	return b.String()
}

func TestCapToLastTokensExactNilExactUsesLocal(t *testing.T) {
	c := heuristicCounter{charsPerToken: 1}
	body := lineBody(10)
	got, _ := CapToLastTokensExact(context.Background(), body, 6, c, nil, "m")
	want, _, _ := CapToLastTokens(body, 6, c)
	if got != want {
		t.Fatalf("nil exact: got %q, want local %q", got, want)
	}
}

func TestCapToLastTokensExactWithinBudgetKeepsCandidate(t *testing.T) {
	c := heuristicCounter{charsPerToken: 1}
	body := lineBody(10)
	exact := &scriptedExact{counts: []int{5}, errAt: -1}
	got, _ := CapToLastTokensExact(context.Background(), body, 6, c, exact, "m")
	local, _, _ := CapToLastTokens(body, 6, c)
	if got != local {
		t.Fatalf("within budget: got %q, want candidate %q", got, local)
	}
	if exact.calls != 1 {
		t.Fatalf("within budget: exact called %d times, want 1", exact.calls)
	}
}

func TestCapToLastTokensExactErrorFallsBackToLocal(t *testing.T) {
	c := heuristicCounter{charsPerToken: 1}
	body := lineBody(10)
	exact := &scriptedExact{counts: []int{0}, errAt: 0}
	got, _ := CapToLastTokensExact(context.Background(), body, 6, c, exact, "m")
	local, _, _ := CapToLastTokens(body, 6, c)
	if got != local {
		t.Fatalf("error fallback: got %q, want candidate %q", got, local)
	}
}

func TestCapToLastTokensExactShrinksOnOvershoot(t *testing.T) {
	c := heuristicCounter{charsPerToken: 1}
	body := lineBody(10)
	// First authoritative count overshoots, second is within budget.
	exact := &scriptedExact{counts: []int{12, 2}, errAt: -1}
	got, _ := CapToLastTokensExact(context.Background(), body, 6, c, exact, "m")
	local, _, _ := CapToLastTokens(body, 6, c)
	if len(got) >= len(local) {
		t.Fatalf("overshoot should shrink: got %q (len %d), local %q (len %d)", got, len(got), local, len(local))
	}
	if exact.calls != 2 {
		t.Fatalf("overshoot: exact called %d times, want 2", exact.calls)
	}
}

func TestCapToLastTokensExactBoundedCalls(t *testing.T) {
	c := heuristicCounter{charsPerToken: 1}
	body := lineBody(40)
	// Never within budget; the refine loop must stop after maxRefineCalls.
	exact := &scriptedExact{counts: []int{9999}, errAt: -1}
	CapToLastTokensExact(context.Background(), body, 30, c, exact, "m")
	if exact.calls > maxRefineCalls {
		t.Fatalf("refine made %d calls, want at most %d", exact.calls, maxRefineCalls)
	}
}

func TestAnthropicExactCounterUsesAPIKeyNotBearer(t *testing.T) {
	var gotKey, gotAuth, gotVersion, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens": 42}`))
	}))
	defer srv.Close()

	counter := NewAnthropicExactCounter(srv.Client(), "secret-key", srv.URL, "2023-06-01")
	got, err := counter.Count(context.Background(), "hello world", "claude-opus-4-8")
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if got != 42 {
		t.Errorf("input_tokens = %d, want 42", got)
	}
	if gotKey != "secret-key" {
		t.Errorf("x-api-key = %q, want %q", gotKey, "secret-key")
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (OAuth must not be used)", gotAuth)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, "2023-06-01")
	}
	if !strings.Contains(gotBody, "claude-opus-4-8") || !strings.Contains(gotBody, "hello world") {
		t.Errorf("request body missing model or content: %q", gotBody)
	}
}

func TestOpenAIExactCounterUsesBearer(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens": 7}`))
	}))
	defer srv.Close()

	counter := NewOpenAIExactCounter(srv.Client(), "sk-test", srv.URL)
	got, err := counter.Count(context.Background(), "count me", "gpt-4o")
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if got != 7 {
		t.Errorf("input_tokens = %d, want 7", got)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}
	if !strings.Contains(gotBody, "gpt-4o") || !strings.Contains(gotBody, "count me") {
		t.Errorf("request body missing model or input: %q", gotBody)
	}
}
