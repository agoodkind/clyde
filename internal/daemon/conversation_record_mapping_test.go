package daemon

import (
	"reflect"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
)

func TestConversationRecordProtoRoundTripCarriesLineage(t *testing.T) {
	t.Parallel()

	withLineage := testDaemonConversationRecord("codex:child", conversation.ProviderCodex)
	withLineage.Lineage = &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindFork,
		ParentProvider:    conversation.ProviderClaude,
		ParentNativeID:    "parent-native",
		ParentMessageUUID: "parent-message",
	}

	withLineageWire := protoConversationRecord(withLineage)
	if withLineageWire.GetLineage() == nil {
		t.Fatalf("wire lineage = nil, want fork lineage")
	}
	if withLineageWire.GetLineage().GetParentProvider() != protoProvider(conversation.ProviderClaude) {
		t.Fatalf(
			"wire lineage parent provider = %v, want %v",
			withLineageWire.GetLineage().GetParentProvider(),
			protoProvider(conversation.ProviderClaude),
		)
	}

	withLineageRoundTrip := conversationRecordFromProto(withLineageWire)
	if !reflect.DeepEqual(withLineageRoundTrip, withLineage) {
		t.Fatalf("round-trip record = %#v, want %#v", withLineageRoundTrip, withLineage)
	}

	withoutLineage := testDaemonConversationRecord("codex:without-lineage", conversation.ProviderCodex)
	withoutLineageWire := protoConversationRecord(withoutLineage)
	if withoutLineageWire.GetLineage() != nil {
		t.Fatalf("wire lineage = %#v, want nil", withoutLineageWire.GetLineage())
	}

	withoutLineageRoundTrip := conversationRecordFromProto(withoutLineageWire)
	if withoutLineageRoundTrip.Lineage != nil {
		t.Fatalf("round-trip lineage = %#v, want nil", withoutLineageRoundTrip.Lineage)
	}
	if !reflect.DeepEqual(withoutLineageRoundTrip, withoutLineage) {
		t.Fatalf("round-trip record = %#v, want %#v", withoutLineageRoundTrip, withoutLineage)
	}
}

func testDaemonConversationRecord(id string, provider conversation.Provider) conversation.Record {
	return conversation.Record{
		ID:            id,
		Provider:      provider,
		NativeID:      id + "-native",
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/" + id + ".jsonl",
		ArtifactKind:  "rollout",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}
}
