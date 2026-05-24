package compact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"goodkind.io/clyde/internal/contextcount"
)

type transcriptTransform struct {
	dropThinking        bool
	imagesAsPlaceholder bool
	toolDefault         ToolDetail
	toolDetailOverride  map[string]ToolDetail
	droppedMessages     map[int]bool
	summaryChunkDrops   map[int]map[string]bool
	summaryEntries      map[int]Entry
	survivingToolUseIDs map[string]bool
}

func applySynthOptionsToTranscript(
	transcript contextcount.Transcript,
	slice *Slice,
	opts SynthOptions,
) contextcount.Transcript {
	transform := transcriptTransform{
		dropThinking:        opts.DropThinking,
		imagesAsPlaceholder: opts.ImagesAsPlaceholder,
		toolDefault:         opts.ToolDefault,
		toolDetailOverride:  opts.ToolDetailOverride,
		droppedMessages:     nil,
		summaryChunkDrops:   nil,
		summaryEntries:      nil,
		survivingToolUseIDs: nil,
	}
	transform.droppedMessages, transform.summaryChunkDrops, transform.summaryEntries = transcriptDropPlan(
		slice,
		opts,
	)
	transform.survivingToolUseIDs = collectTranscriptSurvivingToolUseIDs(transcript, transform)
	return applyTranscriptTransform(transcript, transform)
}

// collectTranscriptSurvivingToolUseIDs walks the transcript the same
// way applyTranscriptTransform will, after dropped-message and
// tool-detail rules apply, and returns the set of tool_use ids that
// will survive into the projected transcript. Used to gate
// tool_result blocks in transformToolResultBlock so the planner's
// projection matches the orphan-guard Apply path emits.
//
// Mirrors collectSurvivingToolUseIDs in apply_natural.go on the
// contextcount.Transcript shape rather than the Entry shape.
func collectTranscriptSurvivingToolUseIDs(
	transcript contextcount.Transcript,
	transform transcriptTransform,
) map[string]bool {
	keep := map[string]bool{}
	for messageIndex, message := range transcript.Messages {
		if transform.droppedMessages != nil && transform.droppedMessages[messageIndex] {
			continue
		}
		for _, block := range message.Content {
			if transcriptBlockType(block.Type) != transcriptBlockToolUse {
				continue
			}
			if block.ToolUseID == "" {
				continue
			}
			if transform.toolDetail(block.ToolUseID) == ToolDetailDrop {
				continue
			}
			keep[block.ToolUseID] = true
		}
	}
	return keep
}

func applyTranscriptTransform(
	transcript contextcount.Transcript,
	transform transcriptTransform,
) contextcount.Transcript {
	out := cloneContextTranscript(transcript)
	messages := make([]contextcount.Message, 0, len(out.Messages))
	for messageIndex, message := range out.Messages {
		if transform.droppedMessages != nil && transform.droppedMessages[messageIndex] {
			continue
		}
		if dropped := transform.summaryChunkDrops[messageIndex]; len(dropped) > 0 {
			if entry, ok := transform.summaryEntries[messageIndex]; ok {
				message.Content = remainingSyntheticSummaryContent(entry, dropped)
			}
		}
		message.Content = transformContentBlocks(message.Content, transform)
		messages = append(messages, message)
	}
	out.Messages = messages
	return out
}

func transformContentBlocks(
	blocks []contextcount.ContentBlock,
	transform transcriptTransform,
) []contextcount.ContentBlock {
	out := make([]contextcount.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		transformed, keep := transformContentBlock(block, transform)
		if !keep {
			continue
		}
		out = append(out, transformed)
	}
	return out
}

