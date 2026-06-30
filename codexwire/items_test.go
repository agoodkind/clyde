package codexwire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInputItemMessageMarshalRoundtrip(t *testing.T) {
	t.Parallel()
	in := InputItem{
		Type: ItemTypeMessage,
		Role: "user",
		Content: ContentItems{
			{Type: ContentItemInputText, Text: "hello"},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"message"`) {
		t.Fatalf("expected message discriminator in %s", string(raw))
	}
	if !strings.Contains(string(raw), `"role":"user"`) {
		t.Fatalf("expected role=user in %s", string(raw))
	}
	if strings.Contains(string(raw), `"call_id"`) {
		t.Fatalf("call_id should be omitted for message; got %s", string(raw))
	}
	var back InputItem
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Type != ItemTypeMessage || back.Role != "user" || back.Content.Text() != "hello" {
		t.Fatalf("roundtrip lost data: %+v", back)
	}
}

func TestInputItemFunctionCallOutputMarshalsStringOutput(t *testing.T) {
	t.Parallel()
	in := InputItem{
		Type:   ItemTypeFunctionCallOutput,
		CallID: "call_1",
		Output: RawOutput("done"),
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"output":"done"`) {
		t.Fatalf("expected string output, got %s", string(raw))
	}
}

func TestOutputTextHandlesStringAndArrayShapes(t *testing.T) {
	t.Parallel()
	if got := OutputText(json.RawMessage(`"plain"`)); got != "plain" {
		t.Fatalf("string output text=%q want plain", got)
	}
	arr := json.RawMessage(`[{"type":"input_text","text":"line one"},{"type":"input_text","text":"line two"}]`)
	if got := OutputText(arr); got != "line one\nline two" {
		t.Fatalf("array output text=%q want joined lines", got)
	}
	if got := OutputText(nil); got != "" {
		t.Fatalf("nil output text=%q want empty", got)
	}
}

func TestCommandValueUnmarshalsStringOrArgv(t *testing.T) {
	t.Parallel()
	var c CommandValue
	if err := c.UnmarshalJSON([]byte(`"pwd"`)); err != nil {
		t.Fatalf("string unmarshal: %v", err)
	}
	if len(c) != 1 || c[0] != "pwd" {
		t.Fatalf("string command=%v want [pwd]", c)
	}
	c = nil
	if err := c.UnmarshalJSON([]byte(`["/bin/sh","-lc","echo hi"]`)); err != nil {
		t.Fatalf("argv unmarshal: %v", err)
	}
	if len(c) != 3 || c[2] != "echo hi" {
		t.Fatalf("argv command=%v want 3 entries", c)
	}
}

func TestLocalShellActionWorkingDirFallback(t *testing.T) {
	t.Parallel()
	a := LocalShellAction{Workdir: "/repo"}
	if a.WorkingDir() != "/repo" {
		t.Fatalf("workdir fallback failed: %q", a.WorkingDir())
	}
	a = LocalShellAction{WorkingDirectory: "/proj", Cwd: "/other"}
	if a.WorkingDir() != "/proj" {
		t.Fatalf("preferred working_directory: %q", a.WorkingDir())
	}
}

// TestReasoningItemAlwaysMarshalsSummary asserts a reasoning input item
// with no summary text still serializes `"summary":[]` on the wire. The
// Codex Responses API treats summary as required on reasoning items and
// rejects the request with [ObjectParam] [input[i].summary]
// [missing_required_parameter] when it is absent, which the omitempty tag
// would otherwise cause for a replayed reasoning item carrying only
// encrypted_content. A struct-level check passes on a nil/empty slice, so
// this asserts the marshaled bytes specifically.
func TestReasoningItemAlwaysMarshalsSummary(t *testing.T) {
	t.Parallel()
	in := InputItem{Type: ItemTypeReasoning, ID: "rs_1", EncryptedContent: "ENC"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"summary":[]`) {
		t.Fatalf("reasoning item must emit summary:[], got %s", string(raw))
	}
	if !strings.Contains(string(raw), `"encrypted_content":"ENC"`) {
		t.Fatalf("reasoning item lost encrypted_content: %s", string(raw))
	}
}

// TestReasoningItemPreservesSummaryEntries asserts non-empty summary text
// round-trips through the custom marshaler unchanged.
func TestReasoningItemPreservesSummaryEntries(t *testing.T) {
	t.Parallel()
	in := InputItem{
		Type:    ItemTypeReasoning,
		ID:      "rs_2",
		Summary: []ReasoningSummary{{Type: "summary_text", Text: "step one"}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"summary":[{"type":"summary_text","text":"step one"}]`) {
		t.Fatalf("reasoning summary entries lost: %s", string(raw))
	}
	var back InputItem
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Summary) != 1 || back.Summary[0].Text != "step one" {
		t.Fatalf("roundtrip lost summary: %+v", back.Summary)
	}
}

// TestNonReasoningItemOmitsSummary asserts the summary field stays omitted
// for non-reasoning item types so the custom marshaler is scoped to the
// one variant that needs it.
func TestNonReasoningItemOmitsSummary(t *testing.T) {
	t.Parallel()
	in := InputItem{
		Type:    ItemTypeMessage,
		Role:    "assistant",
		Content: ContentItems{{Type: ContentItemOutputText, Text: "x"}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"summary"`) {
		t.Fatalf("message item must omit summary, got %s", string(raw))
	}
}
