package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// fakeReasoningGetter is a typed test double for [ReasoningStoreGetter].
// It returns the configured blob (if any) or the configured error; nil
// blob with nil error models a store miss.
type fakeReasoningGetter struct {
	blobs map[string]*ReasoningBlob
	err   error
	calls int
}

func (f *fakeReasoningGetter) Get(_ context.Context, chatKey, itemID string) (*ReasoningBlob, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	key := chatKey + "/" + itemID
	blob, ok := f.blobs[key]
	if !ok {
		return nil, nil
	}
	return blob, nil
}

// envelopeWithRef builds a synthetic-thinking envelope around body with
// the given data-ref attribute. The shape matches the renderer's emitted
// open marker so [adapterrender.ExtractSyntheticParts] returns the same
// (Body, Ref) the inbound mapper sees in production.
func envelopeWithRef(ref, body string) string {
	return adapterrender.FormatSyntheticContentDeltaWithRef(
		adapterrender.SyntheticReasoning, true, ref, body,
	) + adapterrender.SyntheticContentClose(adapterrender.SyntheticReasoning)
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
	out := BuildRequestWithConfig(context.Background(), req, model, "", cfg)
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

// Case 1: native_summary_field + round_trip + hit.
func TestPhase6NativeSummaryRoundTripHitEmitsBothFields(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{
		blobs: map[string]*ReasoningBlob{
			"chat-1/rs_abc": {ChatKey: "chat-1", ItemID: "rs_abc", Encrypted: "ENC123"},
		},
	}
	items := buildPhase6Request(t, envelopeWithRef("rs_abc", "deep thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
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

// Case 2: native_summary_field + round_trip + miss.
func TestPhase6NativeSummaryRoundTripMissOmitsEncrypted(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{blobs: nil}
	cfg := RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
	}
	items := buildPhase6Request(t, envelopeWithRef("rs_miss", "thinking"), cfg)
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item")
	}
	if _, has := r["encrypted_content"]; has {
		t.Fatalf("encrypted_content should be absent on miss, got %#v", r)
	}
	if r["id"] != "rs_miss" {
		t.Fatalf("id = %v", r["id"])
	}
	if got := extractSummaryTexts(r); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("summary = %v", got)
	}
}

// Case 3: native_summary_field + drop.
func TestPhase6NativeSummaryDropEncryptedSkipsLookup(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{
		blobs: map[string]*ReasoningBlob{
			"chat-1/rs_abc": {Encrypted: "ENC"},
		},
	}
	items := buildPhase6Request(t, envelopeWithRef("rs_abc", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedDrop,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
	})
	if getter.calls != 0 {
		t.Fatalf("getter must not be called when encrypted=drop, calls=%d", getter.calls)
	}
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

// Case 4: drop summary + round_trip + hit.
func TestPhase6DropSummaryRoundTripHitOnlyEncrypted(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{
		blobs: map[string]*ReasoningBlob{
			"chat-1/rs_abc": {Encrypted: "ENC"},
		},
	}
	items := buildPhase6Request(t, envelopeWithRef("rs_abc", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item")
	}
	if _, has := r["summary"]; has {
		t.Fatalf("summary must be absent under drop summary mode")
	}
	if r["encrypted_content"] != "ENC" {
		t.Fatalf("encrypted_content = %v", r["encrypted_content"])
	}
}

// Case 5: drop summary + round_trip + miss.
func TestPhase6DropSummaryRoundTripMissEmitsStubID(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{}
	items := buildPhase6Request(t, envelopeWithRef("rs_only", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected stub reasoning item with just id")
	}
	if r["id"] != "rs_only" {
		t.Fatalf("id = %v", r["id"])
	}
	if _, has := r["summary"]; has {
		t.Fatalf("summary must be absent")
	}
	if _, has := r["encrypted_content"]; has {
		t.Fatalf("encrypted_content must be absent on miss")
	}
}

// Case 6: drop summary + drop.
func TestPhase6DropSummaryDropEncryptedEmitsStubIDWhenRefPresent(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{}
	items := buildPhase6Request(t, envelopeWithRef("rs_id", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedDrop,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
	})
	if getter.calls != 0 {
		t.Fatalf("getter must not be called under drop+drop, calls=%d", getter.calls)
	}
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning stub")
	}
	if r["id"] != "rs_id" {
		t.Fatalf("id = %v", r["id"])
	}
	if _, has := r["summary"]; has {
		t.Fatalf("summary must be absent")
	}
}

// Case 7: plain_text_concat + round_trip + hit. The body is folded into
// the assistant message; the encrypted_content rides in a separate
// Reasoning item that precedes the message.
func TestPhase6PlainTextConcatRoundTripHitFoldsBodyAndEmitsEncrypted(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{
		blobs: map[string]*ReasoningBlob{
			"chat-1/rs_abc": {Encrypted: "ENC"},
		},
	}
	items := buildPhase6Request(t, envelopeWithRef("rs_abc", "deep thinking")+"\nfinal answer", RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryPlainText,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
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
	if _, has := r["summary"]; has {
		t.Fatalf("summary must be absent under plain_text_concat")
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

// Case 8: plain_text_concat + round_trip + miss. No reasoning item
// emitted; body still folded into the message.
func TestPhase6PlainTextConcatRoundTripMissEmitsNoReasoningItem(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{}
	items := buildPhase6Request(t, envelopeWithRef("rs_miss", "thinking")+"\nanswer", RequestBuilderConfig{
		RoundTripSummary:               RoundTripSummaryPlainText,
		RoundTripEncrypted:             RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:              getter,
		ChatKey:                        "chat-1",
		InboundThinkingMaterialization: adapterrender.MaterializePlainTextConcat,
	})
	if r := findFirstReasoningItem(items); r != nil {
		t.Fatalf("expected no reasoning item, got %#v", r)
	}
}

// Case 9: plain_text_concat + drop. No reasoning item; no lookup.
func TestPhase6PlainTextConcatDropEncryptedSkipsLookupAndItem(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{
		blobs: map[string]*ReasoningBlob{
			"chat-1/rs_abc": {Encrypted: "ENC"},
		},
	}
	items := buildPhase6Request(t, envelopeWithRef("rs_abc", "thinking")+"\nanswer", RequestBuilderConfig{
		RoundTripSummary:               RoundTripSummaryPlainText,
		RoundTripEncrypted:             RoundTripEncryptedDrop,
		ReasoningStoreGet:              getter,
		ChatKey:                        "chat-1",
		InboundThinkingMaterialization: adapterrender.MaterializePlainTextConcat,
	})
	if getter.calls != 0 {
		t.Fatalf("getter must not be called, calls=%d", getter.calls)
	}
	if r := findFirstReasoningItem(items); r != nil {
		t.Fatalf("expected no reasoning item, got %#v", r)
	}
}

// Legacy attribute-less marker should be dropped silently when summary
// mode is native_summary_field/drop AND encrypted is drop AND there is
// no ref (no payload to round-trip).
func TestPhase6LegacyNoRefDropsSilentlyUnderDropEncrypted(t *testing.T) {
	t.Parallel()
	items := buildPhase6Request(t, envelopeNoRef("legacy thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryDrop,
		RoundTripEncrypted: RoundTripEncryptedDrop,
		ChatKey:            "chat-1",
	})
	if r := findFirstReasoningItem(items); r != nil {
		t.Fatalf("expected NO reasoning item for legacy no-ref under drop+drop, got %#v", r)
	}
}

// Reasoning item must precede its assistant message in input order, even
// when the summary mode keeps the body in the message text.
func TestPhase6ReasoningPrecedesMessage(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{
		blobs: map[string]*ReasoningBlob{
			"chat-1/rs_abc": {Encrypted: "ENC"},
		},
	}
	items := buildPhase6Request(t, envelopeWithRef("rs_abc", "thinking")+"\nfinal answer", RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
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

// Errors from the store are treated as misses; the request build must
// not surface them.
func TestPhase6StoreErrorIsTreatedAsMiss(t *testing.T) {
	t.Parallel()
	getter := &fakeReasoningGetter{err: errors.New("boom")}
	items := buildPhase6Request(t, envelopeWithRef("rs_abc", "thinking"), RequestBuilderConfig{
		RoundTripSummary:   RoundTripSummaryNative,
		RoundTripEncrypted: RoundTripEncryptedRoundTrip,
		ReasoningStoreGet:  getter,
		ChatKey:            "chat-1",
	})
	r := findFirstReasoningItem(items)
	if r == nil {
		t.Fatalf("expected reasoning item even on store error")
	}
	if _, has := r["encrypted_content"]; has {
		t.Fatalf("encrypted_content must be absent on store error")
	}
}

// adapteropenai compile-time guard: the Cursor-shape ChatRequest is the
// same one the package uses elsewhere; importing it here keeps the test
// file's intent visible.
var _ = adapteropenai.ChatRequest{}
