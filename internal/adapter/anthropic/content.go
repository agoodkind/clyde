package anthropic

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/internal/adapter/content"
)

// inboundBlock decodes one content block of an Anthropic request clyde reads
// rather than builds.
//
// [ContentBlock] cannot serve this read: it types a tool_result's content as a
// string because the egress builder only ever emits the string form, while an
// inbound tool_result carries an array of blocks whenever a tool returns
// anything besides plain text (a screenshot tool returns text plus an image).
// Decoding that array into a string field fails and would lose the whole
// message, so the inbound shape keeps the field raw.
type inboundBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Source    *ImageSource    `json:"source"`
}

// wireBlockType is Anthropic's discriminator on a content block. The neutral
// content boundary speaks the OpenAI dialect, so this package owns the mapping
// from Anthropic's own type strings into that boundary rather than letting an
// unmapped Anthropic block fall through the neutral normalizer's
// unsupported-type branch.
type wireBlockType string

const (
	wireBlockText             wireBlockType = "text"
	wireBlockThinking         wireBlockType = "thinking"
	wireBlockRedactedThinking wireBlockType = "redacted_thinking"
	wireBlockImage            wireBlockType = "image"
	wireBlockToolUse          wireBlockType = "tool_use"
	wireBlockToolResult       wireBlockType = "tool_result"
)

// NormalizeContent maps an Anthropic message's content field into the neutral
// content parts the adapter boundary defines, so a caller that reads or renders
// a conversation never decodes Anthropic's own block shapes.
//
// Content is a plain string or an array of blocks. A tool_result block's nested
// content is normalized through the same mapping, so an image inside a tool
// result reaches the caller as a neutral image part rather than as base64 text.
func NormalizeContent(raw json.RawMessage) ([]content.Part, content.Kind) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, content.KindEmpty
	}
	if trimmed[0] == '"' {
		var plain string
		if err := json.Unmarshal(raw, &plain); err != nil {
			return nil, content.KindEmpty
		}
		return []content.Part{textPart(plain)}, content.KindString
	}
	var blocks []inboundBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, content.KindEmpty
	}
	parts := make([]content.Part, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, normalizeBlock(block))
	}
	return parts, content.KindParts
}

// FlattenContent is the lossy text view of an Anthropic content field: text and
// reasoning contribute their words, an image contributes its placeholder, and a
// tool result contributes its own flattened content.
func FlattenContent(raw json.RawMessage) string {
	parts, kind := NormalizeContent(raw)
	if kind == content.KindString {
		if len(parts) == 0 {
			return ""
		}
		return parts[0].Text
	}
	return content.FlattenParts(parts)
}

func normalizeBlock(block inboundBlock) content.Part {
	part := emptyPart(strings.TrimSpace(block.Type))
	switch wireBlockType(part.WireType) {
	case wireBlockText:
		part.Kind = content.PartText
		part.Text = block.Text
	case wireBlockThinking:
		part.Kind = content.PartThinking
		part.Text = block.Thinking
	case wireBlockRedactedThinking:
		// The body is an opaque encrypted blob with no readable words, so the
		// neutral part carries the kind and no text.
		part.Kind = content.PartThinking
	case wireBlockImage:
		part.Kind = content.PartImage
		part.Image = imagePart(block.Source)
	case wireBlockToolUse:
		part.Kind = content.PartToolUse
		part.ID = block.ID
		part.Name = block.Name
		part.Input = block.Input
	case wireBlockToolResult:
		part.Kind = content.PartToolResult
		part.ToolUseID = block.ToolUseID
		part.Content = block.Content
		part.Text = flattenToolResult(block.Content)
	default:
		part.Kind = content.PartUnsupported
	}
	return part
}

// flattenToolResult renders a tool_result block's own content: a string passes
// through, and an array of blocks is normalized through the same mapping so a
// returned image becomes its placeholder instead of its base64 bytes.
func flattenToolResult(nested json.RawMessage) string {
	trimmed := strings.TrimSpace(string(nested))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var plain string
		if err := json.Unmarshal(nested, &plain); err != nil {
			return ""
		}
		return plain
	}
	parts, kind := NormalizeContent(nested)
	if kind != content.KindParts {
		return ""
	}
	rendered := make([]string, 0, len(parts))
	for _, part := range parts {
		text := content.FlattenParts([]content.Part{part})
		if text == "" {
			continue
		}
		rendered = append(rendered, text)
	}
	return strings.Join(rendered, "\n")
}

// imagePart describes an image without carrying its bytes. A base64 source
// holds the image inline; every reader of this boundary wants to know an image
// was there, not to carry its encoding as text, so the neutral part keeps the
// media type and drops the payload.
func imagePart(source *ImageSource) *content.Image {
	if source == nil {
		return nil
	}
	if url := strings.TrimSpace(source.URL); url != "" {
		return &content.Image{URL: url, Detail: ""}
	}
	return &content.Image{URL: "", Detail: strings.TrimSpace(source.MediaType)}
}

func textPart(text string) content.Part {
	part := emptyPart("text")
	part.Kind = content.PartText
	part.Text = text
	return part
}

func emptyPart(wireType string) content.Part {
	if wireType == "" {
		wireType = "text"
	}
	return content.Part{
		Kind:      "",
		WireType:  wireType,
		Text:      "",
		Image:     nil,
		Audio:     nil,
		Refusal:   "",
		ToolUseID: "",
		Content:   nil,
		ID:        "",
		Name:      "",
		Input:     nil,
	}
}
