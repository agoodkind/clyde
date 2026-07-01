package conversation

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

func TestChunkTextSplitsOnLineBoundaries(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("line\n", 200)
	chunks := chunkText(body, 100)
	if len(chunks) < 2 {
		t.Fatalf("chunks len = %d, want >= 2", len(chunks))
	}
	for index, chunk := range chunks {
		if runeLen(chunk) > 100 {
			t.Fatalf("chunk %d has %d runes, want <= 100", index, runeLen(chunk))
		}
	}
	if joinedRuneLen(chunks) == 0 {
		t.Fatalf("chunks dropped all content")
	}
}

func TestChunkTextShortBodyStaysWhole(t *testing.T) {
	t.Parallel()
	chunks := chunkText("short body", 100)
	if len(chunks) != 1 || chunks[0] != "short body" {
		t.Fatalf("chunks = %#v, want one whole chunk", chunks)
	}
}

func TestChunkTextHardSplitsLongSingleLine(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("x", 250)
	chunks := chunkText(line, 100)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3", len(chunks))
	}
	if chunks[0] != strings.Repeat("x", 100) || chunks[2] != strings.Repeat("x", 50) {
		t.Fatalf("hard-split chunks = %#v", chunks)
	}
}

func TestPageReorientItemsRespectsBudgetAndAdvances(t *testing.T) {
	t.Parallel()
	items := []ReorientItem{
		testReorientItem("a", 40),
		testReorientItem("b", 40),
		testReorientItem("c", 40),
	}
	page, next := pageReorientItems(items, 0, 80)
	if len(page) != 1 {
		t.Fatalf("page len = %d, want 1 (budget fits one item plus overhead)", len(page))
	}
	if next != 1 {
		t.Fatalf("next offset = %d, want 1", next)
	}
	page, next = pageReorientItems(items, next, 80)
	if len(page) != 1 || next != 2 {
		t.Fatalf("second page len = %d next = %d, want 1 and 2", len(page), next)
	}
}

func TestPageReorientItemsAlwaysReturnsAtLeastOne(t *testing.T) {
	t.Parallel()
	items := []ReorientItem{testReorientItem("big", 10_000)}
	page, next := pageReorientItems(items, 0, 100)
	if len(page) != 1 {
		t.Fatalf("oversized item: page len = %d, want 1", len(page))
	}
	if next != 1 {
		t.Fatalf("next offset = %d, want 1", next)
	}
}

func TestPageReorientItemsOffsetPastEnd(t *testing.T) {
	t.Parallel()
	items := []ReorientItem{testReorientItem("a", 10)}
	page, next := pageReorientItems(items, 5, 100)
	if page != nil || next != len(items) {
		t.Fatalf("past-end page = %#v next = %d, want nil and %d", page, next, len(items))
	}
}

func TestReorientCursorRoundTrip(t *testing.T) {
	t.Parallel()
	for _, offset := range []int{0, 1, 42, 9999} {
		encoded := encodeReorientCursor(offset)
		decoded, err := unmarshalReorientCursor(encoded)
		if err != nil {
			t.Fatalf("unmarshal(%q) err = %v", encoded, err)
		}
		if decoded != offset {
			t.Fatalf("round trip offset = %d, want %d", decoded, offset)
		}
	}
}

func TestReorientCursorEmptyIsZero(t *testing.T) {
	t.Parallel()
	decoded, err := unmarshalReorientCursor("")
	if err != nil {
		t.Fatalf("empty cursor err = %v", err)
	}
	if decoded != 0 {
		t.Fatalf("empty cursor offset = %d, want 0", decoded)
	}
}

func TestReorientCursorRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := unmarshalReorientCursor("!!!not-base64!!!"); err == nil {
		t.Fatalf("garbage cursor accepted, want error")
	}
}

func TestAppendChunkedItemsSkipsEmpty(t *testing.T) {
	t.Parallel()
	items := appendChunkedItems(nil, ReorientItemKindTail, "Tail", "claude:1", 3, "   ")
	if items != nil {
		t.Fatalf("empty body added items: %#v", items)
	}
}

func TestAppendChunkedItemsNumbersMultipleParts(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("line\n", 2000)
	items := appendChunkedItems(nil, ReorientItemKindTail, "Tail", "claude:1", 3, body)
	if len(items) < 2 {
		t.Fatalf("items len = %d, want >= 2", len(items))
	}
	if !strings.Contains(items[0].Title, "part 1/") {
		t.Fatalf("first item title = %q, want a part marker", items[0].Title)
	}
	for _, item := range items {
		if item.Kind != ReorientItemKindTail || item.ConversationID != "claude:1" || item.MessageIndex != 3 {
			t.Fatalf("item metadata not propagated: %#v", item)
		}
	}
}

