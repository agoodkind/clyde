package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/content"
)

// screenshotBase64 stands in for a real screenshot's inline bytes. No neutral
// part may carry it: base64 costs about one token per byte, so a reader that
// copies it into text blows a token budget sized for prose.
const screenshotBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestNormalizeContentPlainString(t *testing.T) {
	t.Parallel()
	parts, kind := NormalizeContent(json.RawMessage(`"just words"`))
	if kind != content.KindString {
		t.Fatalf("kind = %v, want KindString", kind)
	}
	if len(parts) != 1 || parts[0].Kind != content.PartText || parts[0].Text != "just words" {
		t.Fatalf("parts = %#v, want one text part", parts)
	}
}

func TestNormalizeContentMapsEveryBlockKind(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[` +
		`{"type":"text","text":"answer"},` +
		`{"type":"thinking","thinking":"weighing it","signature":"sig"},` +
		`{"type":"redacted_thinking","data":"opaque"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + screenshotBase64 + `"}},` +
		`{"type":"tool_use","id":"tu1","name":"computer","input":{"action":"screenshot"}},` +
		`{"type":"tool_result","tool_use_id":"tu1","content":"plain output"},` +
		`{"type":"server_tool_use","id":"st1"}` +
		`]`)
	parts, kind := NormalizeContent(raw)
	if kind != content.KindParts {
		t.Fatalf("kind = %v, want KindParts", kind)
	}
	wantKinds := []content.PartKind{
		content.PartText,
		content.PartThinking,
		content.PartThinking,
		content.PartImage,
		content.PartToolUse,
		content.PartToolResult,
		content.PartUnsupported,
	}
	if len(parts) != len(wantKinds) {
		t.Fatalf("parts = %d, want %d", len(parts), len(wantKinds))
	}
	for index, want := range wantKinds {
		if parts[index].Kind != want {
			t.Errorf("part %d kind = %q, want %q", index, parts[index].Kind, want)
		}
	}
	if parts[1].Text != "weighing it" {
		t.Errorf("thinking text = %q, want the reasoning body", parts[1].Text)
	}
	if parts[2].Text != "" {
		t.Errorf("redacted thinking text = %q, want empty; the payload is opaque", parts[2].Text)
	}
	if parts[3].Image == nil || parts[3].Image.Detail != "image/png" {
		t.Errorf("image part = %#v, want the media type and no bytes", parts[3].Image)
	}
	if parts[4].ID != "tu1" || parts[4].Name != "computer" {
		t.Errorf("tool use part = %#v, want the call id and name", parts[4])
	}
	if parts[5].ToolUseID != "tu1" || parts[5].Text != "plain output" {
		t.Errorf("tool result part = %#v, want the answered id and its output", parts[5])
	}
	if parts[6].WireType != "server_tool_use" {
		t.Errorf("unsupported part wire type = %q, want the block's own type", parts[6].WireType)
	}
}

// TestNormalizeContentToolResultBlocksKeepImagesOutOfText is the regression the
// live overflow needs: a screenshot tool returns text plus an image inside one
// tool_result, and the neutral parts must carry the words and the placeholder
// without the inline bytes.
func TestNormalizeContentToolResultBlocksKeepImagesOutOfText(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu1","content":[` +
		`{"type":"text","text":"screenshot captured"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + screenshotBase64 + `"}}` +
		`]}]`)
	parts, kind := NormalizeContent(raw)
	if kind != content.KindParts {
		t.Fatalf("kind = %v, want KindParts", kind)
	}
	if len(parts) != 1 || parts[0].Kind != content.PartToolResult {
		t.Fatalf("parts = %#v, want one tool result part", parts)
	}
	if !strings.Contains(parts[0].Text, "screenshot captured") {
		t.Errorf("tool result text = %q, want the tool's words", parts[0].Text)
	}
	if !strings.Contains(parts[0].Text, "[image]") {
		t.Errorf("tool result text = %q, want the image placeholder", parts[0].Text)
	}
	if strings.Contains(parts[0].Text, screenshotBase64) {
		t.Error("tool result text carries the inline image bytes")
	}
}

func TestFlattenContentRendersImagePlaceholder(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[` +
		`{"type":"text","text":"look at this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + screenshotBase64 + `"}}` +
		`]`)
	flat := FlattenContent(raw)
	if flat != "look at this[image]" {
		t.Fatalf("FlattenContent = %q, want the words plus the placeholder", flat)
	}
}

func TestNormalizeContentEmptyAndUndecodable(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{"} {
		parts, kind := NormalizeContent(json.RawMessage(raw))
		if kind != content.KindEmpty || parts != nil {
			t.Errorf("NormalizeContent(%q) = %#v/%v, want nil/KindEmpty", raw, parts, kind)
		}
	}
}
