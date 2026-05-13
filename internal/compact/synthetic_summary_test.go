package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func syntheticSummaryLine(uuid, parent string, ts time.Time, blocks []string) string {
	content := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, map[string]any{
			"type": "text",
			"text": block,
		})
	}
	payload := map[string]any{
		"uuid":             uuid,
		"parentUuid":       parent,
		"type":             "user",
		"isCompactSummary": true,
		"timestamp":        ts.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func sampleSyntheticSummaryBlocks() []string {
	return []string{
		"## Context continuity notice\n\nYou are the same agent.\n\nUse MCP first.\n\n### What was dropped\n\n- dropped-one\n- dropped-two\n\n### Summary of dropped content\n\n- prior decision\n- pending task\n\n## Surviving transcript\n\n",
		"**User (2026-04-01 00:01Z):** older-user-turn\n\n",
		"**Assistant (2026-04-01 00:02Z):** older-assistant-turn\n\n",
		"## Tool activity\n\n- tool-item-1\n\n- tool-item-2\n\n",
		"## Continue from here.\n",
	}
}

func sampleLegacySyntheticSummaryBlocks() []string {
	return []string{
		"## Continued from prior session (transcript below)\n\n",
		"**User (2026-04-01 00:01Z):** older-user-turn\n\n",
		"**Assistant (2026-04-01 00:02Z):** older-assistant-turn\n\n",
		"## Continue from here.\n",
	}
}

func sampleLargeSyntheticSummaryBlocks() []string {
	return []string{
		"## Context continuity notice\n\nsame agent\n\n## Surviving transcript\n\n",
		"**User:** large-part-a " + strings.Repeat("a ", 2500) + "\n\n" +
			"large-part-b " + strings.Repeat("b ", 2500) + "\n\n" +
			"large-part-c " + strings.Repeat("c ", 2500) + "\n\n",
		"## Continue from here.\n",
	}
}

func TestParseSyntheticSummary_AllSections(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	line := syntheticSummaryLine("s1", "b1", t0, sampleSyntheticSummaryBlocks())
	slice, err := LoadSlice(writeTranscript(t, []string{boundaryLine("b1", "", t0.Add(-time.Second)), line}))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	summary, ok := parseSyntheticSummary(slice.PostBoundary[0])
	if !ok {
		t.Fatalf("parseSyntheticSummary = false")
	}
	if got, want := summary.Continuity, "You are the same agent.\n\nUse MCP first."; got != want {
		t.Fatalf("Continuity = %q, want %q", got, want)
	}
	if got := len(summary.TranscriptTurns); got != 2 {
		t.Fatalf("len(TranscriptTurns) = %d", got)
	}
	if got := len(summary.ToolItems); got != 2 {
		t.Fatalf("len(ToolItems) = %d", got)
	}
	if !summary.HasContinue {
		t.Fatalf("HasContinue = false, want true")
	}
}

func TestParseSyntheticSummary_LegacyHeader(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	line := syntheticSummaryLine("s1", "b1", t0, sampleLegacySyntheticSummaryBlocks())
	slice, err := LoadSlice(writeTranscript(t, []string{boundaryLine("b1", "", t0.Add(-time.Second)), line}))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	summary, ok := parseSyntheticSummary(slice.PostBoundary[0])
	if !ok {
		t.Fatalf("parseSyntheticSummary = false")
	}
	if got, want := len(summary.TranscriptTurns), 2; got != want {
		t.Fatalf("len(TranscriptTurns) = %d, want %d", got, want)
	}
	if got := len(summary.ToolItems); got != 0 {
		t.Fatalf("len(ToolItems) = %d, want 0", got)
	}
	if !summary.HasContinue {
		t.Fatalf("HasContinue = false, want true")
	}
	if !strings.Contains(summary.Continuity, "prior context was compacted earlier") {
		t.Fatalf("Continuity = %q", summary.Continuity)
	}
}

func TestParseSyntheticSummary_OptionalSections(t *testing.T) {
	cases := []struct {
		name           string
		blocks         []string
		wantWhat       bool
		wantSummary    bool
		wantTools      int
		wantTranscript int
	}{
		{
			name: "no what dropped",
			blocks: []string{
				"## Context continuity notice\n\nsame agent\n\n### Summary of dropped content\n\n- summary only\n\n## Surviving transcript\n\n",
				"**User:** turn-a\n\n",
				"## Continue from here.\n",
			},
			wantSummary:    true,
			wantTranscript: 1,
		},
		{
			name: "no dropped summary",
			blocks: []string{
				"## Context continuity notice\n\nsame agent\n\n### What was dropped\n\n- item\n\n## Surviving transcript\n\n",
				"**Assistant:** turn-b\n\n",
			},
			wantWhat:       true,
			wantTranscript: 1,
		},
		{
			name: "no tool activity",
			blocks: []string{
				"## Context continuity notice\n\nsame agent\n\n## Surviving transcript\n\n",
				"**User:** turn-a\n\n",
				"**Assistant:** turn-b\n\n",
			},
			wantTranscript: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := parseSyntheticSummaryBlocks(tc.blocks)
			if err != nil {
				t.Fatalf("parseSyntheticSummaryBlocks: %v", err)
			}
			if got := summary.WhatDropped != ""; got != tc.wantWhat {
				t.Fatalf("has WhatDropped = %v, want %v", got, tc.wantWhat)
			}
			if got := summary.DroppedSummary != ""; got != tc.wantSummary {
				t.Fatalf("has DroppedSummary = %v, want %v", got, tc.wantSummary)
			}
			if got := len(summary.ToolItems); got != tc.wantTools {
				t.Fatalf("len(ToolItems) = %d, want %d", got, tc.wantTools)
			}
			if got := len(summary.TranscriptTurns); got != tc.wantTranscript {
				t.Fatalf("len(TranscriptTurns) = %d, want %d", got, tc.wantTranscript)
			}
		})
	}
}

