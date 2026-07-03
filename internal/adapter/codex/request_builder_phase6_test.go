package codex

import (
	"encoding/json"
	"testing"

	"goodkind.io/clyde/codexwire"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// envelopeWithRefAndEncrypted builds a synthetic-thinking envelope around
// body with the given data-ref attribute on the open marker AND the given
// data-encrypted attribute on the close marker. The open marker is tagged
// data-origin="codex" so the Codex backend's cross-provider rule treats
// it as a native Codex-origin envelope (the same shape the renderer emits
// for Codex streams). The shape matches what the renderer emits in
// production so [adapterrender.ExtractSyntheticParts] returns the same
// (Body, Ref, Encrypted, Origin) tuple the inbound mapper sees.
func envelopeWithRefAndEncrypted(ref, body, encrypted string) string {
	return adapterrender.FormatSyntheticContentDeltaWithRef(
		adapterrender.SyntheticReasoning, true, true, ref, adapterrender.OriginCodex, body,
	) + adapterrender.SyntheticContentCloseWithAttrs(adapterrender.SyntheticReasoning, encrypted, "")
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
) []codexwire.InputItem {
	t.Helper()
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "assistant", Content: json.RawMessage(toJSONString(envelopeText))},
			{Role: "user", Content: json.RawMessage(`"next"`)},
		},
	}
	resolved := codexResolvedForTest(ResolvedAlias{Alias: "gpt-5.4"})
	out := BuildRequestWithConfig(req, resolved, "", cfg)
	return out.Input
}

func toJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// findFirstReasoningItem returns the first item with type=="reasoning"
// in the input slice or zero+false when none is present.
func findFirstReasoningItem(items []codexwire.InputItem) (codexwire.InputItem, bool) {
	for _, item := range items {
		if item.Type == codexwire.ItemTypeReasoning {
			return item, true
		}
	}
	return codexwire.InputItem{}, false
}

// findFirstAssistantMessage returns the first item with type=="message"
// and role=="assistant" or (-1, zero, false) when none is present.
func findFirstAssistantMessage(items []codexwire.InputItem) (int, codexwire.InputItem, bool) {
	for idx, item := range items {
		if item.Type == codexwire.ItemTypeMessage && item.Role == "assistant" {
			return idx, item, true
		}
	}
	return -1, codexwire.InputItem{}, false
}

func assertAssistantMessageContainsText(t *testing.T, items []codexwire.InputItem, want string) {
	t.Helper()
	_, item, found := findFirstAssistantMessage(items)
	if !found {
		t.Fatalf("expected assistant message, got %#v", items)
	}
	for _, content := range item.Content {
		if content.Text == want {
			return
		}
	}
	t.Fatalf("assistant message content = %#v, want text %q", item.Content, want)
}

func extractSummaryTexts(item codexwire.InputItem) []string {
	out := make([]string, 0, len(item.Summary))
	for _, entry := range item.Summary {
		out = append(out, entry.Text)
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
	r, ok := findFirstReasoningItem(items)
	if !ok {
		t.Fatalf("expected reasoning item, got %#v", items)
	}
	if r.ID != "rs_abc" {
		t.Fatalf("id = %q, want rs_abc", r.ID)
	}
	if r.EncryptedContent != "ENC123" {
		t.Fatalf("encrypted_content = %q, want ENC123", r.EncryptedContent)
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
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected no reasoning item")
	}
	assertAssistantMessageContainsText(t, items, "thinking")
}

// Case 3: native_summary_field + drop. Even when the marker carries an
// encrypted blob, the drop lever forces the round-trip to omit it.
func TestPhase6NativeSummaryDropEncryptedOmitsField(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_abc", "thinking", "ENC"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedDrop,
	})
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected no reasoning item")
	}
	assertAssistantMessageContainsText(t, items, "thinking")
}

// Case 4: drop summary + round_trip + marker carries encrypted.
func TestPhase6DropSummaryRoundTripWithEncryptedOnlyEncrypted(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_abc", "thinking", "ENC"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	r, ok := findFirstReasoningItem(items)
	if !ok {
		t.Fatalf("expected reasoning item")
	}
	if r.Summary == nil {
		t.Fatalf("summary slice must be present (Codex Responses requires it; empty array allowed)")
	}
	if len(r.Summary) != 0 {
		t.Fatalf("summary must be an empty slice, got %v", r.Summary)
	}
	if r.EncryptedContent != "ENC" {
		t.Fatalf("encrypted_content = %q", r.EncryptedContent)
	}
}

// Case 5: drop summary + round_trip + marker carries no encrypted. No
// reasoning item is emitted; the body folds into the assistant message.
func TestPhase6DropSummaryRoundTripWithoutEncryptedEmitsNoReasoningItem(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRef("rs_only", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected no reasoning item")
	}
	assertAssistantMessageContainsText(t, items, "thinking")
}

// Case 6: drop summary + drop. RoundTripEncryptedDrop clears encrypted
// payloads, so no reasoning item is emitted even when a ref is present.
func TestPhase6DropSummaryDropEncryptedEmitsNoReasoningItemWhenRefPresent(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRefAndEncrypted("rs_id", "thinking", "ENC_ignored"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedDrop,
	})
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected no reasoning item")
	}
	assertAssistantMessageContainsText(t, items, "thinking")
}

func TestPhase6RefOnlyMarkerNeverEmitsReasoningItemUnderStoreFalse(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeWithRef("rs_poisoned", ""), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
	})
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected no reasoning item for ref-only marker under store=false")
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
	r, ok := findFirstReasoningItem(items)
	if !ok {
		t.Fatalf("expected reasoning item carrying encrypted_content")
	}
	if r.EncryptedContent != "ENC" {
		t.Fatalf("encrypted_content = %q", r.EncryptedContent)
	}
	if r.Summary == nil {
		t.Fatalf("summary slice must be present (Codex Responses requires it; empty array allowed)")
	}
	if len(r.Summary) != 0 {
		t.Fatalf("summary must be an empty slice, got %v", r.Summary)
	}
	idx, _, found := findFirstAssistantMessage(items)
	if !found {
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
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected no reasoning item")
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
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected no reasoning item")
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
	if _, ok := findFirstReasoningItem(items); ok {
		t.Fatalf("expected NO reasoning item for legacy no-ref under drop+drop")
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
			if _, ok := findFirstReasoningItem(items); ok {
				t.Fatalf("summary=%s: expected NO reasoning item when ref is empty", mode)
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
		switch {
		case item.Type == codexwire.ItemTypeReasoning && reasoningIdx < 0:
			reasoningIdx = idx
		case item.Type == codexwire.ItemTypeMessage && item.Role == "assistant" && messageIdx < 0:
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
// native_summary_field. This test exists to ensure the toInputItem
// renderer never reverts to omitempty-style behavior on summary.
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
			out := tc.item.toInputItem()
			if out.Summary == nil {
				t.Fatalf("summary slice must always be non-nil; out=%+v", out)
			}
			if len(tc.item.Summary) != len(out.Summary) {
				t.Fatalf("summary length mismatch: in=%d out=%d", len(tc.item.Summary), len(out.Summary))
			}
		})
	}
}
