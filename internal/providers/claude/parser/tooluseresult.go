package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
)

// ToolUseResultKind names the JSON shape Claude wrote for a transcript entry's
// top-level toolUseResult field. The field is a union whose shape depends on
// the tool that produced it, so the kind selects which member of
// [ToolUseResult] carries the decoded value.
//
// This is the entry-level toolUseResult, not the tool_result content block
// inside message.content that [attachToolOutputs] reads.
type ToolUseResultKind string

const (
	// ToolUseResultKindAbsent marks an entry with no toolUseResult, or a null one.
	ToolUseResultKindAbsent ToolUseResultKind = ""
	// ToolUseResultKindText marks a plain string result, which tools use for
	// errors and for output that carries no structure.
	ToolUseResultKindText ToolUseResultKind = "text"
	// ToolUseResultKindDetail marks a structured object result.
	ToolUseResultKindDetail ToolUseResultKind = "detail"
	// ToolUseResultKindBlocks marks a list of MCP content blocks.
	ToolUseResultKindBlocks ToolUseResultKind = "blocks"
	// ToolUseResultKindUnsupported marks a JSON shape this parser does not
	// model, such as a bare number or boolean. Raw still holds the value.
	ToolUseResultKindUnsupported ToolUseResultKind = "unsupported"
)

// ToolUseResult is the decoded toolUseResult union. Raw always holds the
// original JSON, including for the unsupported kind, so no result value is
// lost even when its shape is not modeled.
type ToolUseResult struct {
	Kind   ToolUseResultKind
	Text   string
	Detail *ToolUseResultDetail
	Blocks []ToolUseResultBlock
	Raw    json.RawMessage
}

// ToolUseResultDetailType names the structured-result variants Claude tags.
type ToolUseResultDetailType string

const (
	// ToolUseResultDetailTypeUnspecified marks a result with no type tag.
	ToolUseResultDetailTypeUnspecified ToolUseResultDetailType = ""
	// ToolUseResultDetailTypeText marks a file read returned as text.
	ToolUseResultDetailTypeText ToolUseResultDetailType = "text"
	// ToolUseResultDetailTypeCreate marks a file the tool created.
	ToolUseResultDetailTypeCreate ToolUseResultDetailType = "create"
	// ToolUseResultDetailTypeUpdate marks a file the tool rewrote.
	ToolUseResultDetailTypeUpdate ToolUseResultDetailType = "update"
)