func TestParseSyntheticSummary_AllowsNestedPriorSummaryBlocks(t *testing.T) {
	blocks := []string{
		"## Context continuity notice\n\nouter summary\n\n## Surviving transcript\n\n",
	}
	blocks = append(blocks, sampleSyntheticSummaryBlocks()...)
	blocks = append(blocks, "## Continue from here.\n")

	summary, err := parseSyntheticSummaryBlocks(blocks)
	if err != nil {
		t.Fatalf("parseSyntheticSummaryBlocks: %v", err)
	}
	if got := len(summary.TranscriptTurns); got < 4 {
		t.Fatalf("len(TranscriptTurns) = %d, want nested blocks retained", got)
	}
	if !summary.HasContinue {
		t.Fatalf("outer continue block should still be recognized")
	}
	order := summary.DropOrder()
	if len(order) == 0 {
		t.Fatalf("DropOrder empty")
	}
	for _, key := range order {
		if strings.HasPrefix(key, "surviving_turn:") {
			return
		}
	}
	t.Fatalf("DropOrder missing surviving nested chunks: %#v", order)
}

func TestParseSyntheticSummary_MalformedOrderFallsBack(t *testing.T) {
	blocks := []string{
		"## Context continuity notice\n\nsame agent\n\n## Surviving transcript\n\n",
		"## Tool activity\n\n- tool-item-1\n\n",
		"**User:** late-turn\n\n",
	}
	if _, err := parseSyntheticSummaryBlocks(blocks); err == nil {
		t.Fatalf("parseSyntheticSummaryBlocks should fail on unexpected ordering")
	}
}

func TestSyntheticSummary_RenderDropsTranscriptTurnByTurnAndHidesEmptyHeaders(t *testing.T) {
	summary, err := parseSyntheticSummaryBlocks(sampleSyntheticSummaryBlocks())
	if err != nil {
		t.Fatalf("parseSyntheticSummaryBlocks: %v", err)
	}
	out := summary.Render(map[string]bool{
		summaryChunkContinue: true,
		"tool_item:0":        true,
		"surviving_turn:0":   true,
		summaryChunkSummary:  true,
		summaryChunkWhat:     true,
	})
	text := joinOutputText(out)
	if strings.Contains(text, "older-user-turn") {
		t.Fatalf("dropped transcript turn still present")
	}
	if !strings.Contains(text, "older-assistant-turn") {
		t.Fatalf("newer transcript turn missing")
	}
	if strings.Contains(text, "tool-item-1") {
		t.Fatalf("dropped tool item still present")
	}
	if !strings.Contains(text, "tool-item-2") {
		t.Fatalf("remaining tool item missing")
	}
	if strings.Contains(text, "### Summary of dropped content") || strings.Contains(text, "### What was dropped") {
		t.Fatalf("empty dropped sections should not render")
	}
	if strings.Contains(text, "## Continue from here.") {
		t.Fatalf("continue block should be removed")
	}
}

type syntheticSummaryCounter struct{}

