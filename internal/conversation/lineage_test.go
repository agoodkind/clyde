package conversation

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRecordHasLineage(t *testing.T) {
	t.Parallel()

	withoutLineage := testLineageRecord("codex:without-lineage", ProviderCodex)
	if withoutLineage.HasLineage() {
		t.Fatalf("nil lineage HasLineage = true, want false")
	}

	withLineage := testLineageRecord("codex:with-lineage", ProviderCodex)
	withLineage.Lineage = &Lineage{
		Kind:              ConversationLineageKindFork,
		ParentProvider:    ProviderClaude,
		ParentNativeID:    "parent-native",
		ParentMessageUUID: "parent-message",
	}
	if !withLineage.HasLineage() {
		t.Fatalf("fork lineage HasLineage = false, want true")
	}
}

func TestParentConversationID(t *testing.T) {
	t.Parallel()

	noLineage := testLineageRecord("codex:no-lineage", ProviderCodex)
	if id, ok := ParentConversationID(noLineage); ok || id != "" {
		t.Fatalf("nil lineage ParentConversationID = (%q, %v), want (\"\", false)", id, ok)
	}

	emptyParent := testLineageRecord("codex:empty-parent", ProviderCodex)
	emptyParent.Lineage = &Lineage{
		Kind:              ConversationLineageKindFork,
		ParentProvider:    ProviderClaude,
		ParentNativeID:    "",
		ParentMessageUUID: "",
	}
	if id, ok := ParentConversationID(emptyParent); ok || id != "" {
		t.Fatalf("empty parent native id ParentConversationID = (%q, %v), want (\"\", false)", id, ok)
	}

	fork := testLineageRecord("codex:fork", ProviderCodex)
	fork.Lineage = &Lineage{
		Kind:              ConversationLineageKindFork,
		ParentProvider:    ProviderClaude,
		ParentNativeID:    "parent-native",
		ParentMessageUUID: "parent-message",
	}
	want := DerivedID(ProviderClaude, "parent-native", "")
	if id, ok := ParentConversationID(fork); !ok || id != want {
		t.Fatalf("fork ParentConversationID = (%q, %v), want (%q, true)", id, ok, want)
	}
}

func TestResolveForkParent(t *testing.T) {
	t.Parallel()

	parentID := DerivedID(ProviderClaude, "parent-native", "")
	parent := testLineageRecord(parentID, ProviderClaude)
	parent.NativeID = "parent-native"
	idx := testLineageIndex(t, []Record{parent})

	forkPresent := testLineageRecord("codex:fork-present", ProviderCodex)
	forkPresent.Lineage = &Lineage{
		Kind:              ConversationLineageKindFork,
		ParentProvider:    ProviderClaude,
		ParentNativeID:    "parent-native",
		ParentMessageUUID: "parent-message",
	}
	got, ok, err := ResolveForkParent(context.Background(), idx, forkPresent)
	if err != nil {
		t.Fatalf("resolve present fork parent: %v", err)
	}
	if !ok || got.ID != parentID {
		t.Fatalf("present fork parent = (%q, %v), want (%q, true)", got.ID, ok, parentID)
	}

	forkAbsent := testLineageRecord("codex:fork-absent", ProviderCodex)
	forkAbsent.Lineage = &Lineage{
		Kind:              ConversationLineageKindFork,
		ParentProvider:    ProviderClaude,
		ParentNativeID:    "absent-native",
		ParentMessageUUID: "",
	}
	if got, ok, err := ResolveForkParent(context.Background(), idx, forkAbsent); ok || err != nil || got.ID != "" {
		t.Fatalf("absent fork parent = (%q, %v, %v), want (\"\", false, nil)", got.ID, ok, err)
	}

	spawn := testLineageRecord("codex:spawn", ProviderCodex)
	spawn.Lineage = &Lineage{
		Kind:              ConversationLineageKindSpawn,
		ParentProvider:    ProviderClaude,
		ParentNativeID:    "parent-native",
		ParentMessageUUID: "",
	}
	if got, ok, err := ResolveForkParent(context.Background(), idx, spawn); ok || err != nil || got.ID != "" {
		t.Fatalf("spawn parent = (%q, %v, %v), want (\"\", false, nil)", got.ID, ok, err)
	}

	noLineage := testLineageRecord("codex:no-lineage", ProviderCodex)
	if got, ok, err := ResolveForkParent(context.Background(), idx, noLineage); ok || err != nil || got.ID != "" {
		t.Fatalf("nil lineage parent = (%q, %v, %v), want (\"\", false, nil)", got.ID, ok, err)
	}

	if got, ok, err := ResolveForkParent(context.Background(), nil, forkPresent); ok || err != nil || got.ID != "" {
		t.Fatalf("nil index parent = (%q, %v, %v), want (\"\", false, nil)", got.ID, ok, err)
	}
}

// testLineageIndex builds an index preloaded with records for snapshot lookups,
// with a no-op scanner and a long debounce so no background refresh runs during
// the test.
func testLineageIndex(t *testing.T, records []Record) *Index {
	t.Helper()
	return &Index{
		mu:          sync.Mutex{},
		registry:    NewRegistry(),
		records:     records,
		prevRecords: nil,
		prevStamps:  nil,
		loaded:      true,
		refreshing:  false,
		lastRefresh: time.Now(),
		cachePath:   filepath.Join(t.TempDir(), cacheFilename),
		debounce:    time.Hour,
		scanProvider: func(context.Context, *Registry, scanCache) (scanResult, error) {
			return scanResult{records: nil, stamps: nil}, nil
		},
	}
}

func testLineageRecord(id string, provider Provider) Record {
	return Record{
		ID:            id,
		Provider:      provider,
		NativeID:      id,
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/" + id + ".jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}
}
