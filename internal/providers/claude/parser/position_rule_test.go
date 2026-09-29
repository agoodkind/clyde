package parser

import (
	"os"
	"testing"
)

// claudeAssistantReplyLine is the assistant reply that follows the first user
// turn in the fixture.
const claudeAssistantReplyLine = `{"uuid":"2","parentUuid":"1","type":"assistant","timestamp":"2026-08-02T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"The linker cannot find the vendored module."}]}}` + "\n"

// claudeAppendedUserLine is one user turn in the shape Claude Code appends to a
// live session transcript.
const claudeAppendedUserLine = `{"uuid":"3","parentUuid":"2","type":"user","timestamp":"2026-08-02T10:01:00Z","message":{"role":"user","content":"vendor it again"}}` + "\n"

// appendTranscriptLine appends one JSONL line to the transcript at path.
func appendTranscriptLine(t *testing.T, path string, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	if _, err := file.WriteString(line); err != nil {
		_ = file.Close()
		t.Fatalf("append transcript line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close transcript: %v", err)
	}
}

// TestStreamAppendKeepsEarlierPositions pins the position rule the search row
// key depends on. The test parses a two message transcript, appends one user
// turn, and parses it again. Every message from the first parse keeps its index
// and its exact text, and the second parse returns one more message.
func TestStreamAppendKeepsEarlierPositions(t *testing.T) {
	t.Parallel()
	path := writeInjectedFixture(t, `"why is the build failing?"`)
	appendTranscriptLine(t, path, claudeAssistantReplyLine)

	before := streamInjectedFixture(t, path, false)
	if len(before) != 2 {
		t.Fatalf("first parse messages = %d, want 2", len(before))
	}

	appendTranscriptLine(t, path, claudeAppendedUserLine)
	after := streamInjectedFixture(t, path, false)

	if len(after) != len(before)+1 {
		t.Fatalf("second parse messages = %d, want %d", len(after), len(before)+1)
	}
	for i, earlier := range before {
		if after[i] != earlier {
			t.Errorf("message %d text = %q after append, want %q", i, after[i], earlier)
		}
	}
	if got := after[len(before)]; got != "vendor it again" {
		t.Errorf("appended message text = %q, want %q", got, "vendor it again")
	}
}
