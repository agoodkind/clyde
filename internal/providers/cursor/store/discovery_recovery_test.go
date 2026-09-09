package cursorstore_test

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

func TestParserDiscoveryRecoversAfterOrdinaryContention(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "initial read"
		if warm {
			name = "prior contribution"
		}
		t.Run(name, func(t *testing.T) {
			root, _, parser := discoveryFixture(t)
			var records map[string]conversation.Record
			if warm {
				_, records = discoverRecords(t, parser, nil)
			}
			execDiscoveryStatements(t, root.GlobalDBPath, `UPDATE composerHeaders SET value = '{"name":"Changed before contention"}'`)
			writer := openDiscoveryWriter(t, root.GlobalDBPath)
			if _, err := writer.Exec("BEGIN EXCLUSIVE"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = writer.Exec("ROLLBACK") })
			before, err := os.Stat(root.GlobalDBPath)
			if err != nil {
				t.Fatal(err)
			}
			failed, records := discoverRecords(t, parser, records)
			candidate, found := failed["composer-a"]
			if found != warm {
				t.Fatalf("failed discovery found composer = %v, prior = %v", found, warm)
			}
			if warm && records[candidate.Path].Title != "Global title" {
				t.Fatal("contention replaced prior contribution")
			}
			if _, err := writer.Exec("ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(root.GlobalDBPath)
			if err != nil {
				t.Fatal(err)
			}
			assertSameRecoveryMetadata(t, before, after)
			db, err := cursorstore.OpenReadOnlyDatabase(t.Context(), root.GlobalDBPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := cursorstore.ReadComposerHeader(t.Context(), db, "composer-a"); err != nil || !found {
				t.Fatalf("direct header read = %v, %v", found, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, records := discoverRecords(t, parser, records)
			candidate, found = recovered["composer-a"]
			if !found || records[candidate.Path].Title != "Changed before contention" {
				t.Fatal("ordinary discovery retained availability failure after unchanged-metadata recovery")
			}
		})
	}
}

func assertSameRecoveryMetadata(t *testing.T, before, after os.FileInfo) {
	t.Helper()
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Fatal("availability recovery changed content metadata")
	}
}

func TestWorkspaceAvailabilityRetriesOncePerDiscoveryPass(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	_, records := discoverRecords(t, parser, nil)
	execDiscoveryStatements(t, entry.StateDBPath, `UPDATE ItemTable SET value = '{"tabs":[{"tabId":"tab","chatTitle":"Recovered legacy title","bubbles":[{"type":"user","text":"legacy question"}]}]}' WHERE key = 'workbench.panel.aichat.view.aichat.chatdata'`)
	writer := openDiscoveryWriter(t, entry.StateDBPath)
	if _, err := writer.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = writer.Exec("ROLLBACK") })
	before, err := os.Stat(entry.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	counter := cursorstore.ObserveDiscoveryReads(t)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for pass := range 2 {
		failed, previous := discoverRecords(t, parser, records)
		records = previous
		if records[failed["workspace~tab"].Path].Title != "Legacy title" {
			t.Fatal("unavailable workspace replaced prior legacy chat")
		}
		if attempts := counter.TakeOpenAttempts(entry.StateDBPath); attempts != 1 {
			t.Errorf("pass %d made %d workspace open attempts, want one shared retry", pass, attempts)
		}
	}
	if count := strings.Count(logs.String(), "sqlite_ping_failed"); count != 1 {
		t.Errorf("unchanged availability warning count = %d: %s", count, logs.String())
	}
	if _, err := writer.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(entry.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRecoveryMetadata(t, before, after)
	recovered, _ := discoverRecords(t, parser, records)
	record, ok := parser.ScanRecord(recovered["workspace~tab"].Path, recovered["workspace~tab"].Stamp)
	if !ok || record.Title != "Recovered legacy title" {
		t.Fatal("workspace did not retry after availability returned")
	}
	if attempts := counter.TakeOpenAttempts(entry.StateDBPath); attempts != 1 {
		t.Errorf("recovery made %d workspace attempts, want one", attempts)
	}
	match, err := parser.ResolveRequestID(t.Context(), "request-one", conversation.RequestLookupOptions{})
	if err != nil || !match.Found {
		t.Fatalf("recovered request ring = %+v, %v", match, err)
	}
	if attempts := counter.TakeOpenAttempts(entry.StateDBPath); attempts != 0 {
		t.Errorf("request reread recovered workspace %d times", attempts)
	}
	if counts := counter.Take(root.GlobalDBPath); counts.Opens != 1 {
		t.Errorf("unexpected global work: %+v", counts)
	}
}

func TestCachedReadDiagnosticsPreserveChangesAndOrdinaryLogs(t *testing.T) {
	root, _, _ := discoveryFixture(t)
	if err := os.Rename(root.GlobalDBPath, root.GlobalDBPath+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root.GlobalDBPath, 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for range 2 {
		if data := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath); data.Err == nil {
			t.Fatal("directory fixture unexpectedly opened")
		}
	}
	if count := strings.Count(logs.String(), "sqlite_ping_failed"); count != 1 {
		t.Fatalf("identical diagnostic count = %d: %s", count, logs.String())
	}
	if !strings.Contains(logs.String(), root.GlobalDBPath) || !strings.Contains(logs.String(), `"err":`) {
		t.Fatalf("original path/cause lost: %s", logs.String())
	}
	if err := os.Chmod(root.GlobalDBPath, 0o700); err != nil {
		t.Fatal(err)
	}
	cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if count := strings.Count(logs.String(), "sqlite_ping_failed"); count != 2 {
		t.Fatalf("changed input did not reset diagnostics: %s", logs.String())
	}
	if err := os.Remove(root.GlobalDBPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.GlobalDBPath, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if !strings.Contains(logs.String(), "file is not a database") {
		t.Fatalf("changed failure was suppressed: %s", logs.String())
	}
	if err := os.Remove(root.GlobalDBPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root.GlobalDBPath+".saved", root.GlobalDBPath); err != nil {
		t.Fatal(err)
	}
	if data := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath); data.Err != nil || len(data.Headers) != 2 {
		t.Fatalf("recovery = %+v", data)
	}

	// Direct readers use the unmodified default logger, outside cached retries.
	logs.Reset()
	for range 2 {
		db, err := cursorstore.OpenReadOnlyDatabase(t.Context(), root.WorkspaceStorageDir)
		if db != nil {
			_ = db.Close()
		}
		if err == nil {
			t.Fatal("directory opened as a database")
		}
	}
	if count := strings.Count(logs.String(), "sqlite_ping_failed"); count != 2 {
		t.Fatalf("ordinary reader diagnostics were suppressed: %s", logs.String())
	}
}
