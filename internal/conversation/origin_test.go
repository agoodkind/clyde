package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// staleIndexServingCache serves a written cache through the public read boundary.
func staleIndexServingCache(t *testing.T, cacheBody string, includeSubagents bool) *Index {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), cacheFilename)
	if err := os.WriteFile(cachePath, []byte(cacheBody), 0o600); err != nil {
		t.Fatalf("write conversation cache: %v", err)
	}
	return &Index{
		mu:               sync.Mutex{},
		includeSubagents: includeSubagents,
		registry:         NewRegistry(),
		records:          nil,
		prevRecords:      nil,
		prevStamps:       nil,
		loaded:           false,
		refreshing:       false,
		lastRefresh:      time.Now(),
		cachePath:        cachePath,
		debounce:         time.Hour,
		scanProvider: func(context.Context, *Registry, scanCache) (scanResult, error) {
			t.Error("scan ran; the test asserts what the cached records decode to")
			return scanResult{records: nil, stamps: nil}, nil
		},
	}
}

func TestListKeepsUnclassifiedCachedRecordsVisible(t *testing.T) {
	t.Parallel()
	cacheBody := fmt.Sprintf(`{"version":%d,"records":[{"id":"claude:legacy","provider":"claude","native_id":"legacy","title":"unclassified","workspace_root":"/repo","artifact_path":"/repo/legacy.jsonl","artifact_kind":"transcript","model":"","created_at":"2026-05-01T10:00:00Z","updated_at":"2026-05-01T10:00:00Z","size_bytes":10,"archived":false}],"stamps":{}}`, cacheFormatVersion)

	idx := staleIndexServingCache(t, cacheBody, false)

	records, err := idx.List(context.Background())
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want the one cached record", len(records))
	}
	if records[0].ID != "claude:legacy" {
		t.Fatalf("id = %q, want claude:legacy", records[0].ID)
	}
	// An unclassified record remains visible when the subagent filter is enabled.
	if records[0].Origin != OriginUnspecified {
		t.Fatalf("origin = %q, want unspecified", records[0].Origin)
	}
	if records[0].IsSubagent() {
		t.Fatalf("IsSubagent = true, want false for an unclassified record")
	}
}

// TestWrittenCacheRoundTripsOriginAndVersion proves a cache this binary writes
// carries both the origin of each conversation and the format version, which is
// what lets the setting be flipped without deleting the cache.
func TestWrittenCacheRoundTripsOriginAndVersion(t *testing.T) {
	t.Parallel()
	cachePath := filepath.Join(t.TempDir(), cacheFilename)
	written := []Record{{
		ID:            "claude:agent",
		Provider:      ProviderClaude,
		NativeID:      "agent",
		Lineage:       nil,
		Origin:        OriginSubagent,
		Title:         "dispatched work",
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/repo/agent.jsonl",
		ArtifactKind:  "transcript",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}}
	if err := writeCache(cachePath, written, nil, nil); err != nil {
		t.Fatalf("write conversation cache: %v", err)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read conversation cache: %v", err)
	}
	var cache cacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("decode conversation cache: %v", err)
	}
	if cache.Version != cacheFormatVersion {
		t.Fatalf("version = %d, want %d", cache.Version, cacheFormatVersion)
	}
	if len(cache.Records) != 1 || cache.Records[0].Origin != OriginSubagent {
		t.Fatalf("cached records = %#v, want one subagent-origin record", cache.Records)
	}

	roundTripped, stamps, _, err := readCache(cachePath)
	if err != nil {
		t.Fatalf("re-read conversation cache: %v", err)
	}
	if len(stamps) != 0 {
		t.Fatalf("stamps = %v, want none; none were written", stamps)
	}
	if len(roundTripped) != 1 || !roundTripped[0].IsSubagent() {
		t.Fatalf("re-read records = %#v, want the subagent origin preserved", roundTripped)
	}
}

func TestReadCacheRejectsUnsupportedFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation-index.json")
	body := `{"version":1,"records":[{"id":"cursor:composer-a","provider":"cursor","native_id":"composer-a","artifact_path":"cursor://root/composer/composer-a"}],"stamps":{"cursor://root/composer/composer-a":{"size":3,"mtime":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	records, stamps, _, err := readCache(path)
	if err == nil || len(records) != 0 || len(stamps) != 0 {
		t.Fatalf("unsupported cache returned records=%v stamps=%v err=%v", records, stamps, err)
	}
}
