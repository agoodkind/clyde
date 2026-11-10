package anthropic

import (
	"bytes"
	"strings"
	"testing"
)

const responseTextTestSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","content":[]}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestResponseTextJoinsTextDeltas(t *testing.T) {
	t.Parallel()
	if got := ResponseText([]byte(responseTextTestSSE)); got != "hello" {
		t.Fatalf("ResponseText() = %q, want hello", got)
	}
}

func TestAppendTextBlockAddsDiagnosticBeforeMessageDelta(t *testing.T) {
	t.Parallel()
	output, err := AppendTextBlock([]byte(responseTextTestSSE), "diagnostic")
	if err != nil {
		t.Fatalf("AppendTextBlock: %v", err)
	}
	if !bytes.Contains(output, []byte(`"index":1`)) {
		t.Fatalf("appended block index missing: %s", output)
	}
	if !bytes.Contains(output, []byte(`"text":"diagnostic"`)) {
		t.Fatalf("diagnostic missing: %s", output)
	}
	diagnosticIndex := strings.Index(string(output), `"text":"diagnostic"`)
	messageDeltaIndex := strings.Index(string(output), "event: message_delta")
	if diagnosticIndex < 0 || messageDeltaIndex < 0 || diagnosticIndex >= messageDeltaIndex {
		t.Fatalf("diagnostic index = %d, message delta index = %d", diagnosticIndex, messageDeltaIndex)
	}
	if got := ResponseText(output); got != "hello\ndiagnostic" {
		t.Fatalf("ResponseText(output) = %q", got)
	}
}
