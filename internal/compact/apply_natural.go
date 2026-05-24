package compact

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// preludeEntryArgs holds the typed fields needed to emit the prelude
// user entry that follows the compact_boundary in the natural-shape
// apply path. The prelude carries the continuity notice, the
// dropped-stats summary, and (optionally) the LLM-generated recap of
// dropped content. Survivor entries follow the prelude, chained via
// parentUuid.
type preludeEntryArgs struct {
	UUID       string
	ParentUUID string
	SessionID  string
	Cwd        string
	Version    string
	Timestamp  time.Time
	Slice      *Slice
	Options    SynthOptions
}

// MarshalPreludeEntry emits the prelude user message that introduces
// the surviving entries to the continuing agent. Content is a single
// text block containing the continuity notice, the dropped-stats
// summary, and the optional LLM-generated recap. Named with a
// Marshal* shape so callers can treat it as a pure encoder; errors
// surface to the orchestrator which logs them with full context.
func MarshalPreludeEntry(a preludeEntryArgs) ([]byte, error) {
	headerText := renderHeader(a.Slice, a.Options)
	content := []OutputBlock{{Text: headerText, Image: nil}}
	message := syntheticMessage{Role: "user", Content: content}
	fields, err := orderedFields(
		field("parentUuid", optionalString(a.ParentUUID)),
		field("isSidechain", false),
		field("type", "user"),
		field("isCompactSummary", true),
		field("timestamp", a.Timestamp.Format(time.RFC3339Nano)),
		field("uuid", a.UUID),
		field("message", message),
		field("cwd", a.Cwd),
		field("sessionId", a.SessionID),
		field("version", a.Version),
	)
	if err != nil {
		return nil, err
	}
	return orderedJSON(fields).Marshal()
}

// survivorEntryArgs holds the per-survivor input needed to emit one
// natural-shape entry copied from slice.PostBoundary with the configured
// drops and transforms applied.
type survivorEntryArgs struct {
	Source     Entry
	UUID       string
	ParentUUID string
	SessionID  string
	Cwd        string
	Version    string
	Timestamp  time.Time
	Options    SynthOptions
}

// buildSurvivorEntries iterates slice.PostBoundary in order and emits
// one JSONL line per surviving entry. Drops (chat axis), transforms
// (thinking/images/tools axes), and tool detail demotions all happen
// here. Each emitted line carries a fresh UUID and a parentUuid that
// chains the survivors together starting from preludeUUID.
//
// Returns the list of JSONL lines, the list of new UUIDs (for ledger
// telemetry), and the final parentUuid in the chain (the last
// survivor's UUID; used by callers that need to know the leaf).
func buildSurvivorEntries(
	slice *Slice,
	opts SynthOptions,
	preludeUUID string,
	sessionID string,
	cwd string,
	version string,
	now time.Time,
) ([][]byte, []string, string, error) {
	if slice == nil {
		return nil, nil, "", fmt.Errorf("buildSurvivorEntries: nil slice")
	}
	lines := make([][]byte, 0, len(slice.PostBoundary))
	uuids := make([]string, 0, len(slice.PostBoundary))
	parentUUID := preludeUUID
	for entryIndex, entry := range slice.PostBoundary {
		if opts.DroppedChatEntries != nil && opts.DroppedChatEntries[entryIndex] {
			continue
		}
		if !entryTypeIsCopyable(entry.Type) {
			continue
		}
		transformedContent, keep := transformSurvivorContent(entry.Content, opts)
		if !keep && entry.TextOnly != "" {
			// String-form message.content: claude stores some user prompts
			// as message.content = "...string..." instead of an array. Treat
			// them like a single text block so they survive instead of
			// silently disappearing.
			transformedContent = []json.RawMessage{encodeSurvivorTextBlock(entry.TextOnly)}
			keep = true
		}
		if !keep {
			continue
		}
		newUUID := uuid.NewString()
		// Stagger timestamps by entryIndex so the order is unambiguous on
		// disk. The original timestamp is preserved by skipping its use
		// entirely; survivors are new entries minted at Apply time.
		ts := now.Add(time.Duration(entryIndex+1) * time.Millisecond)
		line, err := MarshalSurvivorEntryLine(survivorEntryArgs{
			Source:     entry,
			UUID:       newUUID,
			ParentUUID: parentUUID,
			SessionID:  sessionID,
			Cwd:        cwd,
			Version:    version,
			Timestamp:  ts,
			Options:    opts,
		}, transformedContent)
		if err != nil {
			slog.Error("compact.apply.survivor_build_failed",
				"component", "compact",
				"subcomponent", "apply",
				"session_id", sessionID,
				"entry_index", entryIndex,
				"entry_type", entry.Type,
				"err", err,
			)
			return nil, nil, "", fmt.Errorf("build survivor entry %d: %w", entryIndex, err)
		}
		lines = append(lines, line)
		uuids = append(uuids, newUUID)
		parentUUID = newUUID
	}
	return lines, uuids, parentUUID, nil
}

