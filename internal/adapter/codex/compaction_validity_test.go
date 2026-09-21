package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

// compactionValidityFixture is a v2 compaction request with every item kind
// the split counts: two setup items before the transcript, four call kinds
// with their outputs, a reasoning item, and an unfinished turn before the
// trigger.
func compactionValidityFixture() []byte {
	return []byte(`{"model":"gpt-native","input":[` +
		`{"type":"additional_tools","role":"developer"},` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"setup instructions"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"oldest question about the build"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking about the build"}],"encrypted_content":"cipher-1"},` +
		`{"type":"function_call","name":"exec","arguments":"{\"cmd\":\"make build\"}","call_id":"call-f"},` +
		`{"type":"function_call_output","call_id":"call-f","output":"build listing with several lines of output"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"oldest answer about the build"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"older question"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},` +
		`{"type":"custom_tool_call","call_id":"call-c","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},` +
		`{"type":"custom_tool_call_output","call_id":"call-c","output":"patch applied"},` +
		`{"type":"local_shell_call","call_id":"call-s","action":{"type":"exec","command":["pwd"]}},` +
		`{"type":"function_call_output","call_id":"call-s","output":"/repo"},` +
		`{"type":"tool_search_call","call_id":"call-t","arguments":{"query":"calendar"}},` +
		`{"type":"tool_search_output","call_id":"call-t","tools":[{"name":"calendar.search"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"older answer with the patch result"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"newer unfinished question"}]},` +
		`{"type":"custom_tool_call","call_id":"call-pending","name":"apply_patch","input":"pending"},` +
		`{"type":"compaction_trigger"}` +
		`]}`)
}

// TestTruncateCompactionInputStaysValidAtEveryCut drives TruncateCompactionInput
// at every message, segment, and head length the planner can choose, and
// asserts the forwarded input satisfies what the Responses API requires of a
// compaction request: every call has its output, no reasoning item ends the
// kept prefix, the setup items and the unfinished suffix are byte-identical,
// and no call arguments changed.
func TestTruncateCompactionInputStaysValidAtEveryCut(t *testing.T) {
	t.Parallel()
	body := compactionValidityFixture()
	decoded, ok := DecodeCompactionInput(body)
	if !ok {
		t.Fatal("fixture did not decode as a compaction request")
	}
	if decoded.RetainStart != 2 || decoded.InstructionStart != 15 {
		t.Fatalf("layout = retain %d instruction %d, want 2 and 15", decoded.RetainStart, decoded.InstructionStart)
	}
	original := inputItemsForTest(t, body)
	for messageIndex := decoded.RetainStart; messageIndex < decoded.InstructionStart; messageIndex++ {
		for segmentIndex, segment := range decoded.Messages[messageIndex].Segments {
			for _, headRunes := range headRunesForTest(segment) {
				cut := CompactionCut{MessageIndex: messageIndex, SegmentIndex: segmentIndex, HeadRunes: headRunes}
				t.Run(fmt.Sprintf("m%d/s%d/h%d", messageIndex, segmentIndex, headRunes), func(t *testing.T) {
					forwarded, err := TruncateCompactionInput(body, cut, decoded.RetainStart, decoded.InstructionStart)
					if err != nil {
						t.Fatalf("TruncateCompactionInput: %v", err)
					}
					assertCompactionInputValid(t, original, inputItemsForTest(t, forwarded), decoded)
				})
			}
		}
	}
}

func headRunesForTest(segment CompactionSegment) []int {
	full := len([]rune(segment.Text))
	if segment.Atomic || full < 2 {
		return []int{0, full}
	}
	return []int{0, full / 2, full}
}

func inputItemsForTest(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode input: %v", err)
	}
	return request.Input
}

func assertCompactionInputValid(t *testing.T, original, forwarded []json.RawMessage, decoded CompactionInput) {
	t.Helper()
	suffix := original[decoded.InstructionStart:]
	if len(forwarded) < decoded.RetainStart+len(suffix) {
		t.Fatalf("forwarded input has %d items", len(forwarded))
	}
	for index := range decoded.RetainStart {
		if !bytes.Equal(forwarded[index], original[index]) {
			t.Fatalf("setup item %d changed: %s", index, forwarded[index])
		}
	}
	kept := forwarded[decoded.RetainStart : len(forwarded)-len(suffix)]
	for index, item := range suffix {
		if !bytes.Equal(forwarded[len(forwarded)-len(suffix)+index], item) {
			t.Fatalf("instruction item %d changed: %s", index, forwarded[len(forwarded)-len(suffix)+index])
		}
	}
	normalized := codexstore.NormalizeResponseInputItems(kept)
	if !rawCompactionPairsAreComplete(normalized) {
		t.Fatalf("kept prefix has an unanswered or duplicate call: %s", kept)
	}
	if len(normalized) > 0 && normalized[len(normalized)-1].Kind == transcript.CompactedContextItemKindReasoning {
		t.Fatalf("kept prefix ends with a reasoning item: %s", kept)
	}
	assertCallArgumentsUnchanged(t, original, kept)
}

// assertCallArgumentsUnchanged checks every call item in kept against the
// original item with the same call id. The truncation never shortens a call.
func assertCallArgumentsUnchanged(t *testing.T, original, kept []json.RawMessage) {
	t.Helper()
	byCallID := make(map[string]json.RawMessage)
	for _, item := range codexstore.NormalizeResponseInputItems(original) {
		if callID, _, ok := rawCompactionCall(item); ok {
			byCallID[callID] = nil
		}
	}
	for _, raw := range original {
		var probe struct {
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(raw, &probe) == nil {
			if _, isCall := byCallID[probe.CallID]; isCall && byCallID[probe.CallID] == nil {
				byCallID[probe.CallID] = raw
			}
		}
	}
	for index, item := range codexstore.NormalizeResponseInputItems(kept) {
		callID, _, ok := rawCompactionCall(item)
		if !ok {
			continue
		}
		if !bytes.Equal(kept[index], byCallID[callID]) {
			t.Fatalf("call %s changed: %s", callID, kept[index])
		}
	}
}
