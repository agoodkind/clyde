package cursorstore_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

func discoveryFixture(t *testing.T) (cursorstore.DataRoot, cursorstore.WorkspaceEntry, *cursorparser.Parser) {
	t.Helper()
	root, entry := cursorstore.CreateDiscoveryFixture(t)
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", root.RootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	execDiscoveryStatements(t, entry.StateDBPath,
		`UPDATE ItemTable SET value = '{"allComposers":[{"composerId":"composer-a","name":"Workspace title","isArchived":true}]}' WHERE key = 'composer.composerData'`,
		`INSERT INTO ItemTable VALUES ('workbench.panel.aichat.view.aichat.chatdata', '{"tabs":[{"tabId":"tab","chatTitle":"Legacy title","bubbles":[{"type":"user","text":"legacy question"}]}]}')`,
		`INSERT INTO ItemTable VALUES ('aiService.generations', '[{"generationUUID":"request-one","unixMs":1710000000050}]')`)
	execDiscoveryStatements(t, root.GlobalDBPath,
		`CREATE TABLE composerHeaders(composerId TEXT, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, value TEXT)`,
		`INSERT INTO composerHeaders VALUES ('composer-a', 'workspace', 1710000000000, 1710000000100, 0, 0, '{"name":"Global title"}')`,
		`UPDATE cursorDiskKV SET value = '{"composerId":"composer-a","name":"","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":1},{"bubbleId":"bubble-2","type":2}]}' WHERE key = 'composerData:composer-a'`,
		`UPDATE cursorDiskKV SET value = '{"_v":3,"bubbleId":"bubble-1","type":1,"text":"first","requestId":"request-one","createdAt":"2026-05-06T05:00:00Z"}' WHERE key = 'bubbleId:composer-a:bubble-1'`,
		`UPDATE cursorDiskKV SET value = '{"_v":3,"bubbleId":"bubble-2","type":2,"text":"last","createdAt":"2026-05-06T05:00:30Z"}' WHERE key = 'bubbleId:composer-a:bubble-2'`)
	return root, entry, cursorparser.New()
}

func execDiscoveryStatements(t *testing.T, path string, statements ...string) {
	t.Helper()
	db := openDiscoveryWriter(t, path)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func openDiscoveryWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func discoverRecords(t *testing.T, parser *cursorparser.Parser, prior map[string]conversation.Record) (map[string]conversation.ScanCandidate, map[string]conversation.Record) {
	t.Helper()
	candidates, err := parser.Discover(t.Context(), prior)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]conversation.ScanCandidate)
	records := make(map[string]conversation.Record)
	for _, candidate := range candidates {
		record, ok := parser.ScanRecord(candidate.Path, candidate.Stamp)
		if !ok {
			t.Fatalf("ScanRecord failed for %s", candidate.Path)
		}
		byID[record.NativeID] = candidate
		records[candidate.Path] = record
	}
	return byID, records
}

