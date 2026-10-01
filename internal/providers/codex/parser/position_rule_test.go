package parser

import (
	"os"
	"reflect"
	"testing"
)

// codexAppendedUserLine is one user turn in the shape Codex appends to a live
// rollout.
const codexAppendedUserLine = `{"timestamp":"2026-06-08T22:00:08Z","type":"event_msg","payload":{"type":"user_message","message":"now count them"}}` + "\n"

// appendCodexRolloutLine appends one JSONL line to the rollout at path.
func appendCodexRolloutLine(t *testing.T, path string, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open rollout for append: %v", err)
	}
	if _, err := file.WriteString(line); err != nil {
		_ = file.Close()
		t.Fatalf("append rollout line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}
}

// TestCodexParserAppendKeepsEarlierPositions pins the position rule the search
// row key depends on. The test parses a rollout, appends one user turn, and
// parses it again. Every message from the first parse keeps its index and its
// exact text, and the second parse returns one more message. The test covers
// the streaming path and the buffered tool output path.
func TestCodexParserAppendKeepsEarlierPositions(t *testing.T) {
	t.Parallel()
	for _, includeToolOutputs := range []bool{false, true} {
		name := "streaming"
		if includeToolOutputs {
			name = "tool_outputs"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeCodexRollout(t)
			before := collectCodexMessages(t, path, includeToolOutputs)
			if len(before) < 2 {
				t.Fatalf("first parse messages = %d, want at least 2", len(before))
			}

			appendCodexRolloutLine(t, path, codexAppendedUserLine)
			after := collectCodexMessages(t, path, includeToolOutputs)

			if len(after) != len(before)+1 {
				t.Fatalf("second parse messages = %d, want %d", len(after), len(before)+1)
			}
			for i, earlier := range before {
				if after[i].Text != earlier.Text {
					t.Errorf("message %d text = %q after append, want %q", i, after[i].Text, earlier.Text)
					continue
				}
				// Several rollout messages carry only thinking or a tool call and
				// have empty text. Comparing the whole message detects a shift
				// among them.
				if !reflect.DeepEqual(after[i], earlier) {
					t.Errorf("message %d = %+v after append, want %+v", i, after[i], earlier)
				}
			}
			if got := after[len(before)].Text; got != "now count them" {
				t.Errorf("appended message text = %q, want %q", got, "now count them")
			}
		})
	}
}
