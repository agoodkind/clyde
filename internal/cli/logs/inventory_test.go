package logs

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBuildInventoryCategorizesKnownLogFamilies(t *testing.T) {
	root := t.TempDir()
	baseTime := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	files := map[string]int64{
		"mitm/always-on/capture.jsonl":                                    100,
		"mitm/always-on/raw/example/20260506-request.raw":                 200,
		"mitm-launcher/codex-desktop.log":                                 300,
		"mitm/always-on/profiles/cursor/isolated/logs/run/main.log":       400,
		"clyde-daemon.jsonl":                                              500,
		"clyde-daemon-2026-05-06T01-02-03.000.jsonl.gz":                   600,
		"logs/adapter/http/ingress.jsonl":                                 700,
		"logs/adapter/http/ingress-2026-05-06T01-02-03.000.jsonl.gz":      800,
		"logs/chats/chat-key.jsonl":                                       900,
		"sessions/session-id/backups/ledger.jsonl":                        1000,
		"manual-backups/resume-repair-20260505-1740/session/ledger.jsonl": 1100,
		"clyde-daemon.jsonl.pre-repair-20260425T082405":                   1200,
		"logs/adapter/http/ingress.jsonl.lock":                            1300,
		"adapter/anthropic/device_id":                                     1400,
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
	})
	if err != nil {
		t.Fatalf("buildInventory returned error: %v", err)
	}
	assertCategory(t, currentInventory, categoryMITMCaptureIndexes, 1, 100)
	assertCategory(t, currentInventory, categoryMITMRawCaptures, 1, 200)
	assertCategory(t, currentInventory, categoryMITMProfileProcessLogs, 2, 700)
	assertCategory(t, currentInventory, categoryTopLevelProcessLogs, 2, 1100)
	assertCategory(t, currentInventory, categoryConcernLogs, 2, 1500)
	assertCategory(t, currentInventory, categoryPerChatTranscriptLogs, 1, 900)
	assertCategory(t, currentInventory, categorySessionBackupArtifacts, 2, 2100)
	assertCategory(t, currentInventory, categoryPreRepairRetainedLogs, 1, 1200)
	assertCategory(t, currentInventory, categoryLockFiles, 1, 1300)
	assertCategory(t, currentInventory, categoryUncategorizedLogs, 1, 1400)
}

func assertCategory(t *testing.T, currentInventory inventory, currentCategory category, wantCount int, wantBytes int64) {
	t.Helper()
	for _, summary := range currentInventory.Categories {
		if summary.Category != currentCategory {
			continue
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
