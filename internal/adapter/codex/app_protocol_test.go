package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRPCThreadTokenUsageUpdatedNotificationMatchesAppServerSchema(t *testing.T) {
	window := 272000
	notification := RPCThreadTokenUsageUpdatedNotification{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		TokenUsage: RPCThreadTokenUsage{
			Total: RPCTokenUsage{
				TotalTokens:           120,
				InputTokens:           100,
				CachedInputTokens:     64,
				OutputTokens:          20,
				ReasoningOutputTokens: 8,
			},
			Last: RPCTokenUsage{
				TotalTokens:           30,
				InputTokens:           20,
				CachedInputTokens:     0,
				OutputTokens:          10,
				ReasoningOutputTokens: 3,
			},
			ModelContextWindow: &window,
		},
	}

	raw, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("marshal token usage notification: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		`"threadId":"thread-1"`,
		`"turnId":"turn-1"`,
		`"total":`,
		`"last":`,
		`"cachedInputTokens":64`,
		`"modelContextWindow":272000`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("encoded token usage missing %q in %s", want, out)
		}
	}

	var decoded RPCThreadTokenUsageUpdatedNotification
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal token usage notification: %v", err)
	}
	if decoded.TokenUsage.Total.CachedInputTokens != 64 {
		t.Fatalf("total.cachedInputTokens=%d want 64", decoded.TokenUsage.Total.CachedInputTokens)
	}
	if decoded.TokenUsage.Last.CachedInputTokens != 0 {
		t.Fatalf("last.cachedInputTokens=%d want 0", decoded.TokenUsage.Last.CachedInputTokens)
	}
	if decoded.TokenUsage.ModelContextWindow == nil || *decoded.TokenUsage.ModelContextWindow != window {
		t.Fatalf("modelContextWindow=%v want %d", decoded.TokenUsage.ModelContextWindow, window)
	}
}
