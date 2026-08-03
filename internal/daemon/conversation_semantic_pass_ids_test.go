package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// TestPassLogNamesDeliveredConversations proves a delivering pass writes the
// delivered conversation ids on its pass_completed line, so a batch in the log
// attributes to specific conversations instead of a bare count. The hourly
// re-offer diagnosis (CLYDE-640) was blocked on exactly this absence.
func TestPassLogNamesDeliveredConversations(t *testing.T) {
	firstID := "codex:pass-ids-a"
	secondID := "codex:pass-ids-b"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{
			{Record: semanticTestRecord(firstID), Stamp: semanticTestStamp(20, 200)},
			{Record: semanticTestRecord(secondID), Stamp: semanticTestStamp(30, 300)},
		},
		messagesByID: map[string][]transcript.Message{
			firstID:  {{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "alpha"}},
			secondID: {{Role: "user", Timestamp: time.Unix(1710000100, 0), Text: "beta"}},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{firstID, secondID}}
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", logger, semanticTestContentKinds())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	var loggedIDs []string
	for _, line := range strings.Split(logBuffer.String(), "\n") {
		if !strings.Contains(line, "pass_completed") {
			continue
		}
		var record struct {
			SentConversationIDs []string `json:"sent_conversation_ids"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("unmarshal pass log line: %v", err)
		}
		loggedIDs = record.SentConversationIDs
	}
	if len(loggedIDs) != 2 {
		t.Fatalf("sent_conversation_ids = %v, want both delivered ids", loggedIDs)
	}
	logged := map[string]bool{loggedIDs[0]: true, loggedIDs[1]: true}
	if !logged[firstID] || !logged[secondID] {
		t.Fatalf("sent_conversation_ids = %v, want %q and %q", loggedIDs, firstID, secondID)
	}
}

// TestBoundedConversationIDsCapsTheLogLine pins the bound: a backlog pass
// delivering hundreds of conversations logs only the batch head, and the
// existing sent_conversations count carries the total.
func TestBoundedConversationIDsCapsTheLogLine(t *testing.T) {
	t.Parallel()
	ids := make([]string, 0, maxLoggedConversationIDs+5)
	for i := 0; i < maxLoggedConversationIDs+5; i++ {
		ids = append(ids, string(rune('a'+i)))
	}
	bounded := boundedConversationIDs(ids)
	if len(bounded) != maxLoggedConversationIDs {
		t.Fatalf("bounded length = %d, want %d", len(bounded), maxLoggedConversationIDs)
	}
	if bounded[0] != ids[0] {
		t.Fatalf("bounded head = %q, want the delivery-order head %q", bounded[0], ids[0])
	}
	short := []string{"one", "two"}
	if got := boundedConversationIDs(short); len(got) != 2 {
		t.Fatalf("short list length = %d, want 2 unchanged", len(got))
	}
}
