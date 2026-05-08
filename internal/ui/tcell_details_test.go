package ui

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

func TestDetailsView_ContextUsesLoadingAndDiagnosticsInsteadOfUnavailable(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	detail := SessionDetail{
		Model:                 "opus",
		LastMessageTokens:     44,
		ContextUsageStatus:    "failed; retrying",
		TranscriptStatsStatus: "loading...",
	}

	lines := flattenSegments(view.buildLeft(sess, detail))
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "unavailable") {
		t.Fatalf("details pane should not render unavailable:\n%s", joined)
	}
	if !strings.Contains(joined, "Context") || !strings.Contains(joined, "loading...") {
		t.Fatalf("details pane missing loading context row:\n%s", joined)
	}
	if !strings.Contains(joined, "Context probe") || !strings.Contains(joined, "failed; retrying") {
		t.Fatalf("details pane missing retry diagnostic:\n%s", joined)
	}
	if !strings.Contains(joined, "Transcript") || !strings.Contains(joined, "Last msg est") || !strings.Contains(joined, "loading...") {
		t.Fatalf("details pane missing transcript loading row:\n%s", joined)
	}
}

func TestDetailsView_ContextShowsExactUsageWhenLoaded(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	detail := SessionDetail{
		Model: "opus",
		ContextUsage: SessionContextUsage{
			TotalTokens:    84320,
			MaxTokens:      200000,
			Percentage:     42,
			MessagesTokens: 41234,
		},
		ContextUsageLoaded:    true,
		TranscriptStatsLoaded: true,
	}

	lines := flattenSegments(view.buildLeft(sess, detail))
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "84.3k/200k tok  42%") {
		t.Fatalf("details pane missing exact context usage:\n%s", joined)
	}
	if !strings.Contains(joined, "Messages") || !strings.Contains(joined, "~41.2k tok") {
		t.Fatalf("details pane missing live context message token row:\n%s", joined)
	}
	if strings.Contains(joined, "Overview\n  Last msg") || strings.Contains(joined, "Overview\r\n  Last msg") {
		t.Fatalf("details pane should not render transcript-derived rows in Overview:\n%s", joined)
	}
	if strings.Contains(joined, "Diagnostics") {
		t.Fatalf("details pane should not show diagnostics for successful probe:\n%s", joined)
	}
}

func TestDetailsView_TranscriptSectionSeparatesHeuristicsFromContext(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	detail := SessionDetail{
		Model:                "opus",
		LastMessageTokens:    178000,
		TotalMessages:        981,
		CompactionCount:      2,
		LastPreCompactTokens: 543000,
		ContextUsage: SessionContextUsage{
			TotalTokens:    506400,
			MaxTokens:      1000000,
			Percentage:     51,
			MessagesTokens: 474415,
		},
		ContextUsageLoaded:    true,
		TranscriptStatsLoaded: true,
	}

	lines := flattenSegments(view.buildLeft(sess, detail))
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "Overview") || !strings.Contains(joined, "Context") || !strings.Contains(joined, "Messages") {
		t.Fatalf("details pane missing context overview rows:\n%s", joined)
	}
	if !strings.Contains(joined, "Transcript") || !strings.Contains(joined, "Visible msgs") || !strings.Contains(joined, "Last msg est") || !strings.Contains(joined, "Compactions") {
		t.Fatalf("details pane missing transcript section rows:\n%s", joined)
	}
}

func TestDetailsView_ConversationShowsLoadingSpinner(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}

	lines := flattenSegments(view.buildRight(sess, SessionDetail{ConversationLoading: true}))
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "loading conversation") {
		t.Fatalf("conversation pane missing loading state:\n%s", joined)
	}
	if strings.Contains(joined, "no visible messages") {
		t.Fatalf("conversation pane should not show empty state while loading:\n%s", joined)
	}
}

func TestDetailsView_SetPreservesScrollOffsetsForSameSession(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	view.Set(sess, SessionDetail{Model: "opus", ConversationLoading: true})
	view.Left.Offset = 7
	view.Right.Offset = 13

	view.Set(sess, SessionDetail{
		Model: "opus",
		AllMessages: []DetailMessage{{
			Role: "assistant",
			Text: "loaded",
		}},
	})

	if view.Left.Offset != 7 {
		t.Fatalf("left offset reset to %d", view.Left.Offset)
	}
	if view.Right.Offset != 13 {
		t.Fatalf("right offset reset to %d", view.Right.Offset)
	}
}

func TestDetailsView_SetResetsScrollOffsetsForDifferentSession(t *testing.T) {
	view := NewDetailsView()
	first := &session.Session{
		Name: "first",
		Metadata: session.Metadata{
			Name:      "first",
			SessionID: "1234",
		},
	}
	second := &session.Session{
		Name: "second",
		Metadata: session.Metadata{
			Name:      "second",
			SessionID: "5678",
		},
	}
	view.Set(first, SessionDetail{Model: "opus", ConversationLoading: true})
	view.Left.Offset = 7
	view.Right.Offset = 13

	view.Set(second, SessionDetail{Model: "opus", ConversationLoading: true})

	if view.Left.Offset != 0 {
		t.Fatalf("left offset = %d, want reset", view.Left.Offset)
	}
	if view.Right.Offset != 0 {
		t.Fatalf("right offset = %d, want reset", view.Right.Offset)
	}
}

