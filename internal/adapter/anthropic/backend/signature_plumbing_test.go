package anthropicbackend

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// TestStreamTranslatorSignatureDeltaProducesReasoningEventWithSignature
// asserts that a signature_delta SSE payload is translated into a single
// EventReasoningDelta whose Signature carries the opaque signature value
// and whose block-index correlation flows through the underlying
// content_block_delta envelope.
func TestStreamTranslatorSignatureDeltaProducesReasoningEventWithSignature(t *testing.T) {
	t.Parallel()
	tr := NewStreamTranslator("req-sig", "alias-sig")
	const payload = `{"index":3,"delta":{"type":"signature_delta","signature":"sig-bytes-xyz"}}`
	events, finished, reason, usage, err := tr.HandleEventEvents("content_block_delta", []byte(payload))
	if err != nil {
		t.Fatalf("HandleEventEvents: %v", err)
	}
	if finished || reason != "" || usage != nil {
		t.Fatalf("finished=%v reason=%q usage=%+v want zero-value terminal state", finished, reason, usage)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d want 1: %#v", len(events), events)
	}
	got := events[0]
	if got.Kind != adapterrender.EventReasoningDelta {
		t.Fatalf("kind=%q want %q", got.Kind, adapterrender.EventReasoningDelta)
	}
	if got.Signature != "sig-bytes-xyz" {
		t.Fatalf("signature=%q want sig-bytes-xyz", got.Signature)
	}
	if strings.TrimSpace(got.Text) != "" {
		t.Fatalf("text=%q want empty for signature_delta", got.Text)
	}
	if got.EncryptedContent != "" {
		t.Fatalf("encrypted_content=%q want empty for signature_delta", got.EncryptedContent)
	}
}

// TestStreamEventToTranslatorSSEReencodesThinkingSignature asserts that
// re-emitting a thinking_signature StreamEvent through the translator
// boundary yields the upstream-shaped content_block_delta SSE payload
// with `"type":"signature_delta"` and the original signature value.
func TestStreamEventToTranslatorSSEReencodesThinkingSignature(t *testing.T) {
	t.Parallel()
	name, payload, ok := StreamEventToTranslatorSSE(anthropic.StreamEvent{
		Kind:        "thinking_signature",
		Text:        "opaque-sig",
		BlockIndex:  9,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
	})
	if !ok {
		t.Fatalf("ok=false; thinking_signature must round-trip")
	}
	if name != "content_block_delta" {
		t.Fatalf("event=%q want content_block_delta", name)
	}
	body := string(payload)
	if !strings.Contains(body, `"type":"signature_delta"`) {
		t.Fatalf("payload missing signature_delta type: %s", body)
	}
	if !strings.Contains(body, `"signature":"opaque-sig"`) {
		t.Fatalf("payload missing signature value: %s", body)
	}
	if !strings.Contains(body, `"index":9`) {
		t.Fatalf("payload missing block index: %s", body)
	}
}
