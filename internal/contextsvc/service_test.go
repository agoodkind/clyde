package contextsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/api/contextpb"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestGetRecentTurns_returnsTailTruncated(t *testing.T) {
	t.Parallel()

	const workspaceRef = "/Users/x/Sites/repo"
	source := seedConversation(workspaceRef)
	srv := newWithSource(source)

	got, err := srv.GetRecentTurns(context.Background(), &contextpb.GetRecentTurnsRequest{
		WorkspaceRef:    workspaceRef,
		TurnBudget:      2,
		MaxCharsPerTurn: 20,
	})
	if err != nil {
		t.Fatalf("GetRecentTurns returned error: %v", err)
	}
	if len(got.GetTurns()) != 2 {
		t.Fatalf("turns len = %d, want 2", len(got.GetTurns()))
	}
	lastText := got.GetTurns()[1].GetText()
	if len(lastText) > 23 {
		t.Fatalf("last turn text length = %d, want <= 23", len(lastText))
	}
	if !strings.HasSuffix(lastText, "...") {
		t.Fatalf("last turn text = %q, want truncated suffix", lastText)
	}
}

type fakeConversationSource struct {
	records  []conversation.Record
	messages map[string][]transcript.Message
}

func (source *fakeConversationSource) ListPage(
	_ context.Context,
	options conversation.ListOptions,
) (conversation.ListResult, error) {
	return conversation.FilterRecords(source.records, options), nil
}

func (source *fakeConversationSource) Resolve(
	_ context.Context,
	selector string,
) (conversation.Record, error) {
	for _, record := range source.records {
		if record.ID == selector ||
			record.NativeID == selector ||
			record.Title == selector ||
			record.ArtifactPath == selector {
			return record, nil
		}
	}
	return conversation.Record{}, errors.New("conversation not found")
}

func (source *fakeConversationSource) LoadRecentTurns(
	record conversation.Record,
	_ int,
	_ conversation.LoadOptions,
) ([]transcript.Message, error) {
	return source.messages[record.ID], nil
}

func seedConversation(workspaceRef string) *fakeConversationSource {
	newer := seededRecord("claude:newer", workspaceRef, 20)
	older := seededRecord("claude:older", workspaceRef, 10)
	return &fakeConversationSource{
		records: []conversation.Record{newer, older},
		messages: map[string][]transcript.Message{
			newer.ID: {
				seededMessage("user", "first visible turn", 1),
				seededMessage("assistant", "second visible turn", 2),
				seededMessage("system", "internal metadata turn", 3),
				seededMessage("user", "third visible turn", 4),
				seededMessage("assistant", "012345678901234567890123456789", 5),
			},
			older.ID: {
				seededMessage("user", "older turn", 1),
			},
		},
	}
}

func seededRecord(id string, workspaceRef string, updatedAtUnix int64) conversation.Record {
	return conversation.Record{
		ID:            id,
		Provider:      conversation.ProviderClaude,
		NativeID:      id,
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: workspaceRef,
		ArtifactPath:  "/tmp/" + id + ".jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(updatedAtUnix, 0),
		SizeBytes:     10,
		Archived:      false,
	}
}

func seededMessage(role string, text string, timestampUnix int64) transcript.Message {
	return transcript.Message{
		UUID:              role + "-message",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Unix(timestampUnix, 0),
		Text:              text,
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
}