func TestAppendInstructionChunkedItemsBoundsOverlongInstruction(t *testing.T) {
	t.Parallel()
	instruction := strings.Repeat("i", maxReorientItemRunes+10)
	body := strings.Repeat("b", 100)
	items := appendInstructionChunkedItems(nil, ReorientItemKindPreCompactWindow, "Remaining", "claude:1", 7, instruction, body)
	if len(items) < 2 {
		t.Fatalf("items len = %d, want chunked output", len(items))
	}
	for index, item := range items {
		if runeLen(item.Body) > maxReorientItemRunes {
			t.Fatalf("item %d body runes = %d, want <= %d", index, runeLen(item.Body), maxReorientItemRunes)
		}
		if item.Kind != ReorientItemKindPreCompactWindow || item.ConversationID != "claude:1" || item.MessageIndex != 7 {
			t.Fatalf("item metadata not propagated: %#v", item)
		}
	}
	if !strings.Contains(joinedReorientItemBodies(items, ReorientItemKindPreCompactWindow), body) {
		t.Fatalf("chunked items dropped body text")
	}
}

func TestRenderCheckpointSummaryExtractsSummaryText(t *testing.T) {
	t.Parallel()
	checkpoint := CompactionCheckpoint{
		BoundaryIndex: 0,
		SummaryIndex:  1,
		ContextItems: []transcript.CompactedContextItem{
			{
				Kind: transcript.CompactedContextItemKindMessage,
				Message: &transcript.CompactedMessageItem{
					MessageClass: transcript.CompactedMessageClassSummary,
					Content: []transcript.CompactedMessageContentItem{
						{Type: "text", Text: "the work so far"},
					},
				},
			},
			{
				Kind: transcript.CompactedContextItemKindMessage,
				Message: &transcript.CompactedMessageItem{
					MessageClass: transcript.CompactedMessageClassOrdinary,
					Content: []transcript.CompactedMessageContentItem{
						{Type: "text", Text: "ignored ordinary text"},
					},
				},
			},
		},
	}
	got := renderCheckpointSummary(checkpoint)
	if got != "the work so far" {
		t.Fatalf("summary = %q, want %q", got, "the work so far")
	}
}

func TestBuildReorientHeaderReportsParentAndCheckpoint(t *testing.T) {
	t.Parallel()
	current := Record{
		ID:       "claude:child",
		Provider: providerid.ProviderClaude,
		Title:    "Child",
		Lineage: &Lineage{
			Kind:           ConversationLineageKindFork,
			ParentProvider: providerid.ProviderClaude,
			ParentNativeID: "parent",
		},
	}
	parent := &ReorientConversationRef{ID: "claude:parent", Provider: "claude"}
	checkpoint := &CompactionCheckpoint{BoundaryIndex: 10, SummaryIndex: 8, MessagesSummarized: 4}
	header := buildReorientHeader(current, parent, []string{"watch out"}, 2, checkpoint)
	if header.Kind != ReorientItemKindHeader {
		t.Fatalf("header kind = %q", header.Kind)
	}
	for _, want := range []string{"claude:child", "claude:parent", "[fork]", "boundary=10", "warning: watch out"} {
		if !strings.Contains(header.Body, want) {
			t.Fatalf("header body missing %q:\n%s", want, header.Body)
		}
	}
}

func TestBuildReorientHeaderNoParentNoCheckpoint(t *testing.T) {
	t.Parallel()
	current := Record{ID: "codex:solo", Provider: providerid.ProviderCodex}
	header := buildReorientHeader(current, nil, nil, 0, nil)
	for _, want := range []string{"parent:  none", "checkpoint: none"} {
		if !strings.Contains(header.Body, want) {
			t.Fatalf("header body missing %q:\n%s", want, header.Body)
		}
	}
}

