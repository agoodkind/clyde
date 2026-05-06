package codex

import (
	"encoding/json"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// envelopeWithRefAndEncrypted builds a synthetic-thinking envelope around
// body with the given data-ref attribute on the open marker AND the given
// data-encrypted attribute on the close marker. The shape matches what
// the renderer emits in production so [adapterrender.ExtractSyntheticParts]
// returns the same (Body, Ref, Encrypted) tuple the inbound mapper sees.
func envelopeWithRefAndEncrypted(ref, body, encrypted string) string {
	return adapterrender.FormatSyntheticContentDeltaWithRef(
		adapterrender.SyntheticReasoning, true, ref, body,
	) + adapterrender.SyntheticContentCloseWithEncrypted(adapterrender.SyntheticReasoning, encrypted)
}

// envelopeWithRef builds a synthetic-thinking envelope around body with
// the given data-ref attribute and no encrypted blob. Used by tests that
// model a marker carrying only the ref (no encrypted_content was on the
// upstream reasoning item).
func envelopeWithRef(ref, body string) string {
	return envelopeWithRefAndEncrypted(ref, body, "")
}

// envelopeNoRef builds a legacy attribute-less synthetic-thinking
// envelope around body for the legacy-empty-ref case.
func envelopeNoRef(body string) string {
	return adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, body)
}

// buildPhase6Request runs BuildRequestWithConfig with a single assistant
// message whose content contains envelopeText, returning the produced
// input slice. The request mirrors the Cursor BYOK round-trip path: a
// prior assistant turn (envelopeText) followed by a fresh user prompt.
func buildPhase6Request(
	t *testing.T,
	envelopeText string,
	cfg RequestBuilderConfig,
) []map[string]any {
	t.Helper()
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "assistant", Content: json.RawMessage(toJSONString(envelopeText))},
			{Role: "user", Content: json.RawMessage(`"next"`)},
		},
	}
	model := ResolvedModel{Alias: "gpt-5.4"}
	out := BuildRequestWithConfig(req, model, "", cfg)
	return out.Input
}

func toJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// findFirstReasoningItem returns the first item with type=="reasoning" in
// the input slice or nil when none is present.
func findFirstReasoningItem(items []map[string]any) map[string]any {
	for _, item := range items {
		t, _ := item["type"].(string)
		if t == "reasoning" {
			return item
		}
	}
	return nil
}

// findFirstAssistantMessage returns the first item with type=="message"
// and role=="assistant" or nil when none is present.
func findFirstAssistantMessage(items []map[string]any) (int, map[string]any) {
	for idx, item := range items {
		t, _ := item["type"].(string)
		role, _ := item["role"].(string)
		if t == "message" && role == "assistant" {
			return idx, item
		}
	}
	return -1, nil
}

func extractSummaryTexts(item map[string]any) []string {
	raw, ok := item["summary"]
	if !ok {
		return nil
	}
	entries, ok := raw.([]map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		text, _ := entry["text"].(string)
		out = append(out, text)
	}
	return out
}

// Case 1: native_summary_field + round_trip + marker carries encrypted.
func TestPhase6NativeSummaryRoundTripWithEncryptedEmitsBothFields(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_abc", "deep thinking", "ENC123"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item, got %#v", items)
	}
	if r["id"] != "rs_abc" {
		t.Fatalf("id = %v, want rs_abc", r["id"])
	}
	if r["encrypted_content"] != "ENC123" {
		t.Fatalf("encrypted_content = %v, want ENC123", r["encrypted_content"])
	}
	got := extractSummaryTexts(r)
	if len(got) != 1 || got[0] != "deep thinking" {
		t.Fatalf("summary = %v, want [deep thinking]", got)
	}
}

// Case 2: native_summary_field + round_trip + marker carries no encrypted.
// Equivalent to the pre-rewrite store miss.
func TestPhase6NativeSummaryRoundTripWithoutEncryptedOmitsField(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRef("rs_miss", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item")
	}
	if _, has := r["encrypted_content"]; has {
		t.Fatalf("encrypted_content should be absent when marker had none, got %#v", r)
	}
	if r["id"] != "rs_miss" {
		t.Fatalf("id = %v", r["id"])
	}
	if got := extractSummaryTexts(r); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("summary = %v", got)
	}
}

// Case 3: native_summary_field + drop. Even when the marker carries an
// encrypted blob, the drop lever forces the round-trip to omit it.
func TestPhase6NativeSummaryDropEncryptedOmitsField(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_abc", "thinking", "ENC"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedDrop,
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item")
	}
	if _, has := r["encrypted_content"]; has {
		t.Fatalf("encrypted_content present under drop mode")
	}
	if got := extractSummaryTexts(r); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("summary = %v", got)
	}
}

