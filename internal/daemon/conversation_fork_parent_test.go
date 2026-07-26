package daemon

import (
	"context"
	"iter"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// fakeForkParentParser surfaces a single fixed parent record through the
// conversation scan so a real *conversation.Index can be seeded deterministically
// via Refresh, without reading real provider transcripts from disk.
type fakeForkParentParser struct {
	provider conversation.Provider
	path     string
	record   conversation.Record
}

func (p fakeForkParentParser) Provider() conversation.Provider {
	return p.provider
}

func (p fakeForkParentParser) Discover(context.Context, map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	return []conversation.ScanCandidate{
		{Path: p.path, Stamp: conversation.FileStamp{Size: 1, Mtime: time.Unix(1, 0)}},
	}, nil
}

func (p fakeForkParentParser) ScanRecord(string, conversation.FileStamp) (conversation.Record, bool) {
	return p.record, true
}

func (p fakeForkParentParser) Stream(string, conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

func TestProtoConversationRecordSetsForkParent(t *testing.T) {
	// Refresh writes the index cache, so point XDG_CACHE_HOME at a temp dir to
	// keep the real user cache untouched. t.Setenv forbids t.Parallel here.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	parentID := conversation.DerivedID(conversation.ProviderCodex, "parent-thread", "")
	parent := conversation.Record{
		ID:            parentID,
		Provider:      conversation.ProviderCodex,
		NativeID:      "parent-thread",
		Lineage:       nil,
		Title:         "parent",
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/parent.jsonl",
		ArtifactKind:  "rollout",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}

	registry := conversation.NewRegistry()
	registry.Register(fakeForkParentParser{
		provider: conversation.ProviderCodex,
		path:     parent.ArtifactPath,
		record:   parent,
	})
	idx := conversation.NewIndex(registry, config.ConversationConfig{})
	if err := idx.Refresh(context.Background()); err != nil {
		t.Fatalf("seed index via refresh: %v", err)
	}
	if got, ok := idx.RecordByID(parentID); !ok || got.ID != parentID {
		t.Fatalf("seeded parent lookup = (%q, %v), want (%q, true)", got.ID, ok, parentID)
	}

	forkChild := daemonTestRecord("codex:fork-child", false)
	forkChild.Lineage = &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindFork,
		ParentProvider:    conversation.ProviderCodex,
		ParentNativeID:    "parent-thread",
		ParentMessageUUID: "msg",
	}
	forkWire := protoConversationRecord(context.Background(), idx, forkChild)
	if forkWire.GetParentConversationId() != parentID {
		t.Fatalf("fork parent_conversation_id = %q, want %q", forkWire.GetParentConversationId(), parentID)
	}

	spawnChild := daemonTestRecord("codex:spawn-child", false)
	spawnChild.Lineage = &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindSpawn,
		ParentProvider:    conversation.ProviderCodex,
		ParentNativeID:    "parent-thread",
		ParentMessageUUID: "",
	}
	if spawnWire := protoConversationRecord(context.Background(), idx, spawnChild); spawnWire.GetParentConversationId() != "" {
		t.Fatalf("spawn parent_conversation_id = %q, want empty", spawnWire.GetParentConversationId())
	}

	missingChild := daemonTestRecord("codex:missing-parent", false)
	missingChild.Lineage = &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindFork,
		ParentProvider:    conversation.ProviderCodex,
		ParentNativeID:    "absent-thread",
		ParentMessageUUID: "",
	}
	if missingWire := protoConversationRecord(context.Background(), idx, missingChild); missingWire.GetParentConversationId() != "" {
		t.Fatalf("missing-parent parent_conversation_id = %q, want empty", missingWire.GetParentConversationId())
	}
}