func TestFormatCompactTokens(t *testing.T) {
	cases := map[int]string{
		44:      "44",
		1000:    "1k",
		41234:   "41.2k",
		100000:  "100k",
		200000:  "200k",
		1000000: "1M",
		1250000: "1.2M",
	}
	for in, want := range cases {
		if got := formatCompactTokens(in); got != want {
			t.Fatalf("formatCompactTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

func flattenSegments(lines [][]TextSegment) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		var b strings.Builder
		for _, seg := range line {
			b.WriteString(seg.Text)
		}
		out = append(out, b.String())
	}
	return out
}

// TestDetailsView_ContextLoadingRowEmitsSpinnerSegment exercises the
// new live-spinner behavior. The Context and Messages rows must carry
// a TextSegment with Spinner=true so the glyph re-renders on every
// draw rather than freezing on the first frame.
func TestDetailsView_ContextLoadingRowEmitsSpinnerSegment(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	detail := SessionDetail{
		Model:              "opus",
		ContextUsageStatus: "probing",
	}

	rows := view.buildLeft(sess, detail)
	contextRow := findKVRow(rows, "Context")
	if contextRow == nil {
		t.Fatalf("missing Context row in:\n%s", strings.Join(flattenSegments(rows), "\n"))
	}
	if len(contextRow) < 2 {
		t.Fatalf("Context row has %d segments want >=2", len(contextRow))
	}
	value := contextRow[1]
	if !value.Spinner {
		t.Fatalf("Context value segment Spinner=false want true (would freeze the glyph)")
	}
	if value.Text != "probing" {
		t.Fatalf("Context value Text=%q want %q", value.Text, "probing")
	}
}

// TestDetailsView_TerminalStatusDoesNotSpin exercises the rule that
// a settled status (failed:, unsupported, ...) renders as a stable
// non-spinning row.
func TestDetailsView_TerminalStatusDoesNotSpin(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	detail := SessionDetail{
		Model:              "opus",
		ContextUsageStatus: "failed: probe timeout",
	}

	rows := view.buildLeft(sess, detail)
	contextRow := findKVRow(rows, "Context")
	if contextRow == nil || len(contextRow) < 2 {
		t.Fatalf("missing Context row")
	}
	if contextRow[1].Spinner {
		t.Fatalf("terminal status should not spin; Spinner=true on row %+v", contextRow[1])
	}
	if contextRow[1].Text != "failed: probe timeout" {
		t.Fatalf("terminal status text=%q want %q", contextRow[1].Text, "failed: probe timeout")
	}
}

// TestDetailsView_GenericProbingHidesDiagnosticsSection guards the
// "more elegant when all loading" rule: when ContextUsageStatus is a
// generic in-flight sentinel, the Diagnostics section is hidden so
// the user does not see the same word twice (Overview spinner +
// Diagnostics row).
func TestDetailsView_GenericProbingHidesDiagnosticsSection(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	for _, status := range []string{"probing", "loading...", "cooldown", ""} {
		detail := SessionDetail{Model: "opus", ContextUsageStatus: status}
		rows := view.buildLeft(sess, detail)
		joined := strings.Join(flattenSegments(rows), "\n")
		if strings.Contains(joined, "Diagnostics") {
			t.Fatalf("generic status %q should hide Diagnostics; got:\n%s", status, joined)
		}
		if strings.Contains(joined, "Context probe") {
			t.Fatalf("generic status %q should not surface Context probe row", status)
		}
	}
}

// TestDetailsView_ActionableStatusKeepsDiagnosticsSection confirms the
// diagnostics section still renders for failures and other
// actionable statuses.
func TestDetailsView_ActionableStatusKeepsDiagnosticsSection(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			SessionID:     "1234",
			WorkspaceRoot: "/Users/test/Sites/demo",
			Created:       time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC),
			LastAccessed:  time.Date(2026, 4, 24, 9, 5, 0, 0, time.UTC),
		},
	}
	detail := SessionDetail{
		Model:              "opus",
		ContextUsageStatus: "failed: probe spawn refused",
	}
	rows := view.buildLeft(sess, detail)
	joined := strings.Join(flattenSegments(rows), "\n")
	if !strings.Contains(joined, "Diagnostics") {
		t.Fatalf("actionable status should keep Diagnostics:\n%s", joined)
	}
	if !strings.Contains(joined, "failed: probe spawn refused") {
		t.Fatalf("actionable status should appear in Diagnostics:\n%s", joined)
	}
}

func TestDetailsView_ResumeCommandUsesVisibleTitle(t *testing.T) {
	view := NewDetailsView()
	sess := &session.Session{
		Name: "merry-swan",
		Metadata: session.Metadata{
			Name:         "merry-swan",
			SessionID:    "uuid-1",
			DisplayTitle: "Merry Swan",
		},
	}

	rows := view.buildLeft(sess, SessionDetail{Model: "opus"})
	joined := strings.Join(flattenSegments(rows), "\n")

	if !strings.Contains(joined, `clyde resume "Merry Swan"`) {
		t.Fatalf("details resume command missing visible title:\n%s", joined)
	}
	if strings.Contains(joined, "clyde resume merry-swan") {
		t.Fatalf("details resume command used legacy row name:\n%s", joined)
	}
}

// findKVRow returns the row whose first segment label trims to key, or
// nil. The label segment uses fmt.Sprintf("  %-14s", k) so we trim
// leading and trailing whitespace before comparing.
func findKVRow(rows [][]TextSegment, key string) []TextSegment {
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		if strings.TrimSpace(row[0].Text) == key {
			return row
		}
	}
	return nil
}
