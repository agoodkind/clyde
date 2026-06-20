package conversation

import "testing"

func TestConversationInfoCountsMessagesToolsAndSegments(t *testing.T) {
	t.Parallel()
	idx, record, _ := newCompactionExportIndex()

	info, err := idx.ConversationInfo(record)
	if err != nil {
		t.Fatalf("ConversationInfo: %v", err)
	}

	if info.Record.ID != record.ID {
		t.Fatalf("record id = %q, want %q", info.Record.ID, record.ID)
	}
	if info.Stats.TotalMessages != 8 {
		t.Fatalf("total messages = %d, want 8", info.Stats.TotalMessages)
	}
	if info.Stats.VisibleMessages != 3 {
		t.Fatalf("visible messages = %d, want 3", info.Stats.VisibleMessages)
	}
	if info.Stats.SystemMessages != 3 {
		t.Fatalf("system messages = %d, want 3", info.Stats.SystemMessages)
	}
	if info.CompactionCount != 3 {
		t.Fatalf("compaction count = %d, want 3", info.CompactionCount)
	}
	if len(info.Segments) != 4 {
		t.Fatalf("segments = %d, want 4", len(info.Segments))
	}
	if info.Segments[0].Index != 0 || info.Segments[0].SummaryUUID != "summary-3" {
		t.Fatalf("latest segment = %#v, want segment 0 summary-3", info.Segments[0])
	}
}

func TestConversationInfoCountsToolCallsAndOutputs(t *testing.T) {
	t.Parallel()
	seen := []LoadOptions{}
	idx, record := newToolAwareIndex(&seen)

	info, err := idx.ConversationInfo(record)
	if err != nil {
		t.Fatalf("ConversationInfo: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("load calls = %d, want 1", len(seen))
	}
	if !seen[0].IncludeToolOutputs {
		t.Fatal("ConversationInfo did not load tool outputs")
	}
	if info.Stats.ToolCallCount != 1 {
		t.Fatalf("tool call count = %d, want 1", info.Stats.ToolCallCount)
	}
	if info.Stats.ToolOutputCount != 1 {
		t.Fatalf("tool output count = %d, want 1", info.Stats.ToolOutputCount)
	}
}
