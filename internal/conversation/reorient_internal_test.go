package conversation

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/transcript"
)

func TestCapToLastLines(t *testing.T) {
	t.Parallel()
	text := "l1\nl2\nl3\nl4\nl5\n"
	capped, total, truncated := capToLastLines(text, 3)
	if capped != "l3\nl4\nl5\n" {
		t.Fatalf("capped = %q, want last three lines", capped)
	}
	if total != 3 || !truncated {
		t.Fatalf("total = %d truncated = %v, want 3 and true", total, truncated)
	}

	whole, total, truncated := capToLastLines(text, 10)
	if whole != text || total != 5 || truncated {
		t.Fatalf("uncapped whole = %q total = %d truncated = %v", whole, total, truncated)
	}

	zero, total, truncated := capToLastLines(text, 0)
	if zero != text || total != 5 || truncated {
		t.Fatalf("zero cap whole = %q total = %d truncated = %v", zero, total, truncated)
	}

	empty, total, truncated := capToLastLines("", 3)
	if empty != "" || total != 0 || truncated {
		t.Fatalf("empty cap = %q total = %d truncated = %v", empty, total, truncated)
	}
}

func TestCapToLastBytesKeepsTailOnLineBoundary(t *testing.T) {
	t.Parallel()
	text := "alpha line\nbravo line\ncharlie line\n"
	want := "bravo line\ncharlie line\n"
	maxBytes := len(want) + 3
	capped, truncated := capToLastBytes(text, maxBytes)
	if capped != want {
		t.Fatalf("capped = %q, want %q", capped, want)
	}
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if len(capped) > maxBytes {
		t.Fatalf("capped len = %d, want <= %d", len(capped), maxBytes)
	}
}

func TestRenderReorientSnapshotHonorsMaxBytes(t *testing.T) {
	t.Parallel()
	fixture := newReorientFixture(t)
	const maxBytes = 180
	page, err := fixture.index.ReorientPage(
		context.Background(),
		ReorientOptions{ConversationID: fixture.child.ID, MaxBytes: maxBytes},
		"",
		10000,
	)
	if err != nil {
		t.Fatalf("ReorientPage: %v", err)
	}
	if page.TotalBytes > maxBytes {
		t.Fatalf("TotalBytes = %d, want <= %d", page.TotalBytes, maxBytes)
	}
	if len(page.Body) > maxBytes {
		t.Fatalf("page body len = %d, want <= %d", len(page.Body), maxBytes)
	}
}

func TestRenderReorientSnapshotStripsPriorInjection(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 27, 13, 0, 0, 0, time.UTC)
	priorInjectedSummary := "summary before\n" +
		reorienttag.PreCompactionTranscriptOpen + "\n" +
		"OLD-NESTED-TRANSCRIPT\n" +
		reorienttag.PreCompactionTranscriptClose + "\n" +
		"summary after"
	fixture := buildReorientFixture(t, []transcript.Message{
		{
			UUID:      "child-summary-with-prior-injection",
			Role:      "user",
			Timestamp: baseTime,
			Text:      priorInjectedSummary,
			Compaction: &transcript.CompactionMetadata{
				Kind:    transcript.CompactionKindSummary,
				Trigger: transcript.CompactionTriggerManual,
				ContextItems: []transcript.CompactedContextItem{
					{
						Kind: transcript.CompactedContextItemKindMessage,
						Message: &transcript.CompactedMessageItem{
							Role:         "user",
							MessageClass: transcript.CompactedMessageClassSummary,
							Content: []transcript.CompactedMessageContentItem{
								{Type: "text", Text: priorInjectedSummary},
							},
						},
					},
				},
			},
		},
	})
	page, err := fixture.index.ReorientPage(
		context.Background(),
		ReorientOptions{
			ConversationID:      fixture.child.ID,
			SyntheticPreCompact: true,
			IncludeToolOutputs:  true,
			MaxBytes:            0,
			MaxLines:            0,
		},
		"",
		10000,
	)
	if err != nil {
		t.Fatalf("ReorientPage: %v", err)
	}
	if strings.Contains(page.Body, "OLD-NESTED-TRANSCRIPT") {
		t.Fatalf("prior injected transcript survived:\n%s", page.Body)
	}
	if strings.Contains(page.Body, reorienttag.PreCompactionTranscriptOpen) ||
		strings.Contains(page.Body, reorienttag.PreCompactionTranscriptClose) {
		t.Fatalf("prior injection markers survived:\n%s", page.Body)
	}
	for _, want := range []string{"summary before", "summary after"} {
		if !strings.Contains(page.Body, want) {
			t.Fatalf("recovered body missing stripped summary text %q:\n%s", want, page.Body)
		}
	}
}

