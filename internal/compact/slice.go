package compact

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"
)

type entryType string

const (
	entryTypeUser      entryType = "user"
	entryTypeAssistant entryType = "assistant"
	entryTypeSystem    entryType = "system"
)

type contentBlockType string

const (
	contentBlockTypeText             contentBlockType = "text"
	contentBlockTypeThinking         contentBlockType = "thinking"
	contentBlockTypeRedactedThinking contentBlockType = "redacted_thinking"
	contentBlockTypeImage            contentBlockType = "image"
	contentBlockTypeToolUse          contentBlockType = "tool_use"
	contentBlockTypeToolResult       contentBlockType = "tool_result"
)

// Entry is a parsed JSONL transcript line. We keep both the structured
// fields we need for synthesis AND the raw bytes so the apply path can
// re-emit untouched lines verbatim if it ever needs to.
type Entry struct {
	LineIndex int    // 0-based line index in the on-disk file
	Raw       []byte // full original line (no trailing newline)

	// Decoded fields (best-effort, missing fields stay zero).
	UUID       string
	ParentUUID string
	Type       string // "user" | "assistant" | "system"
	Subtype    string // for system entries: e.g. "compact_boundary"
	Timestamp  time.Time
	IsMeta     bool
	IsSummary  bool // isCompactSummary flag

	// Message body. Lazily decoded into typed views.
	Role     string
	Content  []ContentBlock // [] when message.content is a plain string
	TextOnly string         // populated when message.content was a string

	// RehydratedFrom is the PostBoundary index of the synthetic entry
	// this virtual entry was expanded from. -1 for real entries.
	RehydratedFrom int `json:"-"`

	// RehydratedChunkKey is the syntheticSummary chunk key this virtual
	// entry represents. Empty for real entries.
	RehydratedChunkKey string `json:"-"`

	// RehydratedSource is the original synthetic Entry that was expanded.
	// Set only on the first virtual entry produced per expansion. Nil for
	// real entries and for non-first virtual entries.
	RehydratedSource *Entry `json:"-"`
}

// ContentBlock is one element of a provider content array.
// Unknown block types are kept verbatim as Raw so we can pass them
// through if needed.
type ContentBlock struct {
	Type string

	// type=text
	Text string

	// type=thinking / redacted_thinking
	Thinking string

	// type=image
	ImageMediaType string
	ImageDataB64   string
	ImageBytes     int

	// type=tool_use
	ToolUseID string
	ToolName  string
	ToolInput json.RawMessage

	// type=tool_result
	ToolUseRefID string
	ToolIsError  bool
	ToolContent  []ContentBlock // tool_result.content can itself be a content array

	// Raw preserves the original block bytes so synth.go can fall back
	// to verbatim emission for unknown types.
	Raw json.RawMessage
}

// Slice is the post-boundary view of a session.
type Slice struct {
	Path         string
	AllEntries   []Entry // every parsed line, in file order
	BoundaryLine int     // file line index of the most-recent compact_boundary; -1 if none
	BoundaryUUID string  // uuid of that boundary entry
	BoundaryTime time.Time
	PostBoundary []Entry // entries strictly after BoundaryLine

	// PairIndex maps a tool_use id to the (postBoundaryIdx, blockIdx) of
	// the matching tool_result block, when one exists in PostBoundary.
	// Used by the strip phase so demoting a tool removes both halves.
	PairIndex map[string]ToolPair

	// FileBytes is the byte length of Path at read time; recorded so
	// apply can detect concurrent writers via length comparison before
	// truncating on undo.
	FileBytes int64
}

// ToolPair points at the assistant tool_use block and (when present)
// the matching user tool_result block within the post-boundary slice.
type ToolPair struct {
	UseEntryIdx    int // index into Slice.PostBoundary
	UseBlockIdx    int // index into PostBoundary[UseEntryIdx].Content
	ResultEntryIdx int // -1 when no matching tool_result on disk
	ResultBlockIdx int
}

