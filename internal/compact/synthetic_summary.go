package compact

import (
	"fmt"
	"strings"
)

const (
	summaryChunkContinue   = "continue_from_here"
	summaryChunkSummary    = "summary_of_dropped_content"
	summaryChunkWhat       = "what_was_dropped"
	summaryChunkContinuity = "context_continuity_notice"

	syntheticTurnPartPrefix      = ":part:"
	syntheticTurnPartApproxBytes = 4096
)

type syntheticSummary struct {
	Continuity      string
	WhatDropped     string
	DroppedSummary  string
	TranscriptTurns []string
	ToolItems       []string
	HasContinue     bool
}

func parseSyntheticSummary(e Entry) (*syntheticSummary, bool) {
	if !e.IsSummary || e.Type != "user" {
		return nil, false
	}
	if e.TextOnly != "" || len(e.Content) == 0 {
		compactLog.Logger().Debug("compact.synthetic_summary.parse_skipped",
			"component", "compact",
			"subcomponent", "synthetic_summary",
			"reason", "missing_content_array",
			"entry_line", e.LineIndex,
		)
		return nil, false
	}

	blocks := make([]string, 0, len(e.Content))
	for _, b := range e.Content {
		if b.Type != "text" {
			compactLog.Logger().Debug("compact.synthetic_summary.parse_skipped",
				"component", "compact",
				"subcomponent", "synthetic_summary",
				"reason", "non_text_block",
				"entry_line", e.LineIndex,
				"block_type", b.Type,
			)
			return nil, false
		}
		blocks = append(blocks, b.Text)
	}

	summary, err := parseSyntheticSummaryBlocks(blocks)
	if err != nil {
		compactLog.Logger().Debug("compact.synthetic_summary.parse_failed",
			"component", "compact",
			"subcomponent", "synthetic_summary",
			"entry_line", e.LineIndex,
			"err", err,
		)
		return nil, false
	}

	compactLog.Logger().Debug("compact.synthetic_summary.parsed",
		"component", "compact",
		"subcomponent", "synthetic_summary",
		"entry_line", e.LineIndex,
		"transcript_turns", len(summary.TranscriptTurns),
		"tool_items", len(summary.ToolItems),
		"has_summary", summary.DroppedSummary != "",
		"has_what_dropped", summary.WhatDropped != "",
		"has_continue", summary.HasContinue,
	)
	return summary, true
}

func parseSyntheticSummaryBlocks(blocks []string) (*syntheticSummary, error) {
	if len(blocks) == 0 {
		return nil, fmt.Errorf("empty block list")
	}

	header := blocks[0]
	sections, err := parseSyntheticHeader(header)
	if err != nil {
		return nil, err
	}
	summary := &syntheticSummary{
		Continuity:     sections.Continuity,
		WhatDropped:    sections.WhatDropped,
		DroppedSummary: sections.DroppedSummary,
	}

	phase := "transcript"
	nestedSummary := false
	for i := 1; i < len(blocks); i++ {
		block := blocks[i]
		switch {
		case isSyntheticSummaryHeaderBlock(block):
			if phase != "transcript" {
				return nil, fmt.Errorf("nested summary out of order")
			}
			nestedSummary = true
			summary.TranscriptTurns = append(summary.TranscriptTurns, block)
		case strings.HasPrefix(block, "## Tool activity\n\n"):
			if nestedSummary {
				summary.TranscriptTurns = append(summary.TranscriptTurns, block)
				continue
			}
			if phase != "transcript" {
				return nil, fmt.Errorf("tool activity out of order")
			}
			if len(summary.ToolItems) > 0 {
				return nil, fmt.Errorf("duplicate tool activity section")
			}
			items, err := parseToolActivityItems(block)
			if err != nil {
				return nil, err
			}
			summary.ToolItems = items
			phase = "tools"
		case strings.TrimSpace(block) == "## Continue from here.":
			if i != len(blocks)-1 {
				if nestedSummary {
					summary.TranscriptTurns = append(summary.TranscriptTurns, block)
					continue
				}
				return nil, fmt.Errorf("continue block must be final")
			}
			summary.HasContinue = true
			phase = "continue"
		case isRenderedTranscriptTurn(block):
			if phase != "transcript" {
				return nil, fmt.Errorf("transcript turn out of order")
			}
			summary.TranscriptTurns = append(summary.TranscriptTurns, block)
		default:
			return nil, fmt.Errorf("unexpected synthetic summary block %q", summarizeBlock(block))
		}
	}

	return summary, nil
}