func TestCollapseRepeatedRunsKeepsFirstAndDistinctTurns(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	distinct := transcript.Message{
		UUID:      "distinct",
		Role:      "assistant",
		Timestamp: baseTime.Add(4 * time.Minute),
		Text:      "monitor finished with a new result",
	}
	messages := []transcript.Message{
		{
			UUID:      "poll-1",
			Role:      "assistant",
			Timestamp: baseTime,
			Text:      "monitor tick 1 at 2026-06-27T14:00:01Z attempt 1 count 1 for 1s",
		},
		{
			UUID:      "poll-2",
			Role:      "assistant",
			Timestamp: baseTime.Add(time.Minute),
			Text:      "monitor tick 2 at 2026-06-27T14:01:02Z attempt 2 count 2 for 2s",
		},
		{
			UUID:      "poll-3",
			Role:      "assistant",
			Timestamp: baseTime.Add(2 * time.Minute),
			Text:      "monitor tick 3 at 2026-06-27T14:02:03Z attempt 3 count 3 for 3s",
		},
		distinct,
	}
	collapsed := collapseRepeatedRuns(messages)
	if len(collapsed) != 2 {
		t.Fatalf("collapsed len = %d, want 2", len(collapsed))
	}
	if collapsed[0].UUID != "poll-1" {
		t.Fatalf("first collapsed message UUID = %q, want poll-1", collapsed[0].UUID)
	}
	if !strings.Contains(collapsed[0].Text, "[collapsed 2 near-identical turns]") {
		t.Fatalf("collapsed marker missing from first message text: %q", collapsed[0].Text)
	}
	if collapsed[1].UUID != distinct.UUID ||
		collapsed[1].Role != distinct.Role ||
		collapsed[1].Text != distinct.Text {
		t.Fatalf("distinct message changed: %#v", collapsed[1])
	}
}

func TestStripPriorReorientInjectionSpansHandlesNesting(t *testing.T) {
	t.Parallel()
	openTag := reorienttag.PreCompactionTranscriptOpen
	closeTag := reorienttag.PreCompactionTranscriptClose
	body := "head " + openTag + "\nouter A\n" + openTag + "\ninner nested\n" +
		closeTag + "\nouter B\n" + closeTag + " tail"
	got, changed := stripPriorReorientInjectionSpans(body)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if got != "head  tail" {
		t.Fatalf("got %q, want %q (whole outer span removed)", got, "head  tail")
	}
	if strings.Contains(got, openTag) || strings.Contains(got, closeTag) {
		t.Fatalf("orphan injection marker survived: %q", got)
	}
	if strings.Contains(got, "inner nested") || strings.Contains(got, "outer") {
		t.Fatalf("nested injection content survived: %q", got)
	}
}

func TestCollapseRepeatedRunsKeepsDistinctToolCalls(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 27, 15, 0, 0, 0, time.UTC)
	makeToolTurn := func(uuid string, command string, minute int) transcript.Message {
		return transcript.Message{
			UUID:      uuid,
			Role:      "assistant",
			Timestamp: baseTime.Add(time.Duration(minute) * time.Minute),
			Text:      "running a shell command",
			HasTools:  true,
			Tools: []transcript.ToolCall{
				{
					Name:  "Bash",
					Input: transcript.ToolInputJSON{Raw: []byte(`{"command":"` + command + `"}`)},
				},
			},
		}
	}
	distinctTools := []transcript.Message{
		makeToolTurn("tool-1", "ls", 0),
		makeToolTurn("tool-2", "pwd", 1),
		makeToolTurn("tool-3", "whoami", 2),
	}
	collapsedDistinct := collapseRepeatedRuns(distinctTools)
	if len(collapsedDistinct) != 3 {
		t.Fatalf("distinct tool calls collapsed to %d, want 3 (must not merge)", len(collapsedDistinct))
	}

	identicalTools := []transcript.Message{
		makeToolTurn("same-1", "gh pr checks", 0),
		makeToolTurn("same-2", "gh pr checks", 1),
		makeToolTurn("same-3", "gh pr checks", 2),
	}
	collapsedSame := collapseRepeatedRuns(identicalTools)
	if len(collapsedSame) != 1 {
		t.Fatalf("identical tool calls collapsed to %d, want 1", len(collapsedSame))
	}
	if !strings.Contains(collapsedSame[0].Text, "[collapsed 2 near-identical turns]") {
		t.Fatalf("collapse marker missing from identical tool run: %q", collapsedSame[0].Text)
	}
}

