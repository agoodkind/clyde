//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	textOrderConversationID = "cursor:text-order"
	textOrderEarlierDate    = 1_790_000_000
	textOrderMiddleDate     = 1_790_000_060
	textOrderLaterDate      = 1_790_000_120
	textOrderTargetMessage  = 1
	textOrderLaterMessage   = 2
	textOrderAssistant      = "assistant"
	textOrderUser           = "user"
	textOrderEarlierText    = "Inspect the tree."
	textOrderTargetText     = "The tree has one change."
	textOrderLaterText      = "The tree is clean."
	textOrderCommand        = "git status --short"
	textOrderToolName       = "Shell"
	textOrderSplitBytes     = 60000
	textOrderSplitSpaces    = 8
)

// textOrderEndpoint returns an HTTP handler that returns one vector for each input.
func textOrderEndpoint() http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, 0, len(body.Input))
		for position, input := range body.Input {
			digest := fnv.New32a()
			_, _ = digest.Write([]byte(input))
			vector := make([]float64, integrationDimension)
			vector[int(digest.Sum32())%integrationDimension] = 1
			data = append(data, map[string]any{"embedding": vector, "index": position})
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
	}
}

func textOrderDocument(message int32, role string, date int64, text string) semsearch.SemDoc {
	return semsearch.SemDoc{
		ConversationID: textOrderConversationID, ParentConversationID: "", MessageIndex: message, Role: role,
		TimestampUnix: date, Text: text, Tools: nil, Thinking: "", WorkspaceRoot: "", Archived: false, LoadRules: "",
	}
}

// textOrderSplitText contains 60,000 letters followed by 8 spaces. The writer
// stores the letters as the first part and drops the part containing only spaces.
var textOrderSplitText = strings.Repeat("a", textOrderSplitBytes) + strings.Repeat(" ", textOrderSplitSpaces)

// textOrderDocuments returns dated rows before and after the target row.
func textOrderDocuments(targetDate int64, targetText string) []semsearch.SemDoc {
	return []semsearch.SemDoc{
		textOrderDocument(0, textOrderUser, textOrderEarlierDate, textOrderEarlierText),
		textOrderDocument(textOrderTargetMessage, textOrderAssistant, targetDate, targetText),
		textOrderDocument(textOrderLaterMessage, textOrderAssistant, textOrderLaterDate, textOrderLaterText),
	}
}

func textOrderRowDate(t *testing.T, address string, collectionID string, path string) (int64, bool) {
	t.Helper()
	ctx := context.Background()
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("milvusclient.New failed: %v.", err)
	}
	defer func() { _ = reader.Close(ctx) }()
	result, err := reader.Query(ctx, milvusclient.NewQueryOption(vectorsearch.CollectionName(collectionID)).
		WithFilter(fmt.Sprintf("relativePath == %q", path)).
		WithOutputFields("timestampUnix").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("Query failed: %v.", err)
	}
	if result.ResultCount == 0 {
		t.Fatalf("The query did not find rows at %s.", path)
	}
	column := result.GetColumn("timestampUnix")
	first, firstDated := textOrderCellDate(t, column, 0)
	for position := 1; position < result.ResultCount; position++ {
		if stamp, dated := textOrderCellDate(t, column, position); stamp != first || dated != firstDated {
			t.Fatalf("The rows at %s have different dates %d and %d.", path, first, stamp)
		}
	}
	return first, firstDated
}

// textOrderCellDate returns the date in a timestamp cell and reports whether it is known.
func textOrderCellDate(t *testing.T, column column.Column, position int) (int64, bool) {
	t.Helper()
	isNull, err := column.IsNull(position)
	if err != nil {
		t.Fatalf("IsNull failed: %v.", err)
	}
	if isNull {
		return 0, false
	}
	stamp, err := column.GetAsInt64(position)
	if err != nil {
		t.Fatalf("GetAsInt64 failed: %v.", err)
	}
	return stamp, stamp != time.Time{}.Unix()
}