func isSyntheticSummaryHeaderBlock(block string) bool {
	return strings.HasPrefix(block, "## Context continuity notice\n\n") ||
		block == "## Continued from prior session (transcript below)\n\n"
}

type parsedSyntheticHeader struct {
	Continuity     string
	WhatDropped    string
	DroppedSummary string
}

func parseSyntheticHeader(block string) (*parsedSyntheticHeader, error) {
	const (
		continuityHeader = "## Context continuity notice\n\n"
		legacyHeader     = "## Continued from prior session (transcript below)\n\n"
		whatHeader       = "### What was dropped\n\n"
		summaryHeader    = "### Summary of dropped content\n\n"
		transcriptHeader = "## Surviving transcript\n\n"
	)
	if block == legacyHeader {
		return &parsedSyntheticHeader{
			Continuity: "Your prior context was compacted earlier in this same session. The surviving transcript continues below.",
		}, nil
	}
	if !strings.HasPrefix(block, continuityHeader) {
		return nil, fmt.Errorf("missing continuity header")
	}

	body := strings.TrimPrefix(block, continuityHeader)
	parts := []string{}
	if body != "" {
		parts = strings.Split(body, "\n\n")
	}
	parts = trimTrailingEmptyParts(parts)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty continuity body")
	}

	out := &parsedSyntheticHeader{}
	idx := 0
	var continuity []string
	for idx < len(parts) {
		part := parts[idx]
		if part == whatHeader[:len(whatHeader)-2] || part == summaryHeader[:len(summaryHeader)-2] || part == transcriptHeader[:len(transcriptHeader)-2] {
			break
		}
		continuity = append(continuity, part)
		idx++
	}
	out.Continuity = strings.TrimSpace(strings.Join(continuity, "\n\n"))
	if out.Continuity == "" {
		return nil, fmt.Errorf("missing continuity body")
	}

	if idx < len(parts) && parts[idx] == "### What was dropped" {
		idx++
		if idx >= len(parts) {
			return nil, fmt.Errorf("missing what was dropped body")
		}
		out.WhatDropped = strings.TrimSpace(parts[idx])
		if out.WhatDropped == "" {
			return nil, fmt.Errorf("empty what was dropped body")
		}
		idx++
	}

	if idx < len(parts) && parts[idx] == "### Summary of dropped content" {
		idx++
		if idx >= len(parts) {
			return nil, fmt.Errorf("missing dropped summary body")
		}
		out.DroppedSummary = strings.TrimSpace(parts[idx])
		if out.DroppedSummary == "" {
			return nil, fmt.Errorf("empty dropped summary body")
		}
		idx++
	}

	if idx < len(parts) {
		if parts[idx] != "## Surviving transcript" {
			return nil, fmt.Errorf("unexpected header tail %q", summarizeBlock(parts[idx]))
		}
		idx++
	}
	if idx != len(parts) {
		return nil, fmt.Errorf("unexpected extra header content")
	}

	return out, nil
}

func trimTrailingEmptyParts(parts []string) []string {
	for len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func isRenderedTranscriptTurn(block string) bool {
	return strings.HasPrefix(block, "**User") || strings.HasPrefix(block, "**Assistant")
}

func parseToolActivityItems(block string) ([]string, error) {
	body := strings.TrimPrefix(block, "## Tool activity\n\n")
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("empty tool activity body")
	}
	lines := strings.Split(body, "\n")
	var (
		items   []string
		current []string
		inFence bool
	)
	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		item := strings.TrimSpace(strings.Join(current, "\n"))
		if !strings.HasPrefix(item, "- ") {
			return fmt.Errorf("tool activity item missing bullet")
		}
		items = append(items, item)
		current = nil
		return nil
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "- ") && len(current) > 0 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		current = append(current, line)
	}
	if inFence {
		return nil, fmt.Errorf("unterminated tool activity fence")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no tool activity items")
	}
	return items, nil
}

func (s *syntheticSummary) DropOrder() []string {
	order := make([]string, 0, 1+len(s.ToolItems)+len(s.TranscriptTurns)+3)
	if s.HasContinue {
		order = append(order, summaryChunkContinue)
	}
	for i := range s.ToolItems {
		order = append(order, fmt.Sprintf("tool_item:%d", i))
	}
	for i, turn := range s.TranscriptTurns {
		_, parts := splitSyntheticTranscriptTurn(turn)
		for part := range parts {
			order = append(order, fmt.Sprintf("surviving_turn:%d%s%d", i, syntheticTurnPartPrefix, part))
		}
		order = append(order, fmt.Sprintf("surviving_turn:%d", i))
	}
	if s.DroppedSummary != "" {
		order = append(order, summaryChunkSummary)
	}
	if s.WhatDropped != "" {
		order = append(order, summaryChunkWhat)
	}
	order = append(order, summaryChunkContinuity)
	return order
}