// LoadSlice reads the JSONL at path, parses every line, locates the
// most recent compact_boundary system entry, and returns a Slice
// containing both the full entry list and the post-boundary tail.
//
// Lines that fail to parse are preserved as Entry.Raw with empty
// structured fields so the file always round-trips byte-for-byte.
func LoadSlice(path string) (*Slice, error) {
	f, err := os.Open(path)
	if err != nil {
		slog.Error("compact.slice.open_failed", "component", "compact", "path", path, "err", err)
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil {
		slog.Error("compact.slice.stat_failed", "component", "compact", "path", path, "err", err)
		return nil, fmt.Errorf("stat transcript: %w", err)
	}

	scanner := bufio.NewScanner(f)
	// JSONL lines can be very large (image base64 payloads). Allow up
	// to 64 MiB per line so we never silently truncate.
	scanner.Buffer(make([]byte, 1<<20), 64*1024*1024)

	slice := &Slice{
		Path:         path,
		AllEntries:   nil,
		BoundaryLine: -1,
		BoundaryUUID: "",
		BoundaryTime: time.Time{},
		PostBoundary: nil,
		PairIndex:    nil,
		FileBytes:    stat.Size(),
	}
	idx := 0
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		entry := parseEntry(raw, idx)
		slice.AllEntries = append(slice.AllEntries, entry)
		if entry.Type == "system" && entry.Subtype == "compact_boundary" {
			slice.BoundaryLine = idx
			slice.BoundaryUUID = entry.UUID
			slice.BoundaryTime = entry.Timestamp
		}
		idx++
	}
	if err := scanner.Err(); err != nil {
		slog.Error("compact.slice.scan_failed", "component", "compact", "path", path, "err", err)
		return nil, fmt.Errorf("scan transcript: %w", err)
	}

	if slice.BoundaryLine >= 0 {
		slice.PostBoundary = slice.AllEntries[slice.BoundaryLine+1:]
	} else {
		slice.PostBoundary = slice.AllEntries
	}
	slice.PairIndex = buildPairIndex(slice.PostBoundary)
	return slice, nil
}

// parseEntry decodes one JSONL line. Unknown fields are tolerated.
// Errors during decode degrade gracefully: the entry's structured
// fields are left zero and Raw still carries the original bytes so
// the apply path can pass the line through unchanged if needed.
func parseEntry(raw []byte, idx int) Entry {
	entry := Entry{
		LineIndex:          idx,
		Raw:                raw,
		UUID:               "",
		ParentUUID:         "",
		Type:               "",
		Subtype:            "",
		Timestamp:          time.Time{},
		IsMeta:             false,
		IsSummary:          false,
		Role:               "",
		Content:            nil,
		TextOnly:           "",
		RehydratedFrom:     -1,
		RehydratedChunkKey: "",
		RehydratedSource:   nil,
	}
	var top struct {
		UUID       string          `json:"uuid"`
		ParentUUID string          `json:"parentUuid"`
		Type       string          `json:"type"`
		Subtype    string          `json:"subtype"`
		Timestamp  time.Time       `json:"timestamp"`
		IsMeta     bool            `json:"isMeta"`
		IsSummary  bool            `json:"isCompactSummary"`
		Message    json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return entry
	}
	entry.UUID = top.UUID
	entry.ParentUUID = top.ParentUUID
	entry.Type = top.Type
	entry.Subtype = top.Subtype
	entry.Timestamp = top.Timestamp
	entry.IsMeta = top.IsMeta
	entry.IsSummary = top.IsSummary

	if len(top.Message) > 0 {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(top.Message, &msg) == nil {
			entry.Role = msg.Role
			entry.Content, entry.TextOnly = decodeContent(msg.Content)
		}
	}
	return entry
}