func transformContentBlock(
	block contextcount.ContentBlock,
	transform transcriptTransform,
) (contextcount.ContentBlock, bool) {
	switch transcriptBlockType(block.Type) {
	case transcriptBlockText:
		return block, true
	case transcriptBlockThinking, transcriptBlockRedactedThinking:
		if transform.dropThinking {
			return newTextContentBlock(""), false
		}
		return block, true
	case transcriptBlockImage:
		if transform.imagesAsPlaceholder {
			return imagePlaceholderBlock(block), true
		}
		return block, true
	case transcriptBlockToolUse:
		return transformToolUseBlock(block, transform)
	case transcriptBlockToolResult:
		return transformToolResultBlock(block, transform)
	default:
		if len(block.ToolResult) > 0 {
			block.ToolResult = transformContentBlocks(block.ToolResult, transform)
		}
		return block, true
	}
}

func transformToolUseBlock(
	block contextcount.ContentBlock,
	transform transcriptTransform,
) (contextcount.ContentBlock, bool) {
	detail := transform.toolDetail(block.ToolUseID)
	switch detail {
	case ToolDetailDrop:
		return newTextContentBlock(""), false
	case ToolDetailLineOnly:
		return newTextContentBlock(transcriptToolUseSummary(block)), true
	case ToolDetailFull:
		return block, true
	default:
		return block, true
	}
}

func transformToolResultBlock(
	block contextcount.ContentBlock,
	transform transcriptTransform,
) (contextcount.ContentBlock, bool) {
	// Orphan-result guard: when the matching tool_use was dropped (by
	// the chat axis or by tool-detail Drop), drop the tool_result too,
	// matching what Apply writes via apply_natural.go. Without this
	// gate, the planner's projection counts tool_result blocks that
	// Apply will remove, and the floor contract is violated when the
	// planner stops trimming believing the projection is safe.
	if transform.survivingToolUseIDs != nil && !transform.survivingToolUseIDs[block.ToolUseID] {
		return newTextContentBlock(""), false
	}
	detail := transform.toolDetail(block.ToolUseID)
	switch detail {
	case ToolDetailDrop:
		return newTextContentBlock(""), false
	case ToolDetailLineOnly:
		return newTextContentBlock(transcriptToolResultSummary(block)), true
	case ToolDetailFull:
		block.ToolResult = transformContentBlocks(block.ToolResult, transform)
		return block, true
	default:
		block.ToolResult = transformContentBlocks(block.ToolResult, transform)
		return block, true
	}
}

func (t transcriptTransform) toolDetail(toolUseID string) ToolDetail {
	if t.toolDetailOverride != nil {
		if detail, ok := t.toolDetailOverride[toolUseID]; ok {
			return detail
		}
	}
	return t.toolDefault
}

func imagePlaceholderBlock(block contextcount.ContentBlock) contextcount.ContentBlock {
	mediaType := strings.TrimSpace(block.ImageSource.MediaType)
	if mediaType == "" {
		mediaType = "unknown"
	}
	return newTextContentBlock("[image: " + mediaType + "]")
}

