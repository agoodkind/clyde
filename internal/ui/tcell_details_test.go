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