// decodeContent handles the two shapes message.content can take in
// stored transcripts: a plain string OR an array of typed blocks.
func decodeContent(raw json.RawMessage) ([]ContentBlock, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	// String form.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return nil, s
		}
		return nil, ""
	}
	// Array form.
	if raw[0] != '[' {
		return nil, ""
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, ""
	}
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, decodeBlock(b))
	}
	return out, ""
}

func decodeBlock(raw json.RawMessage) ContentBlock {
	block := ContentBlock{
		Type:           "",
		Text:           "",
		Thinking:       "",
		ImageMediaType: "",
		ImageDataB64:   "",
		ImageBytes:     0,
		ToolUseID:      "",
		ToolName:       "",
		ToolInput:      nil,
		ToolUseRefID:   "",
		ToolIsError:    false,
		ToolContent:    nil,
		Raw:            raw,
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return block
	}
	block.Type = head.Type
	switch contentBlockType(block.Type) {
	case contentBlockTypeText:
		var v struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &v); err == nil {
			block.Text = v.Text
		}
	case contentBlockTypeThinking, contentBlockTypeRedactedThinking:
		var v struct {
			Thinking string `json:"thinking"`
			Data     string `json:"data"`
		}
		if err := json.Unmarshal(raw, &v); err == nil {
			block.Thinking = v.Thinking
			if block.Thinking == "" {
				block.Thinking = v.Data
			}
		}
	case contentBlockTypeImage:
		var v struct {
			Source struct {
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source"`
		}
		if err := json.Unmarshal(raw, &v); err == nil {
			block.ImageMediaType = v.Source.MediaType
			block.ImageDataB64 = v.Source.Data
			// base64 decoded length is approximately len(data)*3/4.
			block.ImageBytes = (len(v.Source.Data) * 3) / 4
		}
	case contentBlockTypeToolUse:
		var v struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &v); err == nil {
			block.ToolUseID = v.ID
			block.ToolName = v.Name
			block.ToolInput = v.Input
		}
	case contentBlockTypeToolResult:
		var v struct {
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &v); err == nil {
			block.ToolUseRefID = v.ToolUseID
			block.ToolIsError = v.IsError
			if len(v.Content) > 0 {
				switch v.Content[0] {
				case '[':
					sub, _ := decodeContent(v.Content)
					block.ToolContent = sub
				case '"':
					var s string
					if json.Unmarshal(v.Content, &s) == nil {
						block.ToolContent = []ContentBlock{{
							Type:           string(contentBlockTypeText),
							Text:           s,
							Thinking:       "",
							ImageMediaType: "",
							ImageDataB64:   "",
							ImageBytes:     0,
							ToolUseID:      "",
							ToolName:       "",
							ToolInput:      nil,
							ToolUseRefID:   "",
							ToolIsError:    false,
							ToolContent:    nil,
							Raw:            nil,
						}}
					}
				}
			}
		}
	}
	return block
}

// buildPairIndex scans post-boundary entries for tool_use blocks (in
// assistant entries) and matches them to tool_result blocks (in user
// entries) by id.
func buildPairIndex(entries []Entry) map[string]ToolPair {
	idx := map[string]ToolPair{}
	for ei, e := range entries {
		for bi, b := range e.Content {
			if b.Type == "tool_use" && b.ToolUseID != "" {
				pair := idx[b.ToolUseID]
				pair.UseEntryIdx = ei
				pair.UseBlockIdx = bi
				if pair.ResultEntryIdx == 0 && pair.ResultBlockIdx == 0 {
					pair.ResultEntryIdx = -1
				}
				idx[b.ToolUseID] = pair
			}
		}
	}
	for ei, e := range entries {
		for bi, b := range e.Content {
			if b.Type == "tool_result" && b.ToolUseRefID != "" {
				if pair, ok := idx[b.ToolUseRefID]; ok {
					pair.ResultEntryIdx = ei
					pair.ResultBlockIdx = bi
					idx[b.ToolUseRefID] = pair
				}
			}
		}
	}
	return idx
}
