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