func (syntheticSummaryCounter) CountSyntheticUser(_ context.Context, content []OutputBlock) (int, error) {
	text := joinOutputText(content)
	score := 0
	for _, marker := range []struct {
		needle string
		value  int
	}{
		{"recent-user-turn", 80},
		{"recent-assistant-turn", 80},
		{"## Continue from here.", 10},
		{"tool-item-1", 20},
		{"tool-item-2", 20},
		{"older-user-turn", 30},
		{"older-assistant-turn", 30},
		{"### Summary of dropped content", 40},
		{"### What was dropped", 40},
		{"## Context continuity notice", 50},
	} {
		if strings.Contains(text, marker.needle) {
			score += marker.value
		}
	}
	return score, nil
}

type largeSyntheticSummaryCounter struct{}

func (largeSyntheticSummaryCounter) CountSyntheticUser(_ context.Context, content []OutputBlock) (int, error) {
	text := joinOutputText(content)
	score := 0
	for _, marker := range []struct {
		needle string
		value  int
	}{
		{"recent-user-turn", 100},
		{"recent-assistant-turn", 100},
		{"## Continue from here.", 10},
		{"large-part-a", 100},
		{"large-part-b", 100},
		{"large-part-c", 100},
	} {
		if strings.Contains(text, marker.needle) {
			score += marker.value
		}
	}
	return score, nil
}