// Case 4: drop summary + round_trip + marker carries encrypted.
func TestPhase6DropSummaryRoundTripWithEncryptedOnlyEncrypted(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_abc", "thinking", "ENC"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item")
	}
	summary, has := r["summary"]
	if !has {
		t.Fatalf("summary field must be present (Codex Responses requires it; empty array allowed)")
	}
	if arr, ok := summary.([]map[string]any); !ok || len(arr) != 0 {
		t.Fatalf("summary must be an empty array, got %T %v", summary, summary)
	}
	if r["encrypted_content"] != "ENC" {
		t.Fatalf("encrypted_content = %v", r["encrypted_content"])
	}
}

// Case 5: drop summary + round_trip + marker carries no encrypted.
// The reasoning stub still emits because the ref is present.
func TestPhase6DropSummaryRoundTripWithoutEncryptedEmitsStubID(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRef("rs_only", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected stub reasoning item with just id")
	}
	if r["id"] != "rs_only" {
		t.Fatalf("id = %v", r["id"])
	}
	summary, has := r["summary"]
	if !has {
		t.Fatalf("summary field must be present (Codex Responses requires it; empty array allowed)")
	}
	if arr, ok := summary.([]map[string]any); !ok || len(arr) != 0 {
		t.Fatalf("summary must be an empty array, got %T %v", summary, summary)
	}
	if _, has := r["encrypted_content"]; has {
		t.Fatalf("encrypted_content must be absent on miss")
	}
}

// Case 6: drop summary + drop. Stub by id alone is still emitted.
func TestPhase6DropSummaryDropEncryptedEmitsStubIDWhenRefPresent(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_id", "thinking", "ENC_ignored"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedDrop,
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning stub")
	}
	if r["id"] != "rs_id" {
		t.Fatalf("id = %v", r["id"])
	}
	summary, has := r["summary"]
	if !has {
		t.Fatalf("summary field must be present (Codex Responses requires it; empty array allowed)")
	}
	if arr, ok := summary.([]map[string]any); !ok || len(arr) != 0 {
		t.Fatalf("summary must be an empty array, got %T %v", summary, summary)
	}
	if _, has := r["encrypted_content"]; has {
		t.Fatalf("encrypted_content must be absent under drop mode")
	}
}

// plain_text_concat + round_trip + marker carries encrypted. The body is
// folded into the assistant message; the encrypted_content rides in a
// separate Reasoning item that precedes the message.
func TestPhase6PlainTextConcatRoundTripWithEncryptedFoldsBodyAndEmitsEncrypted(t *testing.T) {
	t.Parallel()
	envelope := envelopeWithRefAndEncrypted("rs_abc", "deep thinking", "ENC")
	items := buildPhase6Request(t, envelope+"\nfinal answer", RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryPlainText,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		// Plain-text-concat must thread MaterializePlainTextConcat into
		// the message-text path so the body survives into the message
		// body. The provider wires this in production via
		// codexSummaryRenderStrategy.
		InboundThinkingMaterialization: adapterrender.MaterializePlainTextConcat,
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item carrying encrypted_content")
	}
	if r["encrypted_content"] != "ENC" {
		t.Fatalf("encrypted_content = %v", r["encrypted_content"])
	}
	summary, has := r["summary"]
	if !has {
		t.Fatalf("summary field must be present (Codex Responses requires it; empty array allowed)")
	}
	if arr, ok := summary.([]map[string]any); !ok || len(arr) != 0 {
		t.Fatalf("summary must be an empty array, got %T %v", summary, summary)
	}
	idx, msg := findFirstAssistantMessage(items)
	if msg == nil {
		t.Fatalf("expected assistant message")
	}
	// Reasoning must precede the assistant message (codex-rs ordering).
	if idx == 0 {
		t.Fatalf("assistant message must come AFTER reasoning item; got idx=0")
	}
}

// plain_text_concat + round_trip + marker carries no encrypted: no
// reasoning item emitted; body still folded into the message.
func TestPhase6PlainTextConcatRoundTripWithoutEncryptedEmitsNoReasoningItem(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRef("rs_miss", "thinking")+"\nanswer", RequestBuilderConfig{
		RoundTripSummary:               RoundTripSummaryPlainText,
		RoundTripEncrypted:             RoundTripEncryptedRoundTrip,
		InboundThinkingMaterialization: adapterrender.MaterializePlainTextConcat,
	})
	if r := findFirstReasoningItem(items); r != nil {
		t.Fatalf("expected no reasoning item, got %#v", r)
	}
}

