package parser

import (
	"os"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

// TestStreamSelectedKeepsEarlierMessagePositionsAfterAppend writes the Copilot
// schema fixture and streams the root chat. The test appends one user.message event
// line to the same events.jsonl file, then streams the root chat again. Every
// message from the first stream must keep its index and exact Text in the second
// stream, and the second stream must return one more message.
func TestStreamSelectedKeepsEarlierMessagePositionsAfterAppend(t *testing.T) {
	path, _ := writeSchemaFixture(t, false)
	parser := New()
	options := conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
		IncludeInjected:       false,
		HarnessTally:          nil,
	}

	first, err := conversation.CollectMessages(parser.StreamSelected(path, "", options))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 2 {
		t.Fatalf("first stream len = %d, want at least 2: %q", len(first), messageTexts(first))
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	appended := `{"id":"13","parentId":"12","timestamp":"2026-08-07T10:00:12Z","type":"user.message","data":{"content":"appended request"}}` + "\n"
	if _, err := file.WriteString(appended); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := conversation.CollectMessages(parser.StreamSelected(path, "", options))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first)+1 {
		t.Fatalf("second stream len = %d, want %d: %q", len(second), len(first)+1, messageTexts(second))
	}
	for i, message := range first {
		if second[i].Text != message.Text {
			t.Fatalf("second stream message %d Text = %q, want %q from the first stream", i, second[i].Text, message.Text)
		}
	}
	if second[len(first)].Text != "appended request" {
		t.Fatalf("appended message Text = %q, want %q", second[len(first)].Text, "appended request")
	}
}