func TestReorientPlacesPreCompactWindowBeforeSummary(t *testing.T) {
	t.Parallel()
	fixture := newReorientFixture(t)
	report, err := fixture.index.Reorient(context.Background(), ReorientOptions{
		ConversationID: fixture.child.ID,
		Before:         2,
		After:          2,
		Limit:          1,
	})
	if err != nil {
		t.Fatalf("Reorient err = %v", err)
	}
	if len(report.Items) < 3 {
		t.Fatalf("items len = %d, want at least 3", len(report.Items))
	}
	if report.Items[0].Kind != ReorientItemKindHeader {
		t.Fatalf("first item kind = %q, want header", report.Items[0].Kind)
	}
	preCompactItem := report.Items[1]
	if preCompactItem.Kind != ReorientItemKindPreCompactWindow {
		t.Fatalf("first non-header item kind = %q, want pre_compact_window", preCompactItem.Kind)
	}
	for _, item := range report.Items {
		if item.Kind != ReorientItemKindPreCompactWindow {
			continue
		}
		if !strings.HasPrefix(item.Title, "Remaining") {
			t.Fatalf("pre-compact title = %q, want Remaining", item.Title)
		}
		if strings.Contains(item.Title, "Raw context") || strings.Contains(item.Body, "Raw context") {
			t.Fatalf("pre-compact item retained raw context label: %#v", item)
		}
		if !strings.Contains(item.Body, "LLM instruction: please exhaust the remaining context before reasoning.") {
			t.Fatalf("pre-compact item missing LLM instruction:\n%s", item.Body)
		}
		if !strings.Contains(item.Body, "Do not skip or refuse because this fills the context window") {
			t.Fatalf("pre-compact item missing refusal mitigation:\n%s", item.Body)
		}
	}
	preCompactBody := joinedReorientItemBodies(report.Items, ReorientItemKindPreCompactWindow)
	if !strings.Contains(preCompactBody, "segment beginning detail") {
		t.Fatalf("pre-compact body missing segment start detail:\n%s", preCompactBody)
	}
	if !strings.Contains(preCompactBody, "I rather idiot proof it") {
		t.Fatalf("pre-compact body missing raw detail:\n%s", preCompactBody)
	}
	for _, unwanted := range []string{
		"ancient pre-previous checkpoint should not appear",
		"previous compact summary should not appear",
		"compact summary should stay supplemental",
		"post compact tail",
	} {
		if strings.Contains(preCompactBody, unwanted) {
			t.Fatalf("pre-compact body included %q:\n%s", unwanted, preCompactBody)
		}
	}
	if !strings.Contains(preCompactBody, "Deeper: clyde conversation search claude:child --around ") ||
		!strings.Contains(preCompactBody, "--window 2") {
		t.Fatalf("pre-compact body missing deeper search line:\n%s", preCompactBody)
	}

	recoveredItem, ok := findReorientItem(report.Items, ReorientItemKindRecoveredContext)
	if !ok {
		t.Fatalf("missing recovered context item")
	}
	if !strings.Contains(recoveredItem.Body, "compact summary should stay supplemental") {
		t.Fatalf("recovered context missing summary:\n%s", recoveredItem.Body)
	}
}

func TestReorientParentAnchorUsesTimestampFallbackBeforeTail(t *testing.T) {
	t.Parallel()
	fixture := newReorientFixture(t)
	report, err := fixture.index.Reorient(context.Background(), ReorientOptions{
		ConversationID: fixture.child.ID,
		Before:         2,
		After:          2,
		Limit:          1,
	})
	if err != nil {
		t.Fatalf("Reorient err = %v", err)
	}
	parentAnchor, ok := findReorientItem(report.Items, ReorientItemKindParentAnchor)
	if !ok {
		t.Fatalf("missing parent anchor item")
	}
	if parentAnchor.MessageIndex != 1 {
		t.Fatalf("parent anchor index = %d, want 1", parentAnchor.MessageIndex)
	}
	if !strings.Contains(parentAnchor.Title, "timestamp") {
		t.Fatalf("parent anchor title = %q, want timestamp fallback marker", parentAnchor.Title)
	}
	if !strings.Contains(parentAnchor.Body, "parent anchor before child") {
		t.Fatalf("parent anchor body missing pre-fork message:\n%s", parentAnchor.Body)
	}
	if strings.Contains(parentAnchor.Body, "parent post fork tail should not appear") {
		t.Fatalf("parent anchor body included parent tail:\n%s", parentAnchor.Body)
	}
}