func TestParserSharedDiscoveryReadCounts(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	counter := cursorstore.ObserveDiscoveryReads(t)
	first, records := discoverRecords(t, parser, nil)
	initialGlobal, initialWorkspace := counter.Take(root.GlobalDBPath), counter.Take(entry.StateDBPath)
	t.Logf("initial discovery global=%+v workspace=%+v", initialGlobal, initialWorkspace)
	if initialGlobal.Opens != 1 || initialWorkspace.Opens != 1 || initialGlobal.SelectAuthorizations == 0 || initialWorkspace.SelectAuthorizations == 0 {
		t.Errorf("initial discovery must share one read per store: global=%+v workspace=%+v", initialGlobal, initialWorkspace)
	}
	second, _ := discoverRecords(t, parser, records)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("unchanged candidates changed")
	}
	unchangedGlobal, unchangedWorkspace := counter.Take(root.GlobalDBPath), counter.Take(entry.StateDBPath)
	t.Logf("unchanged discovery global=%+v workspace=%+v", unchangedGlobal, unchangedWorkspace)
	if unchangedGlobal.Opens != 0 || unchangedGlobal.SelectAuthorizations != 0 || unchangedWorkspace.Opens != 0 || unchangedWorkspace.SelectAuthorizations != 0 {
		t.Errorf("unchanged discovery performed content reads: global=%+v workspace=%+v", unchangedGlobal, unchangedWorkspace)
	}
	match, err := parser.ResolveRequestID(t.Context(), "request-one", conversation.RequestLookupOptions{})
	if err != nil || !match.Found || match.NativeConversationID != "composer-a" {
		t.Fatalf("request lookup = %+v, %v", match, err)
	}
	requestGlobal, requestWorkspace := counter.Take(root.GlobalDBPath), counter.Take(entry.StateDBPath)
	t.Logf("request lookup global=%+v workspace=%+v", requestGlobal, requestWorkspace)
	if requestWorkspace.Opens != 0 || requestWorkspace.SelectAuthorizations != 0 {
		t.Errorf("request lookup reread workspace: %+v", requestWorkspace)
	}
	if requestGlobal.Opens == 0 || requestGlobal.SelectAuthorizations == 0 {
		t.Fatal("instrument missed live request queries")
	}

	execDiscoveryStatements(t, root.GlobalDBPath, `INSERT INTO cursorDiskKV VALUES ('bubbleId:composer-a:orphan', '{"_v":3,"bubbleId":"orphan","type":2,"text":"middle","createdAt":"2026-05-06T05:00:10Z"}')`)
	changed, records := discoverRecords(t, parser, records)
	activeGlobal, activeWorkspace := counter.Take(root.GlobalDBPath), counter.Take(entry.StateDBPath)
	t.Logf("active discovery global=%+v workspace=%+v", activeGlobal, activeWorkspace)
	if activeGlobal.Opens != 1 || activeGlobal.SelectAuthorizations == 0 || activeWorkspace.Opens != 0 || activeWorkspace.SelectAuthorizations != 0 {
		t.Errorf("active-store isolation failed: global=%+v workspace=%+v", activeGlobal, activeWorkspace)
	}
	if first["composer-a"].Stamp.Equal(changed["composer-a"].Stamp) {
		t.Fatal("orphan append did not change stamp")
	}
	messages, err := conversation.CollectMessages(parser.Stream(changed["composer-a"].Path, conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Text != "first" || messages[1].Text != "middle" || messages[2].Text != "last" {
		t.Fatalf("orphan/order lost: %+v", messages)
	}
	record := records[changed["composer-a"].Path]
	if record.Title != "Global title" || record.Archived || record.WorkspaceRoot != "/tmp/project" {
		t.Fatalf("metadata precedence changed: %+v", record)
	}
}

func TestParserDiscoveryRefreshesWALAndMetadata(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	before, records := discoverRecords(t, parser, nil)
	writer := openDiscoveryWriter(t, root.GlobalDBPath)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "PRAGMA wal_checkpoint(TRUNCATE)"} {
		if _, err := writer.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	discoverRecords(t, parser, records)
	mainBefore, err := os.Stat(root.GlobalDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`UPDATE composerHeaders SET isArchived = 1, value = '{"name":"Renamed without timestamp"}' WHERE composerId = 'composer-a'`); err != nil {
		t.Fatal(err)
	}
	mainAfter, err := os.Stat(root.GlobalDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if mainBefore.Size() != mainAfter.Size() || !mainBefore.ModTime().Equal(mainAfter.ModTime()) {
		t.Fatal("fixture write changed main database instead of WAL only")
	}
	after, records := discoverRecords(t, parser, records)
	record := records[after["composer-a"].Path]
	if record.Title != "Renamed without timestamp" || !record.Archived || before["composer-a"].Stamp.Equal(after["composer-a"].Stamp) {
		t.Fatalf("WAL metadata change missed: %+v", record)
	}

	workspaceWriter := openDiscoveryWriter(t, entry.StateDBPath)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "PRAGMA wal_checkpoint(TRUNCATE)"} {
		if _, err := workspaceWriter.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	legacyBefore, records := discoverRecords(t, parser, records)
	workspaceMainBefore, err := os.Stat(entry.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceWriter.Exec(`UPDATE ItemTable SET value = '{"tabs":[{"tabId":"tab","chatTitle":"New legacy title","bubbles":[{"type":"user","text":"changed legacy question"}]}]}' WHERE key = 'workbench.panel.aichat.view.aichat.chatdata'`); err != nil {
		t.Fatal(err)
	}
	workspaceMainAfter, err := os.Stat(entry.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceMainBefore.Size() != workspaceMainAfter.Size() || !workspaceMainBefore.ModTime().Equal(workspaceMainAfter.ModTime()) {
		t.Fatal("workspace write was not WAL only")
	}
	legacyAfter, records := discoverRecords(t, parser, records)
	legacy := records[legacyAfter["workspace~tab"].Path]
	if legacy.Title != "New legacy title" || legacyBefore["workspace~tab"].Stamp.Equal(legacyAfter["workspace~tab"].Stamp) {
		t.Fatalf("legacy WAL change missed: %+v", legacy)
	}
	if err := os.WriteFile(entry.WorkspaceJSONPath, []byte(`{"folder":"vscode-remote://ssh-remote+machine/workspace%20name"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	descriptorAfter, records := discoverRecords(t, parser, records)
	if records[descriptorAfter["composer-a"].Path].WorkspaceRoot != "vscode-remote://ssh-remote+machine/workspace%20name" || descriptorAfter["workspace~tab"].Stamp.Equal(legacyAfter["workspace~tab"].Stamp) {
		t.Fatal("descriptor change was not reflected in both consumers")
	}
}

func TestParserDiscoveryKeepsFailedReadsAndDropsRemovedWorkspaces(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	before, records := discoverRecords(t, parser, nil)
	if err := os.WriteFile(entry.WorkspaceJSONPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, records := discoverRecords(t, parser, records)
	if records[after["composer-a"].Path].WorkspaceRoot != "/tmp/project" {
		t.Fatal("failed descriptor removed prior workspace identity")
	}

	execDiscoveryStatements(t, entry.StateDBPath, `UPDATE ItemTable SET value = '{' WHERE key IN ('composer.composerData', 'workbench.panel.aichat.view.aichat.chatdata', 'aiService.generations')`)
	failed, records := discoverRecords(t, parser, records)
	if len(failed) != len(before) || records[failed["workspace~tab"].Path].Title != "Legacy title" {
		t.Fatal("failed workspace read removed prior contributions")
	}
	match, err := parser.ResolveRequestID(t.Context(), "request-one", conversation.RequestLookupOptions{})
	if err != nil || match.Reason != conversation.RequestNotFoundReasonInconclusive {
		t.Fatalf("failed request-ring read became absence: %+v, %v", match, err)
	}
	counter := cursorstore.ObserveDiscoveryReads(t)
	discoverRecords(t, parser, records)
	if counts := counter.Take(entry.StateDBPath); counts.Opens != 0 || counts.SelectAuthorizations != 0 {
		t.Fatalf("unchanged failed workspace reread: %+v", counts)
	}

	execDiscoveryStatements(t, root.GlobalDBPath, `ALTER TABLE cursorDiskKV RENAME COLUMN key TO unreadable_key`)
	failedGlobal, records := discoverRecords(t, parser, records)
	if !failedGlobal["composer-a"].Stamp.Equal(failed["composer-a"].Stamp) {
		t.Fatal("failed global read changed prior stamp")
	}
	discoverRecords(t, parser, records)
	if counts := counter.Take(root.GlobalDBPath); counts.Opens != 1 {
		t.Fatalf("unchanged failed global store reopened: %+v", counts)
	}

	if err := os.RemoveAll(filepath.Dir(entry.StateDBPath)); err != nil {
		t.Fatal(err)
	}
	removed, _ := discoverRecords(t, parser, records)
	if _, exists := removed["workspace~tab"]; exists {
		t.Fatal("removed workspace retained legacy chat")
	}
	if err := os.Remove(root.GlobalDBPath); err != nil {
		t.Fatal(err)
	}
	empty, _ := discoverRecords(t, parser, records)
	if len(empty) != 0 {
		t.Fatalf("confirmed global deletion retained chats: %+v", empty)
	}
}

func TestParserDiscoveryConcurrentConsumersShareReads(t *testing.T) {
	root, entry, _ := discoveryFixture(t)
	counter := cursorstore.ObserveDiscoveryReads(t)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			parser := cursorparser.New()
			candidates, err := parser.Discover(context.Background(), nil)
			if err != nil || len(candidates) != 2 {
				t.Errorf("concurrent discovery = %d, %v", len(candidates), err)
			}
			lookup, err := cursorstore.FindGenerationEntry(context.Background(), root, "request-one")
			if err != nil || !lookup.Found() {
				t.Errorf("concurrent lookup = %+v, %v", lookup, err)
			}
		})
	}
	workers.Wait()
	if counts := counter.Take(root.GlobalDBPath); counts.Opens != 1 {
		t.Fatalf("concurrent global reads were not shared: %+v", counts)
	}
	if counts := counter.Take(entry.StateDBPath); counts.Opens != 1 {
		t.Fatalf("concurrent workspace reads were not shared: %+v", counts)
	}
}

func TestParserDiscoveryCachesEmptyStoresAndDescriptorErrors(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	execDiscoveryStatements(t, root.GlobalDBPath, "DELETE FROM cursorDiskKV", "DELETE FROM composerHeaders")
	execDiscoveryStatements(t, entry.StateDBPath, "DELETE FROM ItemTable")
	if err := os.WriteFile(entry.WorkspaceJSONPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	counter := cursorstore.ObserveDiscoveryReads(t)
	first, records := discoverRecords(t, parser, nil)
	if len(first) != 0 {
		t.Fatalf("empty stores yielded conversations: %+v", first)
	}
	counter.Take(root.GlobalDBPath)
	counter.Take(entry.StateDBPath)
	for range 3 {
		discoverRecords(t, parser, records)
		match, err := parser.ResolveRequestID(t.Context(), "missing", conversation.RequestLookupOptions{})
		if err != nil || match.Found || match.Reason != conversation.RequestNotFoundReasonNotRetained {
			t.Fatalf("empty lookup: %+v %v", match, err)
		}
	}
	if count := strings.Count(logs.String(), "workspace_descriptor_decode_failed"); count != 1 {
		t.Fatalf("descriptor warning count=%d logs=%s", count, logs.String())
	}
	if counts := counter.Take(entry.StateDBPath); counts.Opens != 0 || counts.SelectAuthorizations != 0 {
		t.Fatalf("empty workspace reread: %+v", counts)
	}
	if counts := counter.Take(root.GlobalDBPath); counts.Opens != 3 || counts.SelectAuthorizations != 0 {
		t.Fatalf("empty global discovery reread (three request opens expected): %+v", counts)
	}

	if err := os.Remove(entry.WorkspaceJSONPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(entry.WorkspaceJSONPath, 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		discoverRecords(t, parser, records)
	}
	if count := strings.Count(logs.String(), "workspace_descriptor_read_failed"); count != 1 {
		t.Fatalf("unreadable descriptor warning count=%d logs=%s", count, logs.String())
	}
	if counts := counter.Take(entry.StateDBPath); counts.Opens != 0 || counts.SelectAuthorizations != 0 {
		t.Fatalf("descriptor-only error reread workspace DB: %+v", counts)
	}
}

func TestParserDiscoveryAddsRemovesAndRecoversWorkspace(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	_, records := discoverRecords(t, parser, nil)
	otherDir := filepath.Join(root.WorkspaceStorageDir, "added")
	if err := os.Mkdir(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The workspace directory already exists when Cursor first writes its DB.
	discoverRecords(t, parser, records)
	otherDB := filepath.Join(otherDir, "state.vscdb")
	execDiscoveryStatements(t, otherDB,
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable VALUES ('workbench.panel.aichat.view.aichat.chatdata', '{"tabs":[{"tabId":"added-tab","chatTitle":"Added","bubbles":[{"type":"user","text":"new"}]}]}')`)
	added, records := discoverRecords(t, parser, records)
	if _, exists := added["added~added-tab"]; !exists {
		t.Fatal("new workspace DB in existing directory missed")
	}
	if err := os.Rename(otherDB, otherDB+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(otherDB, 0o755); err != nil {
		t.Fatal(err)
	}
	failed, records := discoverRecords(t, parser, records)
	if _, exists := failed["added~added-tab"]; !exists {
		t.Fatal("failed workspace open removed prior conversation")
	}
	if err := os.Remove(otherDB); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(otherDB+".saved", otherDB); err != nil {
		t.Fatal(err)
	}
	execDiscoveryStatements(t, otherDB, `UPDATE ItemTable SET value = '{"tabs":[{"tabId":"added-tab","chatTitle":"Recovered","bubbles":[{"type":"user","text":"recovered"}]}]}'`)
	recovered, records := discoverRecords(t, parser, records)
	if records[recovered["added~added-tab"].Path].Title != "Recovered" {
		t.Fatal("recovered workspace read was not refreshed")
	}
	if err := os.RemoveAll(otherDir); err != nil {
		t.Fatal(err)
	}
	removed, records := discoverRecords(t, parser, records)
	if _, exists := removed["added~added-tab"]; exists {
		t.Fatal("removed workspace persisted")
	}
	if _, exists := removed["workspace~tab"]; !exists {
		t.Fatal("removing one workspace affected another")
	}

	if err := os.Chmod(root.WorkspaceStorageDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root.WorkspaceStorageDir, 0o755) })
	denied, records := discoverRecords(t, parser, records)
	if _, exists := denied["workspace~tab"]; !exists {
		t.Fatal("unreadable workspace directory removed prior contribution")
	}
	if err := os.Chmod(root.WorkspaceStorageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(entry.WorkspaceJSONPath); err != nil {
		t.Fatal(err)
	}
	withoutDescriptor, records := discoverRecords(t, parser, records)
	if records[withoutDescriptor["workspace~tab"].Path].WorkspaceRoot != "" {
		t.Fatal("confirmed descriptor removal retained old folder")
	}
}

func TestParserDiscoveryRetriesAfterMetadataRecovery(t *testing.T) {
	root, _, parser := discoveryFixture(t)
	before, records := discoverRecords(t, parser, nil)
	execDiscoveryStatements(t, root.GlobalDBPath,
		`ALTER TABLE composerHeaders RENAME COLUMN value TO hidden_value`,
		`UPDATE cursorDiskKV SET value = '{"composerId":"composer-a","name":"Header renamed","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":1}]}' WHERE key = 'composerData:composer-a'`)
	failed, records := discoverRecords(t, parser, records)
	if !failed["composer-a"].Stamp.Equal(before["composer-a"].Stamp) || records[failed["composer-a"].Path].Title != "Global title" {
		t.Fatal("partial metadata read replaced prior record or stamp")
	}
	execDiscoveryStatements(t, root.GlobalDBPath, `ALTER TABLE composerHeaders RENAME COLUMN hidden_value TO value`)
	recovered, records := discoverRecords(t, parser, records)
	if recovered["composer-a"].Stamp.Equal(before["composer-a"].Stamp) || records[recovered["composer-a"].Path].Title != "Header renamed" {
		t.Fatal("metadata recovery failed to refresh renamed header")
	}
}

func TestParserDiscoveryRefreshesRequestRingAppends(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	_, records := discoverRecords(t, parser, nil)
	match, err := parser.ResolveRequestID(t.Context(), "request-two", conversation.RequestLookupOptions{})
	if err != nil || match.Found {
		t.Fatalf("unexpected initial request match: %+v %v", match, err)
	}
	execDiscoveryStatements(t, entry.StateDBPath, `UPDATE ItemTable SET value = '[{"generationUUID":"request-one","unixMs":1710000000050},{"generationUUID":"request-two","unixMs":1710000000075}]' WHERE key = 'aiService.generations'`)
	execDiscoveryStatements(t, root.GlobalDBPath, `UPDATE cursorDiskKV SET value = '{"_v":3,"bubbleId":"bubble-2","type":2,"text":"last","requestId":"request-two"}' WHERE key = 'bubbleId:composer-a:bubble-2'`)
	// Request lookup is first to read the changed workspace; discovery shares it.
	counter := cursorstore.ObserveDiscoveryReads(t)
	match, err = parser.ResolveRequestID(t.Context(), "request-two", conversation.RequestLookupOptions{})
	if err != nil || !match.Found || match.NativeConversationID != "composer-a" {
		t.Fatalf("appended request missed: %+v %v", match, err)
	}
	discoverRecords(t, parser, records)
	if counts := counter.Take(entry.StateDBPath); counts.Opens != 1 || counts.SelectAuthorizations != 6 {
		t.Fatalf("changed workspace not shared from request to discovery: %+v", counts)
	}
}

func TestDiscoveryResultsDoNotShareMutableSlices(t *testing.T) {
	root, entry, _ := discoveryFixture(t)
	global := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	header := global.Headers["composer-a"]
	header.FullConversationHeadersOnly[0].BubbleID = "mutated"
	global.Headers["composer-a"] = header
	workspace := cursorstore.ReadWorkspaceDiscovery(t.Context(), entry)
	workspace.Legacy.Tabs[0].Bubbles[0].Text = "mutated"
	workspace.Registry.AllComposers[0].Name = "mutated"
	workspace.Generations[0].GenerationUUID = "mutated"
	workspace = cursorstore.ReadWorkspaceDiscovery(t.Context(), entry)
	if workspace.Registry.AllComposers[0].Name != "Workspace title" {
		t.Fatal("caller mutated cached workspace registry")
	}
	parser := cursorparser.New()
	candidates, records := discoverRecords(t, parser, nil)
	if records[candidates["composer-a"].Path].Title != "Global title" {
		t.Fatal("caller mutated cached metadata")
	}
	messages, err := conversation.CollectMessages(parser.Stream(candidates["workspace~tab"].Path, conversation.LoadOptions{}))
	if err != nil || len(messages) != 1 || messages[0].Text != "legacy question" {
		t.Fatalf("caller mutated cached legacy messages: %+v %v", messages, err)
	}
	global = cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if global.Headers["composer-a"].FullConversationHeadersOnly[0].BubbleID != "bubble-1" {
		t.Fatal("caller mutated cached header order")
	}
	lookup, err := cursorstore.FindGenerationEntry(t.Context(), root, "request-one")
	if err != nil || !lookup.Found() {
		t.Fatalf("caller mutated cached generation ring: %+v %v", lookup, err)
	}
}

func TestParserCachedWorkspaceDoesNotWaitForGlobalDiscovery(t *testing.T) {
	root, entry, parser := discoveryFixture(t)
	discoverRecords(t, parser, nil)
	execDiscoveryStatements(t, root.GlobalDBPath, `UPDATE composerHeaders SET value = '{"name":"Active global title"}'`)
	counter := cursorstore.ObserveDiscoveryReads(t)
	entered, release := counter.PauseNextSelect(root.GlobalDBPath)
	var workers sync.WaitGroup
	workers.Go(func() { discoverRecords(t, cursorparser.New(), nil) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("global fixture read never entered")
	}
	requestDone := make(chan conversation.RequestMatch, 1)
	workers.Go(func() {
		match, err := parser.ResolveRequestID(context.Background(), "request-one", conversation.RequestLookupOptions{})
		if err != nil {
			t.Errorf("request failed: %v", err)
		}
		requestDone <- match
	})
	select {
	case match := <-requestDone:
		if !match.Found || match.NativeConversationID != "composer-a" {
			t.Errorf("request = %+v", match)
		}
	case <-time.After(5 * time.Second):
		t.Error("cached workspace request blocked behind an unrelated global discovery read")
	}
	release <- true
	workers.Wait()
	if counts := counter.Take(entry.StateDBPath); counts.Opens != 0 || counts.SelectAuthorizations != 0 {
		t.Fatalf("cached workspace was reread: %+v", counts)
	}
	if counts := counter.Take(root.GlobalDBPath); counts.Opens != 2 {
		t.Fatalf("want one shared discovery read and one targeted request read: %+v", counts)
	}
	candidates, records := discoverRecords(t, parser, nil)
	if records[candidates["composer-a"].Path].Title != "Active global title" {
		t.Fatal("global discovery did not publish its changed result")
	}
	if counts := counter.Take(root.GlobalDBPath); counts.Opens != 0 || counts.SelectAuthorizations != 0 {
		t.Fatalf("global discovery result was not shared: %+v", counts)
	}
}
