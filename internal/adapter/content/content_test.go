package content

import (
	"encoding/json"
	"testing"
)

func TestNormalizeRawString(t *testing.T) {
	parts, kind := NormalizeRaw(json.RawMessage(`"hello"`))
	if kind != KindString {
		t.Fatalf("kind=%v want %v", kind, KindString)
	}
	if len(parts) != 1 || parts[0].Kind != PartText || parts[0].Text != "hello" {
		t.Fatalf("parts=%#v", parts)
	}
}

func TestNormalizeRawTextAndImageParts(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"input_text","text":"look"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,abc","detail":"high"}}
	]`)

	parts, kind := NormalizeRaw(raw)
	if kind != KindParts {
		t.Fatalf("kind=%v want %v", kind, KindParts)
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts)=%d want 2", len(parts))
	}
	if parts[0].Kind != PartText || parts[0].WireType != "input_text" || parts[0].Text != "look" {
		t.Fatalf("text part=%#v", parts[0])
	}
	if parts[1].Kind != PartImage || parts[1].Image == nil {
		t.Fatalf("image part=%#v", parts[1])
	}
	if parts[1].Image.URL != "data:image/png;base64,abc" || parts[1].Image.Detail != "high" {
		t.Fatalf("image=%#v", parts[1].Image)
	}
}

func TestNormalizeRawResponsesInputImageString(t *testing.T) {
	raw := json.RawMessage(`{"type":"input_image","image_url":"https://example.test/image.png","detail":"low"}`)

	parts, kind := NormalizeRaw(raw)
	if kind != KindParts {
		t.Fatalf("kind=%v want %v", kind, KindParts)
	}
	if len(parts) != 1 || parts[0].Kind != PartImage || parts[0].Image == nil {
		t.Fatalf("parts=%#v", parts)
	}
	if parts[0].Image.URL != "https://example.test/image.png" || parts[0].Image.Detail != "low" {
		t.Fatalf("image=%#v", parts[0].Image)
	}
}

func TestNormalizeRawSpecialParts(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"refusal","refusal":"no"},
		{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"done"}]},
		{"type":"made_up","text":"x"}
	]`)

	parts, kind := NormalizeRaw(raw)
	if kind != KindParts {
		t.Fatalf("kind=%v want %v", kind, KindParts)
	}
	if parts[0].Kind != PartRefusal || parts[0].Refusal != "no" {
		t.Fatalf("refusal=%#v", parts[0])
	}
	if parts[1].Kind != PartToolResult || parts[1].ToolUseID != "toolu_1" || len(parts[1].Content) == 0 {
		t.Fatalf("tool result=%#v", parts[1])
	}
	if parts[2].Kind != PartUnsupported || parts[2].WireType != "made_up" {
		t.Fatalf("unsupported=%#v", parts[2])
	}
}

func TestFlattenRawIsLossyTextView(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"look "},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}
	]`)

	if got := FlattenRaw(raw); got != "look [image]" {
		t.Fatalf("FlattenRaw=%q", got)
	}
}
