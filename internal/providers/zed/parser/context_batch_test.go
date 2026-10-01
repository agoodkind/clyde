package parser

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestReadContextWindowsReadsOneFreshZedStream(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updated := time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC)
	data := []byte(`{"version":"0.3.0","title":"Windows","messages":[{"User":{"id":"one","content":[{"Text":"first"}]}},{"User":{"id":"two","content":[{"Text":"second"}]}},{"User":{"id":"three","content":[{"Text":"third"}]}}]}`)
	writeThreadsRow(t, root, "context-windows", "", updated, data)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "context-windows", "", "Windows", "", updated)
	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover actual Zed source: %v, %d candidates", err, len(candidates))
	}
	writer, err := sql.Open("sqlite3", filepath.Join(root, "threads", "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	windows := []conversation.ContextMessageWindow{{Start: 2, End: 3}, {Start: 0, End: 2}, {Start: 1, End: 3}}
	options := conversation.LoadOptions{}
	stats, err := parser.ReadContextWindows(t.Context(), candidates[0].Path, "", windows, options, func(selected [][]transcript.Message) error {
		if len(selected) != 3 || len(selected[0]) != 1 || len(selected[1]) != 2 || len(selected[2]) != 2 {
			t.Fatal("fresh Zed windows have incorrect positions")
		}
		if selected[0][0].Text != "third" || selected[1][0].Text != "first" || selected[1][1].Text != "second" || selected[2][1].Text != "third" {
			t.Fatal("fresh Zed windows have incorrect source content")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.SourceReads != 1 || stats.MessagesVisited != 3 || stats.MessagesRetained != 3 || stats.Windows != 3 {
		t.Fatalf("Zed fresh stream statistics differ: %+v", stats)
	}
	executeZedContextFixture(t, writer, "context-wal.sql")
	stats, err = parser.ReadContextWindows(t.Context(), candidates[0].Path, "", windows, options, func([][]transcript.Message) error {
		executeZedContextFixture(t, writer, "context-windows-edit.sql")
		return nil
	})
	if err == nil || stats.SourceReads != 1 {
		t.Fatalf("Zed plural verification accepted a backing edit: %+v, %v", stats, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = parser.ReadContextWindows(ctx, candidates[0].Path, "", windows, options, func([][]transcript.Message) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Zed plural cancellation returned %v", err)
	}
}