// TestRepairDatesTextRowsWithinStoredOrderAndSkipsBlankRows checks timestamp repair
// against Milvus. It checks composer bounded dates against nearby stored dates and
// verifies that blank rows and blank parts remain undated.
func TestRepairDatesTextRowsWithinStoredOrderAndSkipsBlankRows(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	server := httptest.NewServer(textOrderEndpoint())
	t.Cleanup(server.Close)
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	client := openClient(t, address, vectorsearch.LayoutShared)
	undated := time.Time{}.Unix()
	blankWithTool := textOrderDocuments(textOrderMiddleDate, "")
	blankWithTool[textOrderTargetMessage].Tools = []semsearch.SemToolCall{{Name: textOrderToolName, Display: textOrderCommand, LangHint: "", Output: "", IsError: false, TimestampUnix: nil, TimestampUnknown: false}}
	testCases := []struct {
		name        string
		stored      []semsearch.SemDoc
		source      []semsearch.SemDoc
		bounded     []int64
		blank       bool
		split       bool
		wantDate    int64
		wantOutcome string
	}{
		{
			name:        "composer bounded date between nearest stored dates is dated",
			stored:      textOrderDocuments(undated, textOrderTargetText),
			source:      textOrderDocuments(textOrderMiddleDate, textOrderTargetText),
			bounded:     []int64{textOrderMiddleDate},
			wantDate:    textOrderMiddleDate,
			wantOutcome: vectorsearch.SourceOutcomeMatched,
		},
		{
			name:        "composer bounded date after next stored date conflicts",
			stored:      textOrderDocuments(undated, textOrderTargetText),
			source:      textOrderDocuments(textOrderLaterDate+1, textOrderTargetText),
			bounded:     []int64{textOrderLaterDate + 1},
			wantOutcome: vectorsearch.SourceOutcomeDateOrderConflict,
		},
		{
			name:        "composer bounded date before previous stored date conflicts",
			stored:      textOrderDocuments(undated, textOrderTargetText),
			source:      textOrderDocuments(textOrderEarlierDate-1, textOrderTargetText),
			bounded:     []int64{textOrderEarlierDate - 1},
			wantOutcome: vectorsearch.SourceOutcomeDateOrderConflict,
		},
		{
			name:        "tool bounded date after next stored date is dated",
			stored:      textOrderDocuments(undated, textOrderTargetText),
			source:      textOrderDocuments(textOrderLaterDate+1, textOrderTargetText),
			wantDate:    textOrderLaterDate + 1,
			wantOutcome: vectorsearch.SourceOutcomeMatched,
		},
		{
			name:        "composer bounded date without other stored dates is dated",
			stored:      []semsearch.SemDoc{textOrderDocument(textOrderTargetMessage, textOrderAssistant, undated, textOrderTargetText)},
			source:      []semsearch.SemDoc{textOrderDocument(textOrderTargetMessage, textOrderAssistant, textOrderMiddleDate, textOrderTargetText)},
			bounded:     []int64{textOrderMiddleDate},
			wantDate:    textOrderMiddleDate,
			wantOutcome: vectorsearch.SourceOutcomeMatched,
		},
		{
			name:        "blank row with matching source and tool dates stays undated",
			stored:      blankWithTool,
			source:      blankWithTool,
			blank:       true,
			wantOutcome: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:        "blank second part of matched text stays undated",
			stored:      textOrderDocuments(undated, textOrderSplitText),
			source:      textOrderDocuments(textOrderMiddleDate, textOrderSplitText),
			split:       true,
			wantDate:    textOrderMiddleDate,
			wantOutcome: vectorsearch.SourceOutcomeMatched,
		},
	}
	for position, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			collectionID := fmt.Sprintf("text-order-%d", position)
			manifest := []semsearch.Fingerprint{{ConversationID: textOrderConversationID, Value: "v1"}}
			if _, err := client.UpsertConversationDocuments(ctx, collectionID, testCase.stored, manifest); err != nil {
				t.Fatalf("UpsertConversationDocuments failed: %v.", err)
			}
			if testCase.blank {
				if err := vectorsearch.WriteBlankRowForTest(ctx, client, collectionID, textOrderConversationID, textOrderConversationID, textOrderTargetMessage, textOrderAssistant, " ", 0); err != nil {
					t.Fatalf("WriteBlankRowForTest failed: %v.", err)
				}
			}
			if testCase.split {
				blankPart := strings.Repeat(" ", textOrderSplitSpaces)
				if err := vectorsearch.WriteTextPartForTest(ctx, client, collectionID, textOrderConversationID, textOrderTargetMessage, 1, textOrderAssistant, blankPart); err != nil {
					t.Fatalf("WriteTextPartForTest failed: %v.", err)
				}
			}
			source := vectorsearch.TimestampSourceRepairSource{ConversationID: textOrderConversationID, Documents: testCase.source, ComposerBoundedDates: testCase.bounded}
			report, err := client.RepairTimestampsFromSource(ctx, collectionID, []vectorsearch.TimestampSourceRepairSource{source}, false)
			if err != nil {
				t.Fatalf("RepairTimestampsFromSource failed: %v.", err)
			}
			path := fmt.Sprintf("conv/%s/%d", textOrderConversationID, textOrderTargetMessage)
			if !testCase.split && report.Outcomes[testCase.wantOutcome] != 1 {
				t.Fatalf("The repair returned outcomes %v; the expected outcome is %s.", report.Outcomes, testCase.wantOutcome)
			}
			if testCase.split {
				if report.Outcomes[vectorsearch.SourceOutcomeEmpty] != 1 || report.Outcomes[testCase.wantOutcome] == 0 || len(report.Outcomes) != 2 {
					t.Fatalf("The repair returned outcomes %v; the expected outcomes are %s rows and 1 empty content row.", report.Outcomes, testCase.wantOutcome)
				}
				if stamp, dated := textOrderRowDate(t, address, collectionID, path+"/1"); dated {
					t.Fatalf("The repair assigned the date %d to the blank part.", stamp)
				}
				path += "/0"
			}
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
