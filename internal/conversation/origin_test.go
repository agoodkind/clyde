package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// staleIndexServingCache builds an index over a cache file that is already
// written and whose debounce window has not elapsed, so List serves the cached
// records without scanning. It is the reader's view of a cache an older binary
// left behind.
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

// TestListDecodesCachedRecordsWrittenBeforeOriginExisted covers the cache an
// older binary wrote: it carries no version and no origin key, and it must still
// decode and serve rather than fail.
func TestListDecodesCachedRecordsWrittenBeforeOriginExisted(t *testing.T) {
	t.Parallel()
	cacheBody := `{"records":[{"id":"claude:legacy","provider":"claude","native_id":"legacy","title":"written before origin","workspace_root":"/repo","artifact_path":"/repo/legacy.jsonl","artifact_kind":"transcript","model":"","created_at":"2026-05-01T10:00:00Z","updated_at":"2026-05-01T10:00:00Z","size_bytes":10,"archived":false}],"stamps":{}}`

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
	// An unclassified record is not a subagent conversation, so a cache written
	// before origin existed keeps showing everything it always showed.
	if records[0].Origin != OriginUnspecified {
		t.Fatalf("origin = %q, want unspecified", records[0].Origin)
	}
	if records[0].IsSubagent() {
		t.Fatalf("IsSubagent = true, want false for an unclassified record")
	}
}

// TestReadCacheDropsStampsFromAnOlderFormat proves the one-time re-parse that
// fills in origin for a corpus indexed by an older binary: the records still load
// so startup stays fast, but their stamps are dropped so the next scan re-reads
// every artifact instead of reusing records that can never gain an origin.
func TestReadCacheDropsStampsFromAnOlderFormat(t *testing.T) {
	t.Parallel()
	cachePath := filepath.Join(t.TempDir(), cacheFilename)
	cacheBody := `{"records":[{"id":"claude:legacy","provider":"claude","native_id":"legacy","title":"legacy","artifact_path":"/repo/legacy.jsonl","artifact_kind":"transcript"}],"stamps":{"/repo/legacy.jsonl":{"size":10,"mtime":"2026-05-01T10:00:00Z"}}}`
	if err := os.WriteFile(cachePath, []byte(cacheBody), 0o600); err != nil {
		t.Fatalf("write conversation cache: %v", err)
	}

	records, stamps, err := readCache(cachePath)
	if err != nil {
		t.Fatalf("read conversation cache: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want the one cached record", len(records))
	}
	if len(stamps) != 0 {
		t.Fatalf("stamps = %v, want none so the next scan re-parses every artifact", stamps)
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
	if err := writeCache(cachePath, written, nil); err != nil {
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

	roundTripped, stamps, err := readCache(cachePath)
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