// plain_text_concat + drop. No reasoning item even when the marker
// carried an encrypted blob.
func TestPhase6PlainTextConcatDropEncryptedEmitsNoReasoningItem(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_abc", "thinking", "ENC")+"\nanswer", RequestBuilderConfig{
		RoundTripSummary:               RoundTripSummaryPlainText,
		RoundTripEncrypted:             RoundTripEncryptedDrop,
		InboundThinkingMaterialization: adapterrender.MaterializePlainTextConcat,
	})
	if r := findFirstReasoningItem(items); r != nil {
		t.Fatalf("expected no reasoning item, got %#v", r)
	}
}

// Legacy attribute-less marker should be dropped silently when summary
// mode is drop AND encrypted is drop AND there is no ref (no payload to
// round-trip).
func TestPhase6LegacyNoRefDropsSilentlyUnderDropEncrypted(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeNoRef("legacy thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedDrop,
	})
	if r := findFirstReasoningItem(items); r != nil {
		t.Fatalf("expected NO reasoning item for legacy no-ref under drop+drop, got %#v", r)
	}
}

// Codex rejects Reasoning input items with an empty id (ApiIdParam,
// invalid_id). A marker with no data-ref must NOT produce a Reasoning
// item even when the body is non-empty under any summary mode, otherwise
// the upstream rejects the whole request.
func TestPhase6EmptyRefNeverEmitsReasoningItem(t *testing.T) {
	t.Parallel()
	for _, mode := range []RoundTripSummary{
		RoundTripSummaryNative,
		RoundTripSummaryDrop,
		RoundTripSummaryPlainText,
	} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			items := buildPhase6Request(t, envelopeNoRef("body without ref"), RequestBuilderConfig{
				RoundTripSummary:   mode,
				RoundTripEncrypted: RoundTripEncryptedRoundTrip,
			})
			if r := findFirstReasoningItem(items); r != nil {
				t.Fatalf("summary=%s: expected NO reasoning item when ref is empty, got %#v", mode, r)
			}
		})
	}
}

// Reasoning item must precede its assistant message in input order, even
// when the summary mode keeps the body in the message text.
func TestPhase6ReasoningPrecedesMessage(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_abc", "thinking", "ENC")+"\nfinal answer", RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	reasoningIdx := -1
	messageIdx := -1
	for idx, item := range items {
		t, _ := item["type"].(string)
		role, _ := item["role"].(string)
		switch {
		case t == "reasoning" && reasoningIdx < 0:
			reasoningIdx = idx
		case t == "message" && role == "assistant" && messageIdx < 0:
			messageIdx = idx
		}
	}
	if reasoningIdx < 0 || messageIdx < 0 {
		t.Fatalf("expected both reasoning and assistant message; got r=%d m=%d", reasoningIdx, messageIdx)
	}
	if reasoningIdx > messageIdx {
		t.Fatalf("reasoning item must come BEFORE assistant message (codex-rs ordering); r=%d m=%d", reasoningIdx, messageIdx)
	}
}

// adapteropenai compile-time guard: the Cursor-shape ChatRequest is the
// same one the package uses elsewhere; importing it here keeps the test
// file's intent visible.
var _ = adapteropenai.ChatRequest{}

// TestReasoningInputItemAlwaysEmitsSummaryField pins the contract that
// every emitted Reasoning input item carries a "summary" field, even when
// the field's value is an empty array. The upstream Codex Responses API
// rejects requests with [ObjectParam] [input[i].summary]
// [missing_required_parameter] when this field is absent, regardless of
// whether the round-trip strategy is drop, plain_text_concat, or
// native_summary_field. This test exists to ensure the asMap renderer
// never reverts to omitempty-style behavior on summary.
func TestReasoningInputItemAlwaysEmitsSummaryField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		item reasoningInputItem
	}{
		{
			name: "no summary, no encrypted",
			item: reasoningInputItem{ID: "rs_1", Summary: nil, EncryptedContent: ""},
		},
		{
			name: "no summary, with encrypted",
			item: reasoningInputItem{ID: "rs_2", Summary: nil, EncryptedContent: "ENC"},
		},
		{
			name: "with summary, no encrypted",
			item: reasoningInputItem{
				ID:               "rs_3",
				Summary:          []reasoningSummaryText{{Text: "thinking"}},
				EncryptedContent: "",
			},
		},
		{
			name: "with summary, with encrypted",
			item: reasoningInputItem{
				ID:               "rs_4",
				Summary:          []reasoningSummaryText{{Text: "thinking"}},
				EncryptedContent: "ENC",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := tc.item.asMap()
			summary, has := out["summary"]
			if !has {
				t.Fatalf("summary field must always be present; out=%v", out)
			}
			arr, ok := summary.([]map[string]any)
			if !ok {
				t.Fatalf("summary must be a []map[string]any; got %T", summary)
			}
			if len(tc.item.Summary) != len(arr) {
				t.Fatalf("summary length mismatch: in=%d out=%d", len(tc.item.Summary), len(arr))
			}
		})
	}
}
