package conversation

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestListUsesCacheAndDoesNotForceDebouncedScan(t *testing.T) {
	t.Parallel()

	scanCalls := 0
	idx := &Index{
		mu:          sync.Mutex{},
		records:     nil,
		loaded:      false,
		refreshing:  false,
		lastRefresh: time.Now(),
		cachePath:   filepath.Join(t.TempDir(), cacheFilename),
		debounce:    time.Hour,
		scanProvider: func(context.Context) ([]Record, error) {
			scanCalls++
			return []Record{{
				ID:            "claude:cached",
				Provider:      ProviderClaude,
				NativeID:      "cached",
				Title:         "cached",
				WorkspaceRoot: "",
				ArtifactPath:  "/tmp/cached.jsonl",
				ArtifactKind:  artifactKindTranscript,
				Model:         "",
				CreatedAt:     time.Time{},
				UpdatedAt:     time.Time{},
				SizeBytes:     0,
				Archived:      false,
			}}, nil
		},
	}

	records, err := idx.List(context.Background())
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %d, want empty cold cache result", len(records))
	}
	if scanCalls != 0 {
		t.Fatalf("scan calls after List = %d, want no synchronous scan", scanCalls)
	}
}