func transcriptToolUseSummary(block contextcount.ContentBlock) string {
	name := strings.TrimSpace(block.ToolName)
	if name == "" {
		name = "tool"
	}
	parts := []string{"tool_use: " + name}
	if block.ToolUseID != "" {
		parts = append(parts, "id="+block.ToolUseID)
	}
	if len(bytes.TrimSpace(block.ToolInput)) > 0 {
		parts = append(parts, "input="+singleLineJSON(block.ToolInput, 160))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func transcriptToolResultSummary(block contextcount.ContentBlock) string {
	parts := []string{"tool_result:"}
	if block.ToolUseID != "" {
		parts = append(parts, "id="+block.ToolUseID)
	}
	charCount := transcriptToolResultCharCount(block)
	parts = append(parts, fmt.Sprintf("chars=%d", charCount))
	if block.IsError {
		parts = append(parts, "error=true")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func transcriptToolResultCharCount(block contextcount.ContentBlock) int {
	if block.ToolResultText != "" {
		return len(block.ToolResultText)
	}
	total := 0
	for _, nested := range block.ToolResult {
		if nested.Text != "" {
			total += len(nested.Text)
		}
		total += transcriptToolResultCharCount(nested)
	}
	return total
}

func singleLineJSON(raw json.RawMessage, maxBytes int) string {
	compacted := &bytes.Buffer{}
	if err := json.Compact(compacted, raw); err != nil {
		compacted.Write(bytes.TrimSpace(raw))
	}
	text := compacted.String()
	text = strings.ReplaceAll(text, "\n", " ")
	if maxBytes > 0 && len(text) > maxBytes {
		return text[:maxBytes] + "..."
	}
	return text
}

func transcriptDropPlan(
	slice *Slice,
	opts SynthOptions,
) (map[int]bool, map[int]map[string]bool, map[int]Entry) {
	if len(opts.DroppedChatEntries) == 0 && len(opts.DroppedSummaryChunks) == 0 {
		return nil, nil, nil
	}
	entryToMessage := messageIndexByPostBoundaryIndex(slice)
	droppedMessages := map[int]bool{}
	summaryDrops := map[int]map[string]bool{}
	summaryEntries := map[int]Entry{}
	for entryIndex := range opts.DroppedChatEntries {
		if messageIndex, ok := entryToMessage[entryIndex]; ok {
			droppedMessages[messageIndex] = true
		}
	}
	for entryIndex := range opts.DroppedSummaryChunks {
		entry, entryOK := postBoundaryEntry(slice, entryIndex)
		if !entryOK {
			continue
		}
		if messageIndex, ok := entryToMessage[entryIndex]; ok {
			summaryDrops[messageIndex] = opts.DroppedSummaryChunks[entryIndex]
			summaryEntries[messageIndex] = entry
		}
	}
	return droppedMessages, summaryDrops, summaryEntries
}

func postBoundaryEntry(slice *Slice, entryIndex int) (Entry, bool) {
	if slice == nil || entryIndex < 0 || entryIndex >= len(slice.PostBoundary) {
		return emptyEntryForTransform(), false
	}
	return slice.PostBoundary[entryIndex], true
}

func emptyEntryForTransform() Entry {
	return Entry{
		LineIndex:          0,
		Raw:                nil,
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
}

func remainingSyntheticSummaryContent(
	entry Entry,
	dropped map[string]bool,
) []contextcount.ContentBlock {
	summary, ok := parseSyntheticSummary(entry)
	if !ok {
		return nil
	}
	blocks := remainingSyntheticSummaryBlocks(summary, dropped)
	content := make([]contextcount.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, newTextContentBlock(block))
	}
	return content
}

func remainingSyntheticSummaryBlocks(summary *syntheticSummary, dropped map[string]bool) []string {
	blocks := []string{}
	header := remainingSyntheticHeader(summary, dropped)
	if header != "" {
		blocks = append(blocks, header)
	}
	blocks = append(blocks, remainingSyntheticTurns(summary, dropped)...)
	if toolBlock := remainingSyntheticToolBlock(summary, dropped); toolBlock != "" {
		blocks = append(blocks, toolBlock)
	}
	if summary.HasContinue && !dropped[summaryChunkContinue] {
		blocks = append(blocks, "## Continue from here.\n")
	}
	return blocks
}

func remainingSyntheticHeader(summary *syntheticSummary, dropped map[string]bool) string {
	if dropped[summaryChunkContinuity] &&
		(dropped[summaryChunkWhat] || summary.WhatDropped == "") &&
		(dropped[summaryChunkSummary] || summary.DroppedSummary == "") {
		return ""
	}
	var sb strings.Builder
	if !dropped[summaryChunkContinuity] {
		sb.WriteString("## Context continuity notice\n\n")
		sb.WriteString(strings.TrimSpace(summary.Continuity))
		sb.WriteString("\n\n")
	}
	if summary.WhatDropped != "" && !dropped[summaryChunkWhat] {
		sb.WriteString("### What was dropped\n\n")
		sb.WriteString(strings.TrimSpace(summary.WhatDropped))
		sb.WriteString("\n\n")
	}
	if summary.DroppedSummary != "" && !dropped[summaryChunkSummary] {
		sb.WriteString("### Summary of dropped content\n\n")
		sb.WriteString(strings.TrimSpace(summary.DroppedSummary))
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Surviving transcript\n\n")
	return sb.String()
}

func remainingSyntheticTurns(summary *syntheticSummary, dropped map[string]bool) []string {
	turns := make([]string, 0, len(summary.TranscriptTurns))
	for i, turn := range summary.TranscriptTurns {
		if dropped[fmt.Sprintf("surviving_turn:%d", i)] {
			continue
		}
		prefix, parts := splitSyntheticTranscriptTurn(turn)
		if len(parts) == 0 {
			turns = append(turns, turn)
			continue
		}
		var sb strings.Builder
		sb.WriteString(prefix)
		for partIndex, part := range parts {
			key := fmt.Sprintf("surviving_turn:%d%s%d", i, syntheticTurnPartPrefix, partIndex)
			if dropped[key] {
				continue
			}
			sb.WriteString(part)
		}
		if strings.TrimSpace(sb.String()) != "" {
			turns = append(turns, sb.String())
		}
	}
	return turns
}

func remainingSyntheticToolBlock(summary *syntheticSummary, dropped map[string]bool) string {
	items := make([]string, 0, len(summary.ToolItems))
	for i, item := range summary.ToolItems {
		if dropped[fmt.Sprintf("tool_item:%d", i)] {
			continue
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return ""
	}
	return "## Tool activity\n\n" + strings.Join(items, "\n\n") + "\n\n"
}

func messageIndexByPostBoundaryIndex(slice *Slice) map[int]int {
	out := map[int]int{}
	if slice == nil {
		return out
	}
	messageIndex := 0
	for postIndex, entry := range slice.PostBoundary {
		if !isTranscriptMessageEntry(entry) {
			continue
		}
		out[postIndex] = messageIndex
		messageIndex++
	}
	return out
}

func isTranscriptMessageEntry(entry Entry) bool {
	if entry.Type != "user" && entry.Type != "assistant" {
		return false
	}
	return !isSidechainEntry(entry)
}

func cloneContextTranscript(transcript contextcount.Transcript) contextcount.Transcript {
	out := contextcount.Transcript{
		Model:    transcript.Model,
		System:   make([]contextcount.TextBlock, len(transcript.System)),
		Tools:    make([]contextcount.ToolDef, len(transcript.Tools)),
		Messages: make([]contextcount.Message, len(transcript.Messages)),
	}
	copy(out.System, transcript.System)
	for i, tool := range transcript.Tools {
		out.Tools[i] = contextcount.ToolDef{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: cloneRawMessage(tool.InputSchema),
		}
	}
	for i, message := range transcript.Messages {
		out.Messages[i] = contextcount.Message{
			Role:    message.Role,
			Content: cloneContextContentBlocks(message.Content),
		}
	}
	return out
}

func cloneContextContentBlocks(blocks []contextcount.ContentBlock) []contextcount.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]contextcount.ContentBlock, len(blocks))
	for i, block := range blocks {
		out[i] = cloneContextContentBlock(block)
	}
	return out
}

func cloneContextContentBlock(block contextcount.ContentBlock) contextcount.ContentBlock {
	return contextcount.ContentBlock{
		Type:           block.Type,
		Text:           block.Text,
		Thinking:       block.Thinking,
		Signature:      block.Signature,
		RedactedData:   block.RedactedData,
		ToolUseID:      block.ToolUseID,
		ToolName:       block.ToolName,
		ToolInput:      cloneRawMessage(block.ToolInput),
		ToolResult:     cloneContextContentBlocks(block.ToolResult),
		ToolResultText: block.ToolResultText,
		IsError:        block.IsError,
		ImageSource:    block.ImageSource,
	}
}
