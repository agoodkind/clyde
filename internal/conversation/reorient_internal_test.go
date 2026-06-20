package conversation

import (
	"strings"
	"testing"

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
