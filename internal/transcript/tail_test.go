package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// drainTail collects everything emitted on the channel within
// timeout. The Tailer never closes the channel during a successful
// run so the timeout is the natural exit.
func drainTail(ch <-chan TailLine, timeout time.Duration) []TailLine {
	var out []TailLine
	deadline := time.After(timeout)
	for {
		select {
		case ln, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ln)
		case <-deadline:
			return out
		}
	}
}

func TestTailerEmitsLinesAppendedAfterStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	// Pre-write one line so the tailer has a real file to seek on.
	if err := os.WriteFile(path, []byte("{\"type\":\"system\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tailer, err := OpenTailer(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer tailer.Close()

	// Append two new lines.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	lines := drainTail(tailer.Lines(), 500*time.Millisecond)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d (%v)", len(lines), lines)
	}
	if lines[0].Role != "user" || lines[0].Text != "hello" {
		t.Fatalf("user line wrong: %+v", lines[0])
	}
	if lines[1].Role != "assistant" || lines[1].Text != "hi" {
		t.Fatalf("assistant line wrong: %+v", lines[1])
	}
}

func TestTailerStreamsFromOffsetZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"a\"}}\n" +
		"{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"b\"}}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	tailer, err := OpenTailer(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tailer.Close()

	lines := drainTail(tailer.Lines(), 200*time.Millisecond)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines from start, got %d", len(lines))
	}
}

func TestExtractTextHandlesPlainStringAndBlocks(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"plain"`, "plain"},
		{`[{"type":"text","text":"hello"}]`, "hello"},
		{`[{"type":"image"}]`, "[image]"},
		{`[{"type":"tool_use","name":"Bash"}]`, "[tool: Bash]"},
		{`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
	}
	for _, tc := range cases {
		got := extractText([]byte(tc.in))
		if got != tc.want {
			t.Errorf("input %q: want %q, got %q", tc.in, tc.want, got)
		}
	}
}