func joinOutputText(out []OutputBlock) string {
	var sb strings.Builder
	for _, b := range out {
		if b.Image == nil {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func TestRunPlan_RecompactsPriorSyntheticSummaryByChunk(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		syntheticSummaryLine("s1", "b1", t0.Add(time.Second), sampleSyntheticSummaryBlocks()),
		userText("u1", "s1", "recent-user-turn", t0.Add(2*time.Second)),
		assistantBlocks("a1", "u1", t0.Add(3*time.Second), []map[string]any{
			{"type": "text", "text": "recent-assistant-turn"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	res, err := RunPlan(context.Background(), PlanInput{
		Slice:     slice,
		Strippers: Strippers{Chat: true},
		Target:    350,
		Counter:   syntheticSummaryCounter{},
	})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if res.Options.DroppedChatEntries[0] {
		t.Fatalf("prior synthetic summary should not be dropped as a whole entry")
	}
	dropped := res.Options.DroppedSummaryChunks[0]
	if !dropped[summaryChunkContinue] {
		t.Fatalf("continue chunk should be dropped first")
	}
	if !dropped["tool_item:0"] || !dropped["tool_item:1"] {
		t.Fatalf("tool activity items should be dropped before transcript turns: %#v", dropped)
	}
	if dropped["surviving_turn:0"] {
		t.Fatalf("target should be hit before dropping surviving transcript turns")
	}

	text := joinOutputText(res.BoundaryTail)
	if strings.Contains(text, "tool-item-1") || strings.Contains(text, "tool-item-2") {
		t.Fatalf("dropped chunks still rendered: %q", text)
	}
	if !strings.Contains(text, "older-user-turn") {
		t.Fatalf("surviving transcript should still include old turns")
	}
}

func TestRunPlan_RecompactsLegacySyntheticSummaryByChunk(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		syntheticSummaryLine("s1", "b1", t0.Add(time.Second), sampleLegacySyntheticSummaryBlocks()),
		userText("u1", "s1", "recent-user-turn", t0.Add(2*time.Second)),
		assistantBlocks("a1", "u1", t0.Add(3*time.Second), []map[string]any{
			{"type": "text", "text": "recent-assistant-turn"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	res, err := RunPlan(context.Background(), PlanInput{
		Slice:     slice,
		Strippers: Strippers{Chat: true},
		Target:    275,
		Counter:   syntheticSummaryCounter{},
	})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if res.Options.DroppedChatEntries[0] {
		t.Fatalf("legacy synthetic summary should not be dropped as a whole entry")
	}
	dropped := res.Options.DroppedSummaryChunks[0]
	if !dropped[summaryChunkContinue] {
		t.Fatalf("continue chunk should be dropped first")
	}
	if dropped["surviving_turn:0"] {
		t.Fatalf("target should be hit before dropping legacy transcript turns")
	}
	text := joinOutputText(res.BoundaryTail)
	if strings.Contains(text, "## Continued from prior session (transcript below)") {
		t.Fatalf("legacy header should be canonicalized: %q", text)
	}
	if !strings.Contains(text, "## Context continuity notice") {
		t.Fatalf("canonical continuity header missing: %q", text)
	}
}

func TestRunPlan_RecompactsLargeSyntheticTurnByPart(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		syntheticSummaryLine("s1", "b1", t0.Add(time.Second), sampleLargeSyntheticSummaryBlocks()),
		userText("u1", "s1", "recent-user-turn", t0.Add(2*time.Second)),
		assistantBlocks("a1", "u1", t0.Add(3*time.Second), []map[string]any{
			{"type": "text", "text": "recent-assistant-turn"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	res, err := RunPlan(context.Background(), PlanInput{
		Slice:     slice,
		Strippers: Strippers{Chat: true},
		Target:    410,
		Counter:   largeSyntheticSummaryCounter{},
	})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if !res.HitTarget {
		t.Fatalf("HitTarget = false, final=%d", res.FinalTail)
	}
	if res.Options.DroppedChatEntries[0] {
		t.Fatalf("large prior synthetic summary should not be dropped as a whole entry")
	}
	dropped := res.Options.DroppedSummaryChunks[0]
	if !dropped[summaryChunkContinue] {
		t.Fatalf("continue chunk should be dropped first")
	}
	if !dropped["surviving_turn:0:part:0"] {
		t.Fatalf("large surviving turn should expose droppable part chunks: %#v", dropped)
	}
	if dropped["surviving_turn:0"] {
		t.Fatalf("large surviving turn should not need whole-turn drop")
	}

	text := joinOutputText(res.BoundaryTail)
	if strings.Contains(text, "large-part-a") {
		t.Fatalf("dropped part still rendered")
	}
	if !strings.Contains(text, "**User:**") || !strings.Contains(text, "large-part-b") || !strings.Contains(text, "large-part-c") {
		t.Fatalf("remaining parts should render with speaker label: %q", text[:minInt(len(text), 200)])
	}
}

func TestRunPlan_MalformedSyntheticSummaryFallsBackToWholeTurnDrop(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		syntheticSummaryLine("s1", "b1", t0.Add(time.Second), []string{
			"## Context continuity notice\n\nsame agent\n\n## Surviving transcript\n\n",
			"## Tool activity\n\n- tool-item-1\n\n",
			"**User:** invalid-order-turn\n\n",
		}),
		userText("u1", "s1", "recent-user-turn", t0.Add(2*time.Second)),
		assistantBlocks("a1", "u1", t0.Add(3*time.Second), []map[string]any{
			{"type": "text", "text": "recent-assistant-turn"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	res, err := RunPlan(context.Background(), PlanInput{
		Slice:     slice,
		Strippers: Strippers{Chat: true},
		Target:    150,
		Counter:   syntheticSummaryCounter{},
	})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if !res.Options.DroppedChatEntries[0] {
		t.Fatalf("malformed summary should fall back to whole-entry drop")
	}
	if len(res.Options.DroppedSummaryChunks[0]) != 0 {
		t.Fatalf("malformed summary should not produce virtual chunk drops: %#v", res.Options.DroppedSummaryChunks[0])
	}
}

func TestChatDropOrder_RegularChatRemainsWholeTurn(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		userText("u1", "b1", "older regular user", t0.Add(time.Second)),
		assistantBlocks("a1", "u1", t0.Add(2*time.Second), []map[string]any{
			{"type": "text", "text": "older assistant"},
		}),
		userText("u2", "a1", "recent user", t0.Add(3*time.Second)),
		assistantBlocks("a2", "u2", t0.Add(4*time.Second), []map[string]any{
			{"type": "text", "text": "recent assistant"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	order := chatDropOrder(slice)
	if len(order) != 2 {
		t.Fatalf("len(chatDropOrder) = %d, want 2", len(order))
	}
	for _, step := range order {
		if step.ChunkKey != "" {
			t.Fatalf("regular chat should drop as whole entries, got %+v", step)
		}
	}
}

func TestSyntheticSummary_DroppedTextIncludesVirtualChunks(t *testing.T) {
	summary, err := parseSyntheticSummaryBlocks(sampleSyntheticSummaryBlocks())
	if err != nil {
		t.Fatalf("parseSyntheticSummaryBlocks: %v", err)
	}
	text := summary.DroppedText(map[string]bool{
		"surviving_turn:0": true,
		"tool_item:1":      true,
	})
	for _, needle := range []string{"older-user-turn", "tool-item-2"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("DroppedText missing %q in %q", needle, text)
		}
	}
}

func Example_syntheticSummaryCounter() {
	n, _ := syntheticSummaryCounter{}.CountSyntheticUser(context.Background(), []OutputBlock{{Text: "## Continue from here.\n"}})
	fmt.Println(n)
	// Output: 10
}
