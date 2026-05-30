package logs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildInventoryCategorizesKnownLogFamilies(t *testing.T) {
	root := t.TempDir()
	baseTime := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	files := map[string]int64{
		"mitm/raw/example/20260506-request.raw":                      200,
		"mitm-launcher/codex-desktop.log":                            300,
		"mitm/profiles/cursor/isolated/logs/run/main.log":            400,
		"clyde-daemon.jsonl":                                         500,
		"clyde-daemon-2026-05-06T01-02-03.000.jsonl.gz":              600,
		"logs/adapter/http/ingress.jsonl":                            700,
		"logs/adapter/http/ingress-2026-05-06T01-02-03.000.jsonl.gz": 800,
		"logs/chats/chat-key.jsonl":                                  900,
		"clyde-daemon.jsonl.pre-repair-20260425T082405":              1200,
		"logs/adapter/http/ingress.jsonl.lock":                       1300,
		"adapter/anthropic/device_id":                                1400,
		"codex.jsonl":                                                1500,
		"logs/inventory/events.jsonl":                                1600,
	}
	index := 0
	for relativePath, size := range files {
		writeSizedFile(t, root, relativePath, size)
		modified := baseTime.Add(time.Duration(index) * time.Minute)
		if err := chtimes(filepath.Join(root, relativePath), modified); err != nil {
			t.Fatalf("set mtime for %s: %v", relativePath, err)
		}
		index++
	}
	currentInventory, err := buildInventory(inventoryOptions{
		StateRoot:        root,
		LargestFileLimit: 2,
		Now:              baseTime,
		Mode:             inventoryModeDeep,
	})
	if err != nil {
		t.Fatalf("buildInventory returned error: %v", err)
	}
	assertCategory(t, currentInventory, categoryMITMRawCaptures, 1, 200)
	assertCategorySource(t, currentInventory, categoryMITMRawCaptures, inventorySourceDeepScan)
	assertCategory(t, currentInventory, categoryMITMProfileProcessLogs, 2, 700)
	assertCategory(t, currentInventory, categoryTopLevelProcessLogs, 2, 1100)
	assertCategory(t, currentInventory, categoryProviderSidecarLogs, 1, 1500)
	assertCategory(t, currentInventory, categoryConcernLogs, 2, 1500)
	assertCategory(t, currentInventory, categoryPerChatTranscriptLogs, 1, 900)
	assertCategory(t, currentInventory, categoryInventoryIndexes, 1, 1600)
	assertCategory(t, currentInventory, categoryPreRepairRetainedLogs, 1, 1200)
	assertCategory(t, currentInventory, categoryLockFiles, 1, 1300)
	assertCategory(t, currentInventory, categoryUncategorizedLogs, 1, 1400)
}

func TestBuildInventoryUsesIndexedLocationsByDefault(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, root, "logs/inventory/events.jsonl", 25)
	writeSizedFile(t, root, "mitm/raw/example/20260506-request.raw", 50)

	currentInventory, err := buildInventory(inventoryOptions{StateRoot: root})
	if err != nil {
		t.Fatalf("buildInventory returned error: %v", err)
	}
	if currentInventory.Mode != inventoryModeIndexed {
		t.Fatalf("mode=%q want indexed", currentInventory.Mode)
	}
	// Indexed mode does a single stat per known location, so the MITM raw
	// captures location is the mitm/raw directory rather than the leaf files
	// inside it. The directory inode size is filesystem-dependent, so the
	// expected byte total is the same os.Stat the production indexer uses
	// rather than a hard-coded literal.
	rawDirInfo, err := os.Stat(filepath.Join(root, "mitm", "raw"))
	if err != nil {
		t.Fatalf("stat mitm/raw: %v", err)
	}
	assertCategorySource(t, currentInventory, categoryMITMRawCaptures, inventorySourceIndexed)
	assertCategorySource(t, currentInventory, categoryInventoryIndexes, inventorySourceIndexed)
	assertCategory(t, currentInventory, categoryMITMRawCaptures, 1, rawDirInfo.Size())
	assertCategory(t, currentInventory, categoryInventoryIndexes, 1, 25)
}

