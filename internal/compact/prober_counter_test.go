package compact

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/contextusage"
)

// fakeCandidateProber records every CountCandidate call and returns a
// caller-supplied token total so planner tests can drive the iteration
// loop deterministically.
type fakeCandidateProber struct {
	calls       atomic.Int32
	lastRequest atomic.Pointer[contextusage.CandidateRequest]
	tokens      func(call int32) int
}

func (f *fakeCandidateProber) CountCandidate(_ context.Context, req contextusage.CandidateRequest) (int, error) {
	n := f.calls.Add(1)
	dup := req
	f.lastRequest.Store(&dup)
	if f.tokens == nil {
		return 100, nil
	}
	return f.tokens(n), nil
}

// TestProberCounterRoutesThroughCandidateProber asserts the planner's
// authoritative path runs against the registered CandidateProber and
// that the serialized candidate payload is well-formed JSONL.
func TestProberCounterRoutesThroughCandidateProber(t *testing.T) {
	t.Parallel()
	fake := &fakeCandidateProber{tokens: func(int32) int { return 12_345 }}
	counter := newProberCounter(proberCounterConfig{
		Prober:         fake,
		SessionID:      "session-abc",
		TranscriptPath: "/tmp/projects/encoded/session-abc.jsonl",
		WorkDir:        "/tmp/workspace",
		Cwd:            "/tmp/workspace",
		Version:        "clyde-test",
	})
	got, err := counter.CountSyntheticUser(context.Background(), []OutputBlock{
		{Text: "hello from candidate"},
	})
	if err != nil {
		t.Fatalf("CountSyntheticUser: %v", err)
	}
	if got != 12_345 {
		t.Fatalf("CountSyntheticUser tokens = %d, want 12345", got)
	}
	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("CountCandidate calls = %d, want 1", calls)
	}
	req := fake.lastRequest.Load()
	if req == nil {
		t.Fatal("CountCandidate request not captured")
	}
	if req.LiveSessionID != "session-abc" {
		t.Errorf("LiveSessionID = %q, want session-abc", req.LiveSessionID)
	}
	if req.TranscriptPath != "/tmp/projects/encoded/session-abc.jsonl" {
		t.Errorf("TranscriptPath = %q", req.TranscriptPath)
	}
	if req.WorkDir != "/tmp/workspace" {
		t.Errorf("WorkDir = %q", req.WorkDir)
	}
	body := string(req.CandidateJSONL)
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("candidate jsonl missing trailing newline: %q", body)
	}
	var entry map[string]json.RawMessage
	line := strings.TrimRight(body, "\n")
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("candidate jsonl not valid json: %v", err)
	}
	for _, want := range []string{"type", "isCompactSummary", "uuid", "message", "sessionId"} {
		if _, ok := entry[want]; !ok {
			t.Errorf("candidate jsonl missing %q field", want)
		}
	}
}

// TestRunPlanUsesProberAndDoesNotCallCountTokensAPI runs the planner
// against a fake CandidateProber and an httptest.Server that fails the
// test if it ever receives a request. The planner must finish without
// touching the Anthropic count_tokens endpoint when the proberCounter
// has no debug counter wired in.
func TestRunPlanUsesProberAndDoesNotCallCountTokensAPI(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		userText("u1", "b1", "user message", t0.Add(time.Second)),
		assistantBlocks("a1", "u1", t0.Add(2*time.Second), []map[string]any{
			{"type": "text", "text": "reply"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	// Forbidden HTTP server: any inbound call fails the test.
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("planner unexpectedly hit count_tokens endpoint: %s %s", r.Method, r.URL.Path)
		http.Error(w, "forbidden", http.StatusInternalServerError)
	}))
	defer forbidden.Close()

	fake := &fakeCandidateProber{tokens: func(call int32) int {
		// Baseline high, then drop below target on the second call.
		if call == 1 {
			return 1_000
		}
		return 200
	}}
	counter := newProberCounter(proberCounterConfig{
		Prober:         fake,
		SessionID:      "session-xyz",
		TranscriptPath: "/tmp/projects/encoded/session-xyz.jsonl",
		WorkDir:        "/tmp/workspace",
		Cwd:            "/tmp/workspace",
		Version:        "clyde-test",
	})
	res, err := RunPlan(context.Background(), PlanInput{
		Slice:     slice,
		Strippers: Strippers{Thinking: true, Images: true, Tools: true, Chat: true},
		Target:    500,
		Counter:   counter,
		BatchSize: 1,
	})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if res.BaselineTail != 1_000 {
		t.Errorf("BaselineTail = %d, want 1000", res.BaselineTail)
	}
	if fake.calls.Load() < 1 {
		t.Errorf("CandidateProber not called")
	}
}
