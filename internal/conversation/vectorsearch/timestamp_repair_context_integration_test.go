//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	contextTypedText    = "Fix the build."
	contextCommandBlock = "<cursor_commands>\nrun the tests\n</cursor_commands>\n"
	contextFilesBlock   = "<attached_files>\nmain.go\n</attached_files>\n"
	contextOtherBlock   = "<other_block>\nnotes\n</other_block>\n"
	contextOpenBlock    = "<cursor_commands>\n"
)

// contextDocuments returns dated assistant rows before and after a user row at the
// target position.
func contextDocuments(targetDate int64, targetText string) []semsearch.SemDoc {
	return []semsearch.SemDoc{
		textOrderDocument(0, textOrderAssistant, textOrderEarlierDate, textOrderEarlierText),
		textOrderDocument(textOrderTargetMessage, textOrderUser, targetDate, targetText),
		textOrderDocument(textOrderLaterMessage, textOrderAssistant, textOrderLaterDate, textOrderLaterText),
	}
}

// TestRepairDatesUserTextAfterContextBlocks checks date repair against Milvus for
// user rows that start with context blocks.
func TestRepairDatesUserTextAfterContextBlocks(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	server := httptest.NewServer(textOrderEndpoint())
	t.Cleanup(server.Close)
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	client := openClient(t, address, vectorsearch.LayoutShared)
	undated := time.Time{}.Unix()
	testCases := []struct {
		name        string
		storedText  string
		sourceDate  int64
		wantDate    int64
		wantOutcome string
	}{
		{
			name:        "context blocks before typed text with source date between stored dates",
			storedText:  contextCommandBlock + contextFilesBlock + contextTypedText,
			sourceDate:  textOrderMiddleDate,
			wantDate:    textOrderMiddleDate,
			wantOutcome: vectorsearch.SourceOutcomeMatched,
		},
		{
			name:        "context block with source date after next stored date",
			storedText:  contextCommandBlock + contextTypedText,
			sourceDate:  textOrderLaterDate + 1,
			wantOutcome: vectorsearch.SourceOutcomeDateOrderConflict,
		},
		{
			name:        "unlisted leading context block remains unmatched",
			storedText:  contextOtherBlock + contextTypedText,
			sourceDate:  textOrderMiddleDate,
			wantOutcome: vectorsearch.SourceOutcomeUnmatched,
		},
		{
			name:        "leading context block without closing tag remains unmatched",
			storedText:  contextOpenBlock + contextTypedText,
			sourceDate:  textOrderMiddleDate,
			wantOutcome: vectorsearch.SourceOutcomeUnmatched,
		},
	}
	for position, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			collectionID := fmt.Sprintf("context-blocks-%d", position)
			manifest := []semsearch.Fingerprint{{ConversationID: textOrderConversationID, Value: "v1"}}
			if _, err := client.UpsertConversationDocuments(ctx, collectionID, contextDocuments(undated, testCase.storedText), manifest); err != nil {
				t.Fatalf("UpsertConversationDocuments failed: %v.", err)
			}
			source := vectorsearch.TimestampSourceRepairSource{ConversationID: textOrderConversationID, Documents: contextDocuments(testCase.sourceDate, contextTypedText)}
			report, err := client.RepairTimestampsFromSource(ctx, collectionID, []vectorsearch.TimestampSourceRepairSource{source}, false)
			if err != nil {
				t.Fatalf("RepairTimestampsFromSource failed: %v.", err)
			}
			if report.Outcomes[testCase.wantOutcome] != 1 {
				t.Fatalf("The repair returned outcomes %v; the expected outcome is %s.", report.Outcomes, testCase.wantOutcome)
			}
			path := fmt.Sprintf("conv/%s/%d", textOrderConversationID, textOrderTargetMessage)
			stamp, dated := textOrderRowDate(t, address, collectionID, path)
			if testCase.wantDate == 0 && dated {
				t.Fatalf("The repair assigned the date %d.", stamp)
			}
			if testCase.wantDate != 0 && (!dated || stamp != testCase.wantDate) {
				t.Fatalf("The row has date %d with dated=%t; the expected date is %d.", stamp, dated, testCase.wantDate)
			}
		})
	}
}