func assertCategory(t *testing.T, currentInventory inventory, currentCategory category, wantCount int, wantBytes int64) {
	t.Helper()
	for _, summary := range currentInventory.Categories {
		if summary.Category != currentCategory {
			continue
		}
		if summary.Sink == "" {
			t.Fatalf("%s sink is empty", currentCategory)
		}
		if !summary.CleanupEnabled {
			t.Fatalf("%s cleanup disabled", currentCategory)
		}
		if summary.Count != wantCount {
			t.Fatalf("%s count=%d want %d", currentCategory, summary.Count, wantCount)
		}
		if summary.TotalBytes != wantBytes {
			t.Fatalf("%s total=%d want %d", currentCategory, summary.TotalBytes, wantBytes)
		}
		return
	}
	t.Fatalf("missing category %s", currentCategory)
}

func assertCategorySource(t *testing.T, currentInventory inventory, currentCategory category, wantSource inventorySource) {
	t.Helper()
	for _, summary := range currentInventory.Categories {
		if summary.Category != currentCategory {
			continue
		}
		if summary.Source != wantSource {
			t.Fatalf("%s source=%q want %q", currentCategory, summary.Source, wantSource)
		}
		return
	}
	t.Fatalf("missing category %s", currentCategory)
}

func TestBuildInventoryIndexedModeSurfacesIndexSnapshotFields(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, root, "mitm/raw/example/20260506-request.raw", 64)
	writeSizedFile(t, root, "logs/inventory/events.jsonl", 0)
	indexPath := filepath.Join(root, "logs/inventory/events.jsonl")
	cleanupTime := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	rawLegTime := cleanupTime.Add(time.Minute)
	concernLegTime := cleanupTime.Add(2 * time.Minute)
	contents := strings.Join([]string{
		`{"time":"` + cleanupTime.Format(time.RFC3339Nano) + `","msg":"slogger.cleanup.completed","sinks":["inventory_index"],"root":"` + root + `","scanned_roots":["` + root + `"],"candidates":3,"deleted":2,"bytes_deleted":42,"skipped":["/skip/a"],"errors":["boom"],"duration_ms":7}`,
		`{"time":"` + rawLegTime.Format(time.RFC3339Nano) + `","msg":"logging.request.leg","sinks":["mitm_raw","inventory_index"],"request_id":"req-raw-1"}`,
		`{"time":"` + concernLegTime.Format(time.RFC3339Nano) + `","msg":"logging.request.leg","sinks":["concern","inventory_index"],"request_id":"req-concern-9"}`,
		``,
	}, "\n")
	if err := os.WriteFile(indexPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write inventory index: %v", err)
	}

	currentInventory, err := buildInventory(inventoryOptions{StateRoot: root})
	if err != nil {
		t.Fatalf("buildInventory returned error: %v", err)
	}

	if currentInventory.Mode != inventoryModeIndexed {
		t.Fatalf("mode=%q want indexed", currentInventory.Mode)
	}

	rawSummary := findCategorySummary(t, currentInventory, categoryMITMRawCaptures)
	if !rawSummary.LastEventTimestamp.Equal(rawLegTime) {
		t.Fatalf("raw last_event_timestamp=%v want %v", rawSummary.LastEventTimestamp, rawLegTime)
	}
	if rawSummary.LastEventRequestID != "req-raw-1" {
		t.Fatalf("raw last_event_request_id=%q want req-raw-1", rawSummary.LastEventRequestID)
	}
	if rawSummary.LastCleanupResult == nil {
		t.Fatalf("raw last_cleanup_result is nil")
	}
	if rawSummary.LastCleanupResult.Deleted != 2 {
		t.Fatalf("raw cleanup deleted=%d want 2", rawSummary.LastCleanupResult.Deleted)
	}
	if rawSummary.LastCleanupResult.BytesDeleted != 42 {
		t.Fatalf("raw cleanup bytes_deleted=%d want 42", rawSummary.LastCleanupResult.BytesDeleted)
	}
	if rawSummary.LastCleanupResult.Candidates != 3 {
		t.Fatalf("raw cleanup candidates=%d want 3", rawSummary.LastCleanupResult.Candidates)
	}
	if rawSummary.LastCleanupResult.DurationMS != 7 {
		t.Fatalf("raw cleanup duration_ms=%d want 7", rawSummary.LastCleanupResult.DurationMS)
	}
	if len(rawSummary.LastCleanupResult.Skipped) != 1 || rawSummary.LastCleanupResult.Skipped[0] != "/skip/a" {
		t.Fatalf("raw cleanup skipped=%v want [/skip/a]", rawSummary.LastCleanupResult.Skipped)
	}
	if len(rawSummary.LastCleanupResult.Errors) != 1 || rawSummary.LastCleanupResult.Errors[0] != "boom" {
		t.Fatalf("raw cleanup errors=%v want [boom]", rawSummary.LastCleanupResult.Errors)
	}

	concernSummary := findCategorySummary(t, currentInventory, categoryConcernLogs)
	if !concernSummary.LastEventTimestamp.Equal(concernLegTime) {
		t.Fatalf("concern last_event_timestamp=%v want %v", concernSummary.LastEventTimestamp, concernLegTime)
	}
	if concernSummary.LastEventRequestID != "req-concern-9" {
		t.Fatalf("concern last_event_request_id=%q want req-concern-9", concernSummary.LastEventRequestID)
	}

	lockSummary := findCategorySummary(t, currentInventory, categoryLockFiles)
	if lockSummary.LastCleanupResult != nil {
		t.Fatalf("lock category should not surface cleanup result, got %+v", lockSummary.LastCleanupResult)
	}
}