func TestSliceReorientBodyAdvancesAndReassembles(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	for index := range 40 {
		fmt.Fprintf(&builder, "line %02d of the recovered transcript\n", index)
	}
	body := builder.String()

	var rebuilt strings.Builder
	offset := 0
	pages := 0
	for offset < len(body) {
		slice, next := sliceReorientBody(body, offset, 100)
		if next <= offset {
			t.Fatalf("slice did not advance: offset %d next %d", offset, next)
		}
		if slice != body[offset:next] {
			t.Fatalf("slice mismatch at offset %d", offset)
		}
		rebuilt.WriteString(slice)
		offset = next
		pages++
		if pages > 1000 {
			t.Fatalf("slice loop did not terminate")
		}
	}
	if rebuilt.String() != body {
		t.Fatalf("reassembled body != original")
	}
	if pages < 2 {
		t.Fatalf("pages = %d, want more than one", pages)
	}
}

func TestSliceReorientBodyHardCutsLongLine(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("x", 250)
	slice, next := sliceReorientBody(body, 0, 100)
	if len(slice) != 100 || next != 100 {
		t.Fatalf("hard cut slice len = %d next = %d, want 100 and 100", len(slice), next)
	}
}

func TestReorientCursorRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []reorientCursor{
		{Fingerprint: "", Offset: 0},
		{Fingerprint: "child-37", Offset: 42},
		{Fingerprint: "boundary-uuid", Offset: 9999},
	}
	for _, want := range cases {
		encoded := encodeReorientCursor(want)
		got, err := decodeReorientCursor(encoded)
		if err != nil {
			t.Fatalf("decode(%q) err = %v", encoded, err)
		}
		if got != want {
			t.Fatalf("round trip = %#v, want %#v", got, want)
		}
	}
}

func TestReorientCursorEmptyIsZero(t *testing.T) {
	t.Parallel()
	got, err := decodeReorientCursor("")
	if err != nil {
		t.Fatalf("empty cursor err = %v", err)
	}
	if got != (reorientCursor{Fingerprint: "", Offset: 0}) {
		t.Fatalf("empty cursor = %#v, want zero", got)
	}
}

func TestReorientCursorRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := decodeReorientCursor("!!!not-base64!!!"); err == nil {
		t.Fatalf("garbage cursor accepted, want error")
	}
}

func TestReorientSelectorPicksPreBoundaryHistory(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	messages := reorientChildMessages(baseTime)
	if got := reorientSelector(messages, false); got != "1..2" {
		t.Fatalf("selector = %q, want 1..2 (all segments before the latest boundary)", got)
	}
	if got := reorientSelector(messages, true); got != "all" {
		t.Fatalf("synthetic selector = %q, want all", got)
	}
	uncompacted := []transcript.Message{{UUID: "a", Role: "user", Text: "hi"}}
	if got := reorientSelector(uncompacted, false); got != "0" {
		t.Fatalf("uncompacted selector = %q, want 0", got)
	}
}

func TestReorientFingerprintIsLatestBoundary(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	messages := reorientChildMessages(baseTime)
	if got := reorientFingerprint(messages); got != "child-37" {
		t.Fatalf("fingerprint = %q, want child-37", got)
	}
	uncompacted := []transcript.Message{{UUID: "a", Role: "user", Text: "hi"}}
	if got := reorientFingerprint(uncompacted); got != "" {
		t.Fatalf("uncompacted fingerprint = %q, want empty", got)
	}
}

