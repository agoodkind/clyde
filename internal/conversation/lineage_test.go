package conversation

import (
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
