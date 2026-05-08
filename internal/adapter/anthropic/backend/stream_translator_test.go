package anthropicbackend

import (
	"strings"
	"testing"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// TestStreamTranslatorRedactedThinkingFlowsThroughHandleEvents asserts that
// a captured Anthropic SSE sequence containing surrounding text_delta
// events plus a redacted_thinking content_block_start (with the opaque
// data blob inline) followed by content_block_stop flows through
// HandleEventEvents without error and emits the expected reasoning
// triplet: EventReasoningSignaled with ReasoningKind="redacted",
// EventReasoningDelta with ReasoningKind="redacted" carrying the data
// blob on RedactedData, and EventReasoningFinished. The text_delta
// events bracketing the redacted span are emitted as
// EventAssistantTextDelta so the redacted span does not interfere with
// the surrounding visible-text flow.
func TestStreamTranslatorRedactedThinkingFlowsThroughHandleEvents(t *testing.T) {
	t.Parallel()
	tr := NewStreamTranslator("req-redacted", "alias-redacted")

	type step struct {
		name    string
		payload string
	}
	sequence := []step{
		{name: "message_start", payload: `{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`},
		{name: "content_block_start", payload: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{name: "content_block_delta", payload: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"prefix "}}`},
		{name: "content_block_stop", payload: `{"type":"content_block_stop","index":0}`},
		{name: "content_block_start", payload: `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"REDACTED_BLOB_BASE64=="}}`},
		{name: "content_block_stop", payload: `{"type":"content_block_stop","index":1}`},
		{name: "content_block_start", payload: `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`},
		{name: "content_block_delta", payload: `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"suffix"}}`},
		{name: "content_block_stop", payload: `{"type":"content_block_stop","index":2}`},
	}

	var emitted []adapterrender.Event
	for _, s := range sequence {
		events, _, _, _, err := tr.HandleEventEvents(s.name, []byte(s.payload))
		if err != nil {
			t.Fatalf("HandleEventEvents(%s): %v", s.name, err)
		}
		emitted = append(emitted, events...)
	}

	var (
		sawSignaled bool
		sawDelta    bool
		sawFinished bool
		sawPrefix   bool
		sawSuffix   bool
		dataBlob    string
	)
	for _, ev := range emitted {
		switch ev.Kind {
		case adapterrender.EventReasoningSignaled:
			if strings.TrimSpace(ev.ReasoningKind) == "redacted" {
				sawSignaled = true
			}
		case adapterrender.EventReasoningDelta:
			if strings.TrimSpace(ev.ReasoningKind) == "redacted" {
				sawDelta = true
				dataBlob = ev.RedactedData
				if strings.TrimSpace(ev.Text) != "[redacted]" {
					t.Fatalf("redacted delta text=%q want [redacted]", ev.Text)
				}
			}
		case adapterrender.EventReasoningFinished:
			if strings.TrimSpace(ev.ReasoningKind) == "redacted" {
				sawFinished = true
			}
		case adapterrender.EventAssistantTextDelta:
			if ev.Text == "prefix " {
				sawPrefix = true
			}
			if ev.Text == "suffix" {
				sawSuffix = true
			}
		}
	}

	if !sawSignaled {
		t.Fatalf("missing EventReasoningSignaled with ReasoningKind=redacted in %+v", emitted)
	}
	if !sawDelta {
		t.Fatalf("missing EventReasoningDelta with ReasoningKind=redacted in %+v", emitted)
	}
	if !sawFinished {
		t.Fatalf("missing EventReasoningFinished with ReasoningKind=redacted in %+v", emitted)
	}
	if dataBlob != "REDACTED_BLOB_BASE64==" {
		t.Fatalf("RedactedData=%q want REDACTED_BLOB_BASE64==", dataBlob)
	}
	if !sawPrefix || !sawSuffix {
		t.Fatalf("expected surrounding text deltas to flow through; got %+v", emitted)
	}
}