func TestReorientSyntheticPreCompactBoundarySkipsCompactSummary(t *testing.T) {
	t.Parallel()
	fixture := newReorientFixture(t)
	report, err := fixture.index.Reorient(context.Background(), ReorientOptions{
		ConversationID:      fixture.child.ID,
		Before:              2,
		After:               2,
		Limit:               1,
		SyntheticPreCompact: true,
	})
	if err != nil {
		t.Fatalf("Reorient err = %v", err)
	}
	preCompactBody := joinedReorientItemBodies(report.Items, ReorientItemKindPreCompactWindow)
	if !strings.Contains(preCompactBody, "post compact tail") || !strings.Contains(preCompactBody, "tail request") {
		t.Fatalf("synthetic pre-compact body missing transcript tail:\n%s", preCompactBody)
	}
	if strings.Contains(preCompactBody, "compact summary should stay supplemental") {
		t.Fatalf("synthetic pre-compact body included prior compact summary:\n%s", preCompactBody)
	}
	if _, ok := findReorientItem(report.Items, ReorientItemKindRecoveredContext); ok {
		t.Fatalf("synthetic pre-compact report included recovered compact summary")
	}
	header, ok := findReorientItem(report.Items, ReorientItemKindHeader)
	if !ok {
		t.Fatalf("missing header item")
	}
	if report.Checkpoint == nil {
		t.Fatalf("synthetic report checkpoint is nil")
	}
	wantBoundary := fmt.Sprintf("boundary=%d", report.Checkpoint.BoundaryIndex)
	if !strings.Contains(header.Body, wantBoundary) {
		t.Fatalf("synthetic header missing %s:\n%s", wantBoundary, header.Body)
	}
}

func testReorientItem(title string, bodyBytes int) ReorientItem {
	return ReorientItem{
		Kind:         ReorientItemKindTail,
		Title:        title,
		Body:         strings.Repeat("x", bodyBytes),
		MessageIndex: -1,
	}
}

func runeLen(text string) int {
	return len([]rune(text))
}

func joinedRuneLen(chunks []string) int {
	total := 0
	for _, chunk := range chunks {
		total += runeLen(chunk)
	}
	return total
}

type reorientFixture struct {
	index *Index
	child Record
}

type staticParser struct {
	provider       providerid.Provider
	messagesByPath map[string][]transcript.Message
}

func (parser staticParser) Provider() providerid.Provider {
	return parser.provider
}

func (parser staticParser) Discover(_ context.Context, _ map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (parser staticParser) ScanRecord(_ string, _ FileStamp) (Record, bool) {
	return Record{}, false
}

func (parser staticParser) Stream(path string, _ LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		for _, message := range parser.messagesByPath[path] {
			if !yield(message, nil) {
				return
			}
		}
	}
}

func newReorientFixture(t *testing.T) reorientFixture {
	t.Helper()
	baseTime := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	parent := Record{
		ID:           "claude:parent",
		Provider:     providerid.ProviderClaude,
		NativeID:     "parent",
		ArtifactPath: "parent.jsonl",
		CreatedAt:    baseTime.Add(-10 * time.Minute),
		UpdatedAt:    baseTime.Add(30 * time.Minute),
	}
	child := Record{
		ID:           "claude:child",
		Provider:     providerid.ProviderClaude,
		NativeID:     "child",
		ArtifactPath: "child.jsonl",
		CreatedAt:    baseTime,
		UpdatedAt:    baseTime.Add(20 * time.Minute),
		Lineage: &Lineage{
			Kind:              ConversationLineageKindFork,
			ParentProvider:    providerid.ProviderClaude,
			ParentNativeID:    "parent",
			ParentMessageUUID: "missing-parent-message",
		},
	}
	registry := NewRegistry()
	registry.Register(staticParser{
		provider: providerid.ProviderClaude,
		messagesByPath: map[string][]transcript.Message{
			parent.ArtifactPath: reorientParentMessages(baseTime),
			child.ArtifactPath:  reorientChildMessages(baseTime),
		},
	})
	index := NewIndex(registry)
	index.records = []Record{child, parent}
	index.prevRecords = recordsByPath(index.records)
	index.loaded = true
	return reorientFixture{index: index, child: child}
}

func reorientParentMessages(baseTime time.Time) []transcript.Message {
	return []transcript.Message{
		{
			UUID:      "parent-0",
			Role:      "user",
			Timestamp: baseTime.Add(-5 * time.Minute),
			Text:      "parent setup",
		},
		{
			UUID:      "parent-1",
			Role:      "assistant",
			Timestamp: baseTime.Add(-1 * time.Minute),
			Text:      "parent anchor before child",
		},
		{
			UUID:      "parent-2",
			Role:      "user",
			Timestamp: baseTime.Add(10 * time.Minute),
			Text:      "parent post fork tail should not appear",
		},
	}
}

