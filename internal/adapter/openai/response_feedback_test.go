package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestResponseTextUsesOutputTextDeltas(t *testing.T) {
	body := []byte(
		"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"first "}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"second"}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"output":[]}}` + "\n\n",
	)
	if got := ResponseText(body); got != "first second" {
		t.Fatalf("ResponseText() = %q", got)
	}
}

func TestResponseTextUsesCompletedOutputWithoutDeltas(t *testing.T) {
	body := []byte(
		"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}` + "\n\n",
	)
	if got := ResponseText(body); got != "answer" {
		t.Fatalf("ResponseText() = %q", got)
	}
}

func TestAppendTextItemAddsCompleteMessageBeforeTerminalEvent(t *testing.T) {
	body := []byte(
		": keep this comment\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"answer","sequence_number":7}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","sequence_number":8,"response":{"id":"resp_1","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}]},"future":"preserved"}` + "\n\n",
	)
	augmented, err := AppendTextItem(body, "retry")
	if err != nil {
		t.Fatalf("AppendTextItem: %v", err)
	}
	if !bytes.Contains(augmented, []byte(": keep this comment")) || !bytes.Contains(augmented, []byte(`"future":"preserved"`)) {
		t.Fatalf("unknown data was removed: %s", augmented)
	}
	if strings.Index(string(augmented), "retry") > strings.Index(string(augmented), "event: response.completed") {
		t.Fatalf("feedback appears after response.completed: %s", augmented)
	}
	events := parseResponseSSE(augmented)
	wantSequence := 8
	for _, event := range events {
		var identity responseEventIdentity
		if json.Unmarshal(event.data, &identity) != nil || identity.SequenceNumber == nil {
			continue
		}
		if *identity.SequenceNumber < wantSequence {
			continue
		}
		if *identity.SequenceNumber != wantSequence {
			t.Fatalf("sequence = %d, want %d", *identity.SequenceNumber, wantSequence)
		}
		wantSequence++
	}
	if wantSequence != 15 {
		t.Fatalf("next sequence = %d, want 15", wantSequence)
	}
	if got := ResponseText(augmented); got != "answerretry" {
		t.Fatalf("ResponseText(augmented) = %q", got)
	}
}