func (s *syntheticSummary) DroppedText(dropped map[string]bool) string {
	if len(dropped) == 0 {
		return ""
	}
	var sections []string
	if dropped[summaryChunkContinuity] {
		sections = append(sections, "### Context continuity notice\n\n"+strings.TrimSpace(s.Continuity))
	}
	if dropped[summaryChunkWhat] && s.WhatDropped != "" {
		sections = append(sections, "### What was dropped\n\n"+strings.TrimSpace(s.WhatDropped))
	}
	if dropped[summaryChunkSummary] && s.DroppedSummary != "" {
		sections = append(sections, "### Summary of dropped content\n\n"+strings.TrimSpace(s.DroppedSummary))
	}
	turns := make([]string, 0, len(s.TranscriptTurns))
	for i, turn := range s.TranscriptTurns {
		if dropped[fmt.Sprintf("surviving_turn:%d", i)] {
			turns = append(turns, strings.TrimSpace(turn))
			continue
		}
		_, parts := splitSyntheticTranscriptTurn(turn)
		for partIdx, part := range parts {
			if dropped[fmt.Sprintf("surviving_turn:%d%s%d", i, syntheticTurnPartPrefix, partIdx)] {
				turns = append(turns, strings.TrimSpace(part))
			}
		}
	}
	if len(turns) > 0 {
		sections = append(sections, "### Surviving transcript\n\n"+strings.Join(turns, "\n\n"))
	}
	items := make([]string, 0, len(s.ToolItems))
	for i, item := range s.ToolItems {
		if dropped[fmt.Sprintf("tool_item:%d", i)] {
			items = append(items, strings.TrimSpace(item))
		}
	}
	if len(items) > 0 {
		sections = append(sections, "### Tool activity\n\n"+strings.Join(items, "\n\n"))
	}
	if dropped[summaryChunkContinue] && s.HasContinue {
		sections = append(sections, "### Continue from here.\n\n## Continue from here.")
	}
	return strings.Join(sections, "\n\n")
}

func summarizeBlock(block string) string {
	block = strings.TrimSpace(block)
	if len(block) > 64 {
		return block[:61] + "..."
	}
	return block
}

func splitSyntheticTranscriptTurn(turn string) (string, []string) {
	if len(turn) <= syntheticTurnPartApproxBytes {
		return "", nil
	}
	prefix, body := splitSyntheticTranscriptTurnPrefix(turn)
	paragraphs := strings.SplitAfter(body, "\n\n")
	if len(paragraphs) > 1 {
		parts := coalesceSyntheticTurnParts(paragraphs)
		if len(parts) > 1 {
			return prefix, parts
		}
	}
	parts := splitSyntheticTurnByBytes(body)
	if len(parts) <= 1 {
		return "", nil
	}
	return prefix, parts
}

func splitSyntheticTranscriptTurnPrefix(turn string) (string, string) {
	idx := strings.Index(turn, ":**")
	if idx < 0 {
		return "", turn
	}
	bodyStart := idx + len(":**")
	for bodyStart < len(turn) && turn[bodyStart] == ' ' {
		bodyStart++
	}
	return turn[:bodyStart], turn[bodyStart:]
}

func coalesceSyntheticTurnParts(paragraphs []string) []string {
	var parts []string
	var current strings.Builder
	for _, paragraph := range paragraphs {
		if current.Len() > 0 && current.Len()+len(paragraph) > syntheticTurnPartApproxBytes {
			parts = append(parts, current.String())
			current.Reset()
		}
		current.WriteString(paragraph)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func splitSyntheticTurnByBytes(turn string) []string {
	var parts []string
	for len(turn) > syntheticTurnPartApproxBytes {
		cut := syntheticTurnPartApproxBytes
		if next := strings.LastIndexAny(turn[:cut], " \n\t"); next > 0 {
			cut = next + 1
		}
		parts = append(parts, turn[:cut])
		turn = turn[cut:]
	}
	if turn != "" {
		parts = append(parts, turn)
	}
	return parts
}