func reorientChildMessages(baseTime time.Time) []transcript.Message {
	messages := []transcript.Message{
		{
			UUID:      "child-0",
			Role:      "user",
			Timestamp: baseTime.Add(time.Minute),
			Text:      "ancient pre-previous checkpoint should not appear",
		},
		{
			UUID:      "child-1",
			Role:      "system",
			Timestamp: baseTime.Add(2 * time.Minute),
			Compaction: &transcript.CompactionMetadata{
				Kind:                    transcript.CompactionKindBoundary,
				Trigger:                 transcript.CompactionTriggerManual,
				TailUUID:                "child-0",
				MessagesSummarized:      1,
				ReplacementHistoryCount: 1,
			},
		},
		{
			UUID:      "child-2",
			Role:      "user",
			Timestamp: baseTime.Add(3 * time.Minute),
			Text:      "previous compact summary should not appear",
			Compaction: &transcript.CompactionMetadata{
				Kind:                    transcript.CompactionKindSummary,
				Trigger:                 transcript.CompactionTriggerManual,
				MessagesSummarized:      1,
				ReplacementHistoryCount: 1,
			},
		},
		{
			UUID:      "child-3",
			Role:      "user",
			Timestamp: baseTime.Add(4 * time.Minute),
			Text:      "segment beginning detail",
		},
	}
	for index := range 30 {
		messages = append(messages, transcript.Message{
			UUID:      fmt.Sprintf("child-filler-%02d", index),
			Role:      "assistant",
			Timestamp: baseTime.Add(time.Duration(5+index) * time.Minute),
			Text:      fmt.Sprintf("segment filler %02d", index),
		})
	}
	messages = append(
		messages,
		transcript.Message{
			UUID:      "child-34",
			Role:      "user",
			Timestamp: baseTime.Add(35 * time.Minute),
			Text:      "I rather idiot proof it",
		},
		transcript.Message{
			UUID:      "child-35",
			Role:      "assistant",
			Timestamp: baseTime.Add(36 * time.Minute),
			Text:      "working before compact",
		},
		transcript.Message{
			UUID:      "child-36",
			Role:      "system",
			Timestamp: baseTime.Add(37 * time.Minute),
			Compaction: &transcript.CompactionMetadata{
				Kind:                    transcript.CompactionKindBoundary,
				Trigger:                 transcript.CompactionTriggerManual,
				TailUUID:                "child-35",
				MessagesSummarized:      33,
				ReplacementHistoryCount: 1,
			},
		},
		transcript.Message{
			UUID:       "child-37",
			ParentUUID: "child-36",
			Role:       "user",
			Timestamp:  baseTime.Add(38 * time.Minute),
			Text:       "compact summary should stay supplemental",
			Compaction: &transcript.CompactionMetadata{
				Kind:                    transcript.CompactionKindSummary,
				Trigger:                 transcript.CompactionTriggerManual,
				MessagesSummarized:      33,
				ReplacementHistoryCount: 1,
				ContextItems: []transcript.CompactedContextItem{
					{
						Kind: transcript.CompactedContextItemKindMessage,
						Message: &transcript.CompactedMessageItem{
							Role:         "user",
							MessageClass: transcript.CompactedMessageClassSummary,
							Content: []transcript.CompactedMessageContentItem{
								{Type: "text", Text: "compact summary should stay supplemental"},
							},
						},
					},
				},
			},
		},
		transcript.Message{
			UUID:      "child-38",
			Role:      "assistant",
			Timestamp: baseTime.Add(39 * time.Minute),
			Text:      "post compact tail",
		},
		transcript.Message{
			UUID:      "child-39",
			Role:      "user",
			Timestamp: baseTime.Add(40 * time.Minute),
			Text:      "tail request",
		},
	)
	return messages
}

func findReorientItem(items []ReorientItem, kind ReorientItemKind) (ReorientItem, bool) {
	for _, item := range items {
		if item.Kind == kind {
			return item, true
		}
	}
	return ReorientItem{}, false
}

func joinedReorientItemBodies(items []ReorientItem, kind ReorientItemKind) string {
	var bodies []string
	for _, item := range items {
		if item.Kind == kind {
			bodies = append(bodies, item.Body)
		}
	}
	return strings.Join(bodies, "\n")
}
