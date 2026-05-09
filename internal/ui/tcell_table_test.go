package ui

import (
	"strings"
	"testing"
)

func TestTableMoveDownFirstPressActivatesFirstRow(t *testing.T) {
	table := NewTableWidget([]string{"NAME"})
	table.Rows = [][]TableCell{
		{{Text: "a"}},
		{{Text: "b"}},
		{{Text: "c"}},
	}
	table.SelectedRow = 0
	table.Active = false
	table.Rect = Rect{X: 0, Y: 0, W: 40, H: 6}

	table.MoveDown(1)
	if !table.Active {
		t.Fatalf("expected first move-down to activate table")
	}
	if table.SelectedRow != 0 {
		t.Fatalf("expected first move-down to select first row, got %d", table.SelectedRow)
	}

	table.MoveDown(1)
	if table.SelectedRow != 1 {
		t.Fatalf("expected second move-down to select second row, got %d", table.SelectedRow)
	}
}

func TestTableMoveUpFirstPressActivatesCurrentRow(t *testing.T) {
	table := NewTableWidget([]string{"NAME"})
	table.Rows = [][]TableCell{
		{{Text: "a"}},
		{{Text: "b"}},
		{{Text: "c"}},
	}
	table.SelectedRow = 2
	table.Active = false
	table.Rect = Rect{X: 0, Y: 0, W: 40, H: 6}

	table.MoveUp(1)
	if !table.Active {
		t.Fatalf("expected first move-up to activate table")
	}
	if table.SelectedRow != 2 {
		t.Fatalf("expected first move-up to keep current row selected, got %d", table.SelectedRow)
	}

	table.MoveUp(1)
	if table.SelectedRow != 1 {
		t.Fatalf("expected second move-up to move selection, got %d", table.SelectedRow)
	}
}

// TestTableMaxColumnWidthCapsLongCell asserts that a column with a
// configured cap never widens beyond that cap, even when a single row
// has a very long value. This is the regression coverage for the
// "NAME column spans the entire terminal" report: without the cap,
// ColumnWidths grows to fit the longest cell and the rest of the
// table is pushed off screen on a narrow terminal.
func TestTableMaxColumnWidthCapsLongCell(t *testing.T) {
	const nameCap = 40
	table := NewTableWidget([]string{"NAME", "BASEDIR"})
	table.MaxColumnWidths = []int{nameCap, 0}
	long := strings.Repeat("x", 120)
	table.Rows = [][]TableCell{
		{{Text: long}, {Text: "/repo"}},
		{{Text: "short"}, {Text: "/other"}},
	}

	widths := table.ColumnWidths()
	if widths[0] != nameCap {
		t.Fatalf("expected NAME column to be capped at %d, got %d", nameCap, widths[0])
	}
	// BASEDIR has no cap, so it grows to whichever is wider between
	// the header label and the longest cell value.
	wantBasedir := len("BASEDIR")
	if widths[1] != wantBasedir {
		t.Fatalf("expected BASEDIR column to fit header (%d), got %d", wantBasedir, widths[1])
	}
}

// TestTableTruncateAddsEllipsis asserts that a value longer than the
// column cap is rendered with a trailing ellipsis so the user can
// tell the cell was clipped.
func TestTableTruncateAddsEllipsis(t *testing.T) {
	got := truncateToWidth("abcdefghij", 5)
	if got != "abcd…" {
		t.Fatalf("truncateToWidth(%q, 5) = %q, want %q", "abcdefghij", got, "abcd…")
	}
	if got := truncateToWidth("abc", 10); got != "abc" {
		t.Fatalf("short input should pass through, got %q", got)
	}
	if got := truncateToWidth("abcdef", 0); got != "abcdef" {
		t.Fatalf("zero cap should pass through, got %q", got)
	}
}
