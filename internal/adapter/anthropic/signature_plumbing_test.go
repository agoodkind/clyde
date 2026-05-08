package anthropic

import "testing"

// TestDispatchSSESignatureDeltaEmitsThinkingSignature asserts that a
// content_block_delta carrying a signature_delta payload is surfaced as
// a StreamEvent of kind "thinking_signature" with Text holding the
// opaque signature value and the originating block index preserved.
func TestDispatchSSESignatureDeltaEmitsThinkingSignature(t *testing.T) {
	t.Parallel()
	var got []StreamEvent
	sink := func(ev StreamEvent) error {
		got = append(got, ev)
		return nil
	}
	usage := &Usage{}
	stop := ""
	blockTypes := map[int]string{}
	const data = `{"index":7,"delta":{"type":"signature_delta","signature":"opaque-sig-bytes"}}`
	if err := dispatchSSE("content_block_delta", data, sink, usage, &stop, blockTypes); err != nil {
		t.Fatalf("dispatchSSE: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events=%d want 1: %#v", len(got), got)
	}
	want := StreamEvent{
		Kind:        "thinking_signature",
		Text:        "opaque-sig-bytes",
		BlockIndex:  7,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
	}
	if got[0] != want {
		t.Fatalf("event=%#v want %#v", got[0], want)
	}
}
