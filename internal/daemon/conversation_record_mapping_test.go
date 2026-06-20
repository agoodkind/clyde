package daemon

import (
	"context"
	"reflect"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func TestConversationRecordProtoRoundTripCarriesLineage(t *testing.T) {
	t.Parallel()

	// An empty index resolves no fork parent, so parent_conversation_id stays
	// empty and the round trip exercises only the lineage mapping.
	idx := conversation.NewIndex(conversation.NewRegistry())

	withLineage := testDaemonConversationRecord("codex:child", conversation.ProviderCodex)
	withLineage.Lineage = &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindFork,
		ParentProvider:    conversation.ProviderClaude,
		ParentNativeID:    "parent-native",
		ParentMessageUUID: "parent-message",
	}

	withLineageWire := protoConversationRecord(context.Background(), idx, withLineage)
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
	withoutLineageWire := protoConversationRecord(context.Background(), idx, withoutLineage)
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

func TestConversationInfoProtoMappingCarriesStatsAndSegments(t *testing.T) {
	t.Parallel()
	idx := conversation.NewIndex(conversation.NewRegistry())
	record := testDaemonConversationRecord("claude:info", conversation.ProviderClaude)
	info := conversation.Info{
		Record: record,
		Stats: conversation.Stats{
			TotalMessages:     8,
			VisibleMessages:   4,
			UserMessages:      2,
			AssistantMessages: 2,
			SystemMessages:    4,
			ToolCallCount:     3,
			ToolOutputCount:   1,
		},
		CompactionCount: 2,
		Segments: []conversation.CompactionSegment{{
			Index:               0,
			StartMessageIndex:   4,
			EndMessageIndex:     8,
			HasStartingSummary:  true,
			SummaryMessageIndex: 4,
			SummaryUUID:         "summary-0",
			SummaryTimestamp:    time.Unix(10, 0),
			VisibleMessageCount: 3,
			ToolCallCount:       2,
			Checkpoint: conversation.CompactionCheckpoint{
				BoundaryIndex:           -1,
				BoundaryUUID:            "",
				SummaryIndex:            4,
				SummaryUUID:             "summary-0",
				ContextItems:            nil,
				Trigger:                 "",
				HeadUUID:                "",
				AnchorUUID:              "",
				TailUUID:                "",
				MessagesSummarized:      0,
				ReplacementHistoryCount: 0,
			},
		}},
	}

	wire := protoConversationInfo(context.Background(), idx, info)

	if wire.GetConversation().GetId() != record.ID {
		t.Fatalf("conversation id = %q, want %q", wire.GetConversation().GetId(), record.ID)
	}
	if wire.GetStats().GetTotalMessages() != 8 {
		t.Fatalf("total messages = %d, want 8", wire.GetStats().GetTotalMessages())
	}
	if wire.GetCompactionCount() != 2 {
		t.Fatalf("compaction count = %d, want 2", wire.GetCompactionCount())
	}
	if len(wire.GetSegments()) != 1 {
		t.Fatalf("segments = %d, want 1", len(wire.GetSegments()))
	}
	segment := wire.GetSegments()[0]
	if segment.GetExportSelector() != "0" {
		t.Fatalf("export selector = %q, want 0", segment.GetExportSelector())
	}
	if segment.GetSummaryTimestampUnix() != 10 {
		t.Fatalf("summary timestamp = %d, want 10", segment.GetSummaryTimestampUnix())
	}

	roundTrip := conversationInfoFromProto(&clydev1.GetConversationInfoResponse{
		Conversation:    wire.GetConversation(),
		Stats:           wire.GetStats(),
		CompactionCount: wire.GetCompactionCount(),
		Segments:        wire.GetSegments(),
	})
	if roundTrip.Record.ID != info.Record.ID {
		t.Fatalf("round-trip record id = %q, want %q", roundTrip.Record.ID, info.Record.ID)
	}
	if roundTrip.Stats.ToolCallCount != info.Stats.ToolCallCount {
		t.Fatalf("round-trip tool calls = %d, want %d", roundTrip.Stats.ToolCallCount, info.Stats.ToolCallCount)
	}
	if len(roundTrip.Segments) != 1 || roundTrip.Segments[0].SummaryUUID != "summary-0" {
		t.Fatalf("round-trip segments = %#v", roundTrip.Segments)
	}
}