// entryTypeIsCopyable reports whether a post-boundary entry's type
// should be carried forward as a survivor at all. Only user and
// assistant chat-shaped entries qualify; system entries (compact
// boundaries from prior runs, turn-duration markers, stop-hook
// summaries) carry no chat-side content the continuing agent needs to
// see.
func entryTypeIsCopyable(rawType string) bool {
	switch entryType(rawType) {
	case entryTypeUser, entryTypeAssistant:
		return true
	case entryTypeSystem:
		return false
	}
	return false
}

// transformSurvivorContent applies the configured per-block transforms
// to one entry's content array and returns the transformed list along
// with a "keep" flag that is false when every block was dropped.
//
// The transforms mirror what the planner's measure path applies via
// applySynthOptionsToTranscript, so the projection the planner counts
// matches the bytes Apply writes.
func transformSurvivorContent(
	blocks []ContentBlock,
	opts SynthOptions,
) ([]json.RawMessage, bool) {
	out := make([]json.RawMessage, 0, len(blocks))
	for _, b := range blocks {
		raw, keep := transformSurvivorBlock(b, opts)
		if !keep {
			continue
		}
		out = append(out, raw)
	}
	return out, len(out) > 0
}

// transformSurvivorBlock applies the configured transform to one
// content block. Returns the JSON bytes to emit and a keep flag. A
// false keep means "drop this block entirely."
func transformSurvivorBlock(
	b ContentBlock,
	opts SynthOptions,
) (json.RawMessage, bool) {
	switch contentBlockType(b.Type) {
	case contentBlockTypeText:
		return b.Raw, true
	case contentBlockTypeThinking, contentBlockTypeRedactedThinking:
		if opts.DropThinking {
			return nil, false
		}
		return b.Raw, true
	case contentBlockTypeImage:
		if opts.ImagesAsPlaceholder {
			return survivorImagePlaceholder(b), true
		}
		return b.Raw, true
	case contentBlockTypeToolUse:
		detail := survivorToolDetail(b.ToolUseID, opts)
		switch detail {
		case ToolDetailDrop:
			return nil, false
		case ToolDetailLineOnly:
			return survivorToolUseLineOnly(b), true
		case ToolDetailFull:
			return b.Raw, true
		}
	case contentBlockTypeToolResult:
		detail := survivorToolDetail(b.ToolUseRefID, opts)
		switch detail {
		case ToolDetailDrop:
			return nil, false
		case ToolDetailLineOnly:
			return nil, false
		case ToolDetailFull:
			return b.Raw, true
		}
	}
	if len(b.Raw) > 0 {
		return b.Raw, true
	}
	return nil, false
}

// survivorToolDetail resolves the tool detail level for one tool_use
// id, falling back to opts.ToolDefault when no override exists.
func survivorToolDetail(toolUseID string, opts SynthOptions) ToolDetail {
	if opts.ToolDetailOverride != nil {
		if detail, ok := opts.ToolDetailOverride[toolUseID]; ok {
			return detail
		}
	}
	return opts.ToolDefault
}

// survivorImagePlaceholder mints a text content block that stands in
// for an image when --images is enabled. The text describes the image
// so the model sees the same visual reference, just textually.
func survivorImagePlaceholder(b ContentBlock) json.RawMessage {
	mediaType := b.ImageMediaType
	if mediaType == "" {
		mediaType = "unknown"
	}
	text := fmt.Sprintf("[image: %s, %d bytes]", mediaType, b.ImageBytes)
	return encodeSurvivorTextBlock(text)
}

// survivorToolUseLineOnly mints a text block that summarises a
// demoted tool_use. The paired tool_result is dropped separately in
// transformSurvivorBlock.
func survivorToolUseLineOnly(b ContentBlock) json.RawMessage {
	name := b.ToolName
	if name == "" {
		name = "tool"
	}
	text := fmt.Sprintf("[tool call demoted: %s (id=%s)]", name, b.ToolUseID)
	return encodeSurvivorTextBlock(text)
}

// encodeSurvivorTextBlock marshals a small text content block as the
// JSON shape claude expects: {"type":"text","text":"..."}.
func encodeSurvivorTextBlock(text string) json.RawMessage {
	payload := outputTextPayload{Type: string(contentBlockTypeText), Text: text}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{"type":"text","text":""}`)
	}
	return encoded
}

// survivorMessage is the inner `message` object on a survivor entry.
// Content is a list of pre-encoded JSON blocks so the transform stays
// fully typed at the boundary.
type survivorMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// MarshalSurvivorEntryLine emits one survivor entry as a JSONL line.
// The field order mirrors the existing synthetic-user entry so
// downstream boundary scanners and chain walkers see a familiar
// shape. Named with a Marshal* shape so callers can treat it as a
// pure encoder; errors surface to the orchestrator which logs them
// with full context.
func MarshalSurvivorEntryLine(a survivorEntryArgs, content []json.RawMessage) ([]byte, error) {
	message := survivorMessage{Role: a.Source.Role, Content: content}
	fields, err := orderedFields(
		field("parentUuid", optionalString(a.ParentUUID)),
		field("isSidechain", false),
		field("type", a.Source.Type),
		field("timestamp", a.Timestamp.Format(time.RFC3339Nano)),
		field("uuid", a.UUID),
		field("message", message),
		field("cwd", a.Cwd),
		field("sessionId", a.SessionID),
		field("version", a.Version),
	)
	if err != nil {
		return nil, err
	}
	return orderedJSON(fields).Marshal()
}