func TestReorientPageRecoversPreBoundaryHistoryOnly(t *testing.T) {
	t.Parallel()
	fixture := newReorientFixture(t)
	body, total, pages := collectReorientPages(t, fixture.index, ReorientOptions{ConversationID: fixture.child.ID})
	if total <= 0 || pages < 1 {
		t.Fatalf("total = %d pages = %d, want positive", total, pages)
	}
	for _, want := range []string{"segment beginning detail", "I rather idiot proof it", "working before compact"} {
		if !strings.Contains(body, want) {
			t.Fatalf("recovered body missing pre-boundary detail %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"post compact tail", "tail request"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("recovered body included post-boundary tail %q:\n%s", unwanted, body)
		}
	}
}

func TestReorientPageStableWhenTailGrows(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	base := buildReorientFixture(t, nil)
	baseBody, baseTotal, _ := collectReorientPages(t, base.index, ReorientOptions{ConversationID: base.child.ID})

	grown := buildReorientFixture(t, []transcript.Message{
		{
			UUID:      "child-40",
			Role:      "assistant",
			Timestamp: baseTime.Add(41 * time.Minute),
			Text:      "new post compact work after the loop started",
		},
		{
			UUID:      "child-41",
			Role:      "user",
			Timestamp: baseTime.Add(42 * time.Minute),
			Text:      "even more new tail",
		},
	})
	grownBody, grownTotal, _ := collectReorientPages(t, grown.index, ReorientOptions{ConversationID: grown.child.ID})

	if grownTotal != baseTotal {
		t.Fatalf("total bytes changed after tail growth: base %d grown %d", baseTotal, grownTotal)
	}
	if grownBody != baseBody {
		t.Fatalf("recovered body changed after tail growth")
	}
	if strings.Contains(grownBody, "new post compact work") || strings.Contains(grownBody, "even more new tail") {
		t.Fatalf("recovered body leaked post-boundary growth:\n%s", grownBody)
	}
}

func TestReorientPageRestartsOnFingerprintMismatch(t *testing.T) {
	t.Parallel()
	fixture := newReorientFixture(t)
	staleCursor := encodeReorientCursor(reorientCursor{Fingerprint: "stale-fingerprint", Offset: 24})
	page, err := fixture.index.ReorientPage(context.Background(), ReorientOptions{ConversationID: fixture.child.ID}, staleCursor, 200)
	if err != nil {
		t.Fatalf("ReorientPage: %v", err)
	}
	if !page.Restart {
		t.Fatalf("restart = false, want true on fingerprint mismatch")
	}
	if page.Offset != 0 {
		t.Fatalf("offset = %d, want 0 after restart", page.Offset)
	}
	firstPage, err := fixture.index.ReorientPage(context.Background(), ReorientOptions{ConversationID: fixture.child.ID}, "", 200)
	if err != nil {
		t.Fatalf("ReorientPage first: %v", err)
	}
	if page.Body != firstPage.Body {
		t.Fatalf("restart body != first page body")
	}
}

func TestExportMaxLinesKeepsLastLines(t *testing.T) {
	t.Parallel()
	fixture := newReorientFixture(t)
	options := ExportOptions{
		Format:     ExportFormatMarkdown,
		Whitespace: WhitespaceDense,
		Content:    NewContentKindSet(ContentKindChat, ContentKindToolCalls),
		Compaction: CompactionExportOptions{IncludeSelector: "all"},
	}
	full, err := fixture.index.Export(fixture.child, options)
	if err != nil {
		t.Fatalf("Export full: %v", err)
	}
	fullLines := countLines(string(full))
	if fullLines <= 5 {
		t.Fatalf("fixture export has %d lines, want more than 5 for a meaningful cap", fullLines)
	}

	options.MaxLines = 5
	capped, err := fixture.index.Export(fixture.child, options)
	if err != nil {
		t.Fatalf("Export capped: %v", err)
	}
	if got := countLines(string(capped)); got != 5 {
		t.Fatalf("capped export has %d lines, want 5", got)
	}
	if !strings.HasSuffix(strings.TrimRight(string(full), "\n"), strings.TrimRight(string(capped), "\n")) {
		t.Fatalf("capped export is not the tail of the full export:\ncapped=%q", string(capped))
	}
}

func countLines(text string) int {
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

func collectReorientPages(t *testing.T, idx *Index, options ReorientOptions) (string, int, int) {
	t.Helper()
	var builder strings.Builder
	cursor := ""
	total := -1
	pages := 0
	seen := map[string]bool{}
	for {
		page, err := idx.ReorientPage(context.Background(), options, cursor, 200)
		if err != nil {
			t.Fatalf("ReorientPage: %v", err)
		}
		if total < 0 {
			total = page.TotalBytes
		}
		builder.WriteString(page.Body)
		pages++
		if page.Remaining <= 0 {
			break
		}
		if page.NextCursor == "" {
			t.Fatalf("remaining %d but next cursor is empty", page.Remaining)
		}
		if seen[page.NextCursor] {
			t.Fatalf("repeated next cursor %q", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
		if pages > 1000 {
			t.Fatalf("reorient page loop did not terminate")
		}
	}
	return builder.String(), total, pages
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
	return buildReorientFixture(t, nil)
}

func buildReorientFixture(t *testing.T, extraChildTail []transcript.Message) reorientFixture {
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
	childMessages := reorientChildMessages(baseTime)
	if len(extraChildTail) > 0 {
		childMessages = append(childMessages, extraChildTail...)
	}
	registry := NewRegistry()
	registry.Register(staticParser{
		provider: providerid.ProviderClaude,
		messagesByPath: map[string][]transcript.Message{
			parent.ArtifactPath: reorientParentMessages(baseTime),
			child.ArtifactPath:  childMessages,
		},
	})
	index := NewIndex(registry)
	records := []Record{child, parent}
	index.records = records
	index.prevRecords = recordsByPath(index.records)
	index.loaded = true
	index.lastRefresh = time.Now()
	index.cachePath = filepath.Join(t.TempDir(), cacheFilename)
	index.debounce = time.Hour
	index.scanProvider = func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: records, stamps: nil}, nil
	}
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
			Text:      "oldest pre-boundary detail",
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