func TestBuildInventoryIndexedModeMissingIndexFileLeavesSnapshotEmpty(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, root, "mitm/raw/example/20260506-request.raw", 16)

	currentInventory, err := buildInventory(inventoryOptions{StateRoot: root})
	if err != nil {
		t.Fatalf("buildInventory returned error: %v", err)
	}

	rawSummary := findCategorySummary(t, currentInventory, categoryMITMRawCaptures)
	if !rawSummary.LastEventTimestamp.IsZero() {
		t.Fatalf("expected zero last_event_timestamp, got %v", rawSummary.LastEventTimestamp)
	}
	if rawSummary.LastEventRequestID != "" {
		t.Fatalf("expected empty last_event_request_id, got %q", rawSummary.LastEventRequestID)
	}
	if rawSummary.LastCleanupResult != nil {
		t.Fatalf("expected nil last_cleanup_result, got %+v", rawSummary.LastCleanupResult)
	}
}

func TestBuildInventoryDeepModeIgnoresIndexSnapshotMetadata(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, root, "mitm/raw/example/20260506-request.raw", 64)
	writeSizedFile(t, root, "logs/inventory/events.jsonl", 0)
	indexPath := filepath.Join(root, "logs/inventory/events.jsonl")
	cleanupTime := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	contents := strings.Join([]string{
		`{"time":"` + cleanupTime.Format(time.RFC3339Nano) + `","msg":"slogger.cleanup.completed","sinks":["inventory_index"],"root":"` + root + `","scanned_roots":["` + root + `"],"candidates":3,"deleted":2,"bytes_deleted":42,"skipped":["/skip/a"],"errors":["boom"],"duration_ms":7}`,
		``,
	}, "\n")
	if err := os.WriteFile(indexPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write inventory index: %v", err)
	}

	currentInventory, err := buildInventory(inventoryOptions{StateRoot: root, Mode: inventoryModeDeep})
	if err != nil {
		t.Fatalf("buildInventory returned error: %v", err)
	}
	if currentInventory.Mode != inventoryModeDeep {
		t.Fatalf("mode=%q want deep", currentInventory.Mode)
	}
	rawSummary := findCategorySummary(t, currentInventory, categoryMITMRawCaptures)
	if !rawSummary.LastEventTimestamp.IsZero() {
		t.Fatalf("deep mode should ignore snapshot last_event_timestamp, got %v", rawSummary.LastEventTimestamp)
	}
	if rawSummary.LastEventRequestID != "" {
		t.Fatalf("deep mode should ignore snapshot last_event_request_id, got %q", rawSummary.LastEventRequestID)
	}
	if rawSummary.LastCleanupResult != nil {
		t.Fatalf("deep mode should ignore snapshot last_cleanup_result, got %+v", rawSummary.LastCleanupResult)
	}
}

func findCategorySummary(t *testing.T, currentInventory inventory, currentCategory category) categorySummary {
	t.Helper()
	for _, summary := range currentInventory.Categories {
		if summary.Category == currentCategory {
			return summary
		}
	}
	t.Fatalf("missing category %s", currentCategory)
	return categorySummary{}
}
