package parser

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

// thinkingOnlyTranscript has a thinking-only assistant entry (no text, no
// tool_use), the shape Claude writes when it streams a thinking block as its
// own record, followed by a normal text reply.
const thinkingOnlyTranscript = `{"type":"assistant","uuid":"u1","timestamp":"2026-06-08T22:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"weighing the options","signature":"sig"}]}}
{"type":"assistant","uuid":"u2","timestamp":"2026-06-08T22:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"here is the answer"}]}}
`

// TestThinkingOnlyAssistantEntryIsKept asserts a thinking-only assistant entry
// survives parsing and carries its thinking text, so export can render and gate
// it. Before the fix these entries were dropped because they had no text and no
// tool calls.
func TestThinkingOnlyAssistantEntryIsKept(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(thinkingOnlyTranscript), 0o600); err != nil {
		t.Fatalf("write transcript fixture: %v", err)
	}

	opts := conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}
	var thinking, text int
	p := New()
	for message, err := range p.Stream(path, opts) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if message.Thinking != "" {
			thinking++
			if message.Thinking != "weighing the options" {
				t.Errorf("thinking = %q", message.Thinking)
			}
		}
		if message.Text != "" {
			text++
		}
	}
	if thinking != 1 {
		t.Errorf("thinking-bearing messages = %d, want 1", thinking)
	}
	if text != 1 {
		t.Errorf("text messages = %d, want 1", text)
	}
}
