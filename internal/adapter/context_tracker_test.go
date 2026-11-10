package adapter

import "testing"

func mustRaw(s string) []byte {
	return []byte(s)
}

func TestContextUsageTrackerAccumulatesPriorOutputIntoSurfacedPrompt(t *testing.T) {
	tracker := newContextUsageTracker()
	key := "conversation:test"

	first := tracker.Track(key, Usage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	})
	if first.usage.PromptTokens != 100 || first.usage.TotalTokens != 120 {
		t.Fatalf("first usage = %+v", first.usage)
	}

	second := tracker.Track(key, Usage{
		PromptTokens:     130,
		CompletionTokens: 15,
		TotalTokens:      145,
	})
	if second.rolledFrom != 20 {
		t.Fatalf("rolledFrom = %d want 20", second.rolledFrom)
	}
	if second.usage.PromptTokens != 150 {
		t.Fatalf("surfaced prompt = %d want 150", second.usage.PromptTokens)
	}
	if second.usage.CompletionTokens != 15 || second.usage.TotalTokens != 165 {
		t.Fatalf("second usage = %+v", second.usage)
	}
}

func TestContextUsageTrackerResetsAfterSharpPromptDrop(t *testing.T) {
	tracker := newContextUsageTracker()
	key := "conversation:test"

	_ = tracker.Track(key, Usage{
		PromptTokens:     400,
		CompletionTokens: 80,
		TotalTokens:      480,
	})

	next := tracker.Track(key, Usage{
		PromptTokens:     120,
		CompletionTokens: 10,
		TotalTokens:      130,
	})
	if next.rolledFrom != 0 {
		t.Fatalf("rolledFrom = %d want 0 after reset", next.rolledFrom)
	}
	if next.usage.PromptTokens != 120 || next.usage.TotalTokens != 130 {
		t.Fatalf("usage after reset = %+v", next.usage)
	}
}

func TestRequestContextTrackerKeyPrefersUserAndMetadata(t *testing.T) {
	req := ChatRequest{
		User: "composer-123",
		Metadata: mustRaw(`{
			"conversation_id": "conv-1",
			"composerId": "cmp-1"
		}`),
		Messages: []ChatMessage{
			{Role: "user", Content: mustRaw(`"hello"`)},
		},
	}
	if got := requestContextTrackerKey(req, "clyde-opus-4-7"); got != "user:composer-123" {
		t.Fatalf("key = %q", got)
	}

	req.User = ""
	if got := requestContextTrackerKey(req, "clyde-opus-4-7"); got != "meta:conv-1" {
		t.Fatalf("metadata key = %q", got)
	}
}

func TestRequestContextTrackerKeyFallsBackToFirstUserFingerprint(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: mustRaw(`"sys"`)},
			{Role: "user", Content: mustRaw(`"hello"`)},
		},
	}
	got1 := requestContextTrackerKey(req, "clyde-opus-4-7")
	got2 := requestContextTrackerKey(req, "clyde-opus-4-7")
	if got1 == "" || got1 != got2 {
		t.Fatalf("unstable fallback key: %q vs %q", got1, got2)
	}
}