// ToolUseResultDetail models the structured result keys the high-volume tools
// write: the shell family, the file-read family, and the file-write and
// file-edit family. Every tool defines its own further keys, so the remainder
// stays in [ToolUseResult].Raw rather than being flattened into this struct.
type ToolUseResultDetail struct {
	Type ToolUseResultDetailType `json:"type"`

	// Shell results.
	Stdout                   string `json:"stdout"`
	Stderr                   string `json:"stderr"`
	Interrupted              bool   `json:"interrupted"`
	IsImage                  bool   `json:"isImage"`
	NoOutputExpected         bool   `json:"noOutputExpected"`
	ReturnCodeInterpretation string `json:"returnCodeInterpretation"`
	TimedOutAfterMs          int    `json:"timedOutAfterMs"`
	BackgroundTaskID         string `json:"backgroundTaskId"`
	BackgroundCwdHint        string `json:"backgroundCwdHint"`

	// File reads.
	File *ToolUseResultFile `json:"file"`

	// File writes and edits.
	FilePath        string                   `json:"filePath"`
	Content         ToolUseResultContent     `json:"content"`
	OriginalFile    string                   `json:"originalFile"`
	UserModified    bool                     `json:"userModified"`
	OldString       string                   `json:"oldString"`
	NewString       string                   `json:"newString"`
	ReplaceAll      bool                     `json:"replaceAll"`
	StructuredPatch []ToolUseResultPatchHunk `json:"structuredPatch"`

	// Keys shared across the remaining tools.
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// ToolUseResultFile is the file payload a read returns, including the window
// the tool actually read when the file was longer than the read limit.
type ToolUseResultFile struct {
	FilePath            string `json:"filePath"`
	Content             string `json:"content"`
	NumLines            int    `json:"numLines"`
	StartLine           int    `json:"startLine"`
	TotalLines          int    `json:"totalLines"`
	TruncatedByTokenCap bool   `json:"truncatedByTokenCap"`
}

// ToolUseResultPatchHunk is one hunk of the diff an edit or write produced.
type ToolUseResultPatchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

// ToolUseResultContent is the content member of a structured result. A tool
// writes it either as the file text or as a list of content blocks, so both
// forms are modeled and Raw keeps the original either way.
type ToolUseResultContent struct {
	Text   string
	Blocks []ToolUseResultBlock
	Raw    json.RawMessage
}

// emptyToolUseResultContent returns the zero content value, written out so
// exhaustruct sees every field set.
func emptyToolUseResultContent() ToolUseResultContent {
	return ToolUseResultContent{Text: "", Blocks: nil, Raw: nil}
}

// UnmarshalJSON decodes the content member from either of its two forms. A
// third shape keeps Raw and returns no error, on the same reasoning as
// [ToolUseResult.UnmarshalJSON].
func (content *ToolUseResultContent) UnmarshalJSON(data []byte) error {
	*content = emptyToolUseResultContent()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	content.Raw = append(json.RawMessage(nil), data...)
	switch trimmed[0] {
	case '"':
		if err := json.Unmarshal(trimmed, &content.Text); err != nil {
			slog.Debug("providers.claude.parser.tool_use_result_content_text_failed", "concern", concern, "component", "claude", "err", err)
			return fmt.Errorf("decode claude tool use result content text: %w", err)
		}
		return nil
	case '[':
		if err := json.Unmarshal(trimmed, &content.Blocks); err != nil {
			slog.Debug("providers.claude.parser.tool_use_result_content_blocks_failed", "concern", concern, "component", "claude", "err", err)
			return fmt.Errorf("decode claude tool use result content blocks: %w", err)
		}
		return nil
	default:
		slog.Debug("providers.claude.parser.tool_use_result_content_unsupported", "concern", concern, "component", "claude", "shape", string(trimmed[:1]))
		return nil
	}
}

// ToolUseResultBlock is one content block of an MCP tool result. Text blocks
// carry Type and Text; a resource link carries the resource fields instead.
type ToolUseResultBlock struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Name        string `json:"name"`
	URI         string `json:"uri"`
	MimeType    string `json:"mimeType"`
	Description string `json:"description"`
	Server      string `json:"server"`
}

// emptyToolUseResult returns the zero union value, written out so exhaustruct
// sees every field set.
func emptyToolUseResult() ToolUseResult {
	return ToolUseResult{
		Kind:   ToolUseResultKindAbsent,
		Text:   "",
		Detail: nil,
		Blocks: nil,
		Raw:    nil,
	}
}

// UnmarshalJSON decodes the toolUseResult union by its JSON shape. A shape
// this parser does not model decodes to [ToolUseResultKindUnsupported] with
// the value preserved in Raw and no error, because a result shape clyde does
// not read must not cost the reader the rest of the transcript entry.
func (result *ToolUseResult) UnmarshalJSON(data []byte) error {
	*result = emptyToolUseResult()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	result.Raw = append(json.RawMessage(nil), data...)
	switch trimmed[0] {
	case '"':
		if err := json.Unmarshal(trimmed, &result.Text); err != nil {
			slog.Debug("providers.claude.parser.tool_use_result_text_failed", "concern", concern, "component", "claude", "err", err)
			return fmt.Errorf("decode claude tool use result text: %w", err)
		}
		result.Kind = ToolUseResultKindText
		return nil
	case '{':
		var detail ToolUseResultDetail
		result.Kind = ToolUseResultKindDetail
		if err := json.Unmarshal(trimmed, &detail); err != nil {
			slog.Debug("providers.claude.parser.tool_use_result_detail_failed", "concern", concern, "component", "claude", "err", err)
			result.Detail = &detail
			return fmt.Errorf("decode claude tool use result detail: %w", err)
		}
		result.Detail = &detail
		return nil
	case '[':
		var blocks []ToolUseResultBlock
		result.Kind = ToolUseResultKindBlocks
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			slog.Debug("providers.claude.parser.tool_use_result_blocks_failed", "concern", concern, "component", "claude", "err", err)
			return fmt.Errorf("decode claude tool use result blocks: %w", err)
		}
		result.Blocks = blocks
		return nil
	default:
		slog.Debug("providers.claude.parser.tool_use_result_unsupported", "concern", concern, "component", "claude", "shape", string(trimmed[:1]))
		result.Kind = ToolUseResultKindUnsupported
		return nil
	}
}
