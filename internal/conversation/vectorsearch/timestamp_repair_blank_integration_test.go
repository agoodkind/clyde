//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	blankRepairConversationID = "cursor:blank-row"
	blankRepairFirstDate      = 1_790_000_000
	blankRepairToolDate       = 1_790_000_060
	blankRepairLastDate       = 1_790_000_120
	blankRepairOtherDate      = 1_790_000_061
	blankRepairBlankMessage   = 1
	blankRepairAssistant      = "assistant"
	blankRepairUser           = "user"
	blankRepairFirstText      = "Inspect the tree."
	blankRepairOtherFirstText = "Inspect a different tree."
	blankRepairLastText       = "The tree is clean."
	blankRepairCommand        = "git status --short"
	blankRepairOtherCommand   = "git log --oneline"
	blankRepairToolName       = "Shell"
	blankRepairInputField     = "input"
)

// blankRepairEndpoint returns an HTTP handler that generates one vector for each input.
func blankRepairEndpoint() http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		var inputs []string
		if err := json.Unmarshal(body[blankRepairInputField], &inputs); err != nil {
			var single string
			if singleErr := json.Unmarshal(body[blankRepairInputField], &single); singleErr != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			inputs = []string{single}
		}
		data := make([]map[string]any, 0, len(inputs))
		for position, input := range inputs {
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

func blankRepairDocument(message int32, role string, date int64, text string, command string) semsearch.SemDoc {
	doc := semsearch.SemDoc{
		ConversationID: blankRepairConversationID, ParentConversationID: "", MessageIndex: message, Role: role,
		TimestampUnix: date, Text: text, Tools: nil, Thinking: "", WorkspaceRoot: "", Archived: false, LoadRules: "",
	}
	if command != "" {
		doc.Tools = []semsearch.SemToolCall{{Name: blankRepairToolName, Display: command, LangHint: "", Output: "", IsError: false, TimestampUnix: nil, TimestampUnknown: false}}
	}
	return doc
}

func blankRepairDocuments(firstText string, toolDate int64, command string) []semsearch.SemDoc {
	return []semsearch.SemDoc{
		blankRepairDocument(0, blankRepairUser, blankRepairFirstDate, firstText, ""),
		blankRepairDocument(blankRepairBlankMessage, blankRepairAssistant, toolDate, "", command),
		blankRepairDocument(2, blankRepairAssistant, blankRepairLastDate, blankRepairLastText, ""),
	}
}

func blankRowDate(t *testing.T, address string, collectionID string) (int64, bool) {
	t.Helper()
	ctx := context.Background()
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("milvusclient.New failed: %v.", err)
	}
	defer func() { _ = reader.Close(ctx) }()
	path := fmt.Sprintf("conv/%s/%d", blankRepairConversationID, blankRepairBlankMessage)
	result, err := reader.Query(ctx, milvusclient.NewQueryOption(vectorsearch.CollectionName(collectionID)).
		WithFilter(fmt.Sprintf("relativePath == %q", path)).
		WithOutputFields("timestampUnix").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("Query failed: %v.", err)
	}
	if result.ResultCount != 1 {
		t.Fatalf("The query found %d blank rows instead of 1.", result.ResultCount)
	}
	column := result.GetColumn("timestampUnix")
	isNull, err := column.IsNull(0)
	if err != nil {
		t.Fatalf("IsNull failed: %v.", err)
	}
	if isNull {
		return 0, false
	}
	stamp, err := column.GetAsInt64(0)
	if err != nil {
		t.Fatalf("GetAsInt64 failed: %v.", err)
	}
	return stamp, stamp != time.Time{}.Unix()
}

// TestRepairDatesBlankRowOnlyWithExactStoredRows checks that source repair dates a blank
// text row when its stored tool row and nearest text rows match the source and its
// stored tool date equals the source date. It also checks that repair leaves the row
// without a date when any match fails or the stored tool row does not exist.
func TestRepairDatesBlankRowOnlyWithExactStoredRows(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	server := httptest.NewServer(blankRepairEndpoint())
	t.Cleanup(server.Close)
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	client := openClient(t, address, vectorsearch.LayoutShared)
	testCases := []struct {
		name       string
		stored     []semsearch.SemDoc
		source     []semsearch.SemDoc
		wantMatch  bool
		wantReason string
	}{
		{
			name:      "the stored rows match the source",
			stored:    blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:    blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			wantMatch: true,
		},
		{
			name:       "the tool row differs from the source",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairOtherCommand),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the tool row date differs from the source",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairDocuments(blankRepairFirstText, blankRepairOtherDate, blankRepairCommand),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "a nearby text row differs from the source",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairDocuments(blankRepairOtherFirstText, blankRepairToolDate, blankRepairCommand),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the message does not have a stored tool row",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, ""),
			source:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
	}
	for position, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			collectionID := fmt.Sprintf("blank-repair-%d", position)
			manifest := []semsearch.Fingerprint{{ConversationID: blankRepairConversationID, Value: "v1"}}
			if _, err := client.UpsertConversationDocuments(ctx, collectionID, testCase.stored, manifest); err != nil {
				t.Fatalf("UpsertConversationDocuments failed: %v.", err)
			}
			if err := vectorsearch.WriteBlankRowForTest(ctx, client, collectionID, blankRepairConversationID, blankRepairBlankMessage, blankRepairAssistant); err != nil {
				t.Fatalf("WriteBlankRowForTest failed: %v.", err)
			}
			source := vectorsearch.TimestampSourceRepairSource{ConversationID: blankRepairConversationID, Documents: testCase.source}
			report, err := client.RepairTimestampsFromSource(ctx, collectionID, []vectorsearch.TimestampSourceRepairSource{source}, false)
			if err != nil {
				t.Fatalf("RepairTimestampsFromSource failed: %v.", err)
			}
			stamp, dated := blankRowDate(t, address, collectionID)
			if testCase.wantMatch {
				if !dated || stamp != blankRepairToolDate {
					t.Fatalf("The blank row has timestamp %d with dated=%t; it must have timestamp %d with dated=true.", stamp, dated, blankRepairToolDate)
				}
				return
			}
			if dated {
				t.Fatalf("The repair dated the blank row with %d.", stamp)
			}
			if report.Outcomes[testCase.wantReason] != 1 {
				t.Fatalf("The repair returned outcomes %v; the expected count for %s is 1.", report.Outcomes, testCase.wantReason)
			}
		})
	}
}
