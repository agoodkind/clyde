//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	remainingRepeatedMessage = 2
	remainingLastMessage     = 3
	remainingMarker          = "\n[turn marker]"
)

func remainingMatchText(_ string, text string) string {
	return strings.TrimSuffix(strings.TrimSuffix(text, remainingMarker), strings.TrimPrefix(remainingMarker, "\n"))
}

func remainingDocuments(firstText string, secondText string, secondDate int64, secondCommand string) []semsearch.SemDoc {
	return []semsearch.SemDoc{
		blankRepairDocument(0, blankRepairUser, blankRepairFirstDate, firstText, ""),
		blankRepairDocument(blankRepairBlankMessage, blankRepairAssistant, blankRepairToolDate, "", blankRepairCommand),
		blankRepairDocument(remainingRepeatedMessage, blankRepairAssistant, secondDate, secondText, secondCommand),
		blankRepairDocument(remainingLastMessage, blankRepairAssistant, blankRepairLastDate, blankRepairLastText, ""),
	}
}

// remainingRowDate returns the date of the stored text row at message and reports
// whether the date is known.
func remainingRowDate(t *testing.T, address string, collectionID string, message int) (int64, bool) {
	t.Helper()
	ctx := context.Background()
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("milvusclient.New failed: %v.", err)
	}
	defer func() { _ = reader.Close(ctx) }()
	path := fmt.Sprintf("conv/%s/%d", blankRepairConversationID, message)
	result, err := reader.Query(ctx, milvusclient.NewQueryOption(vectorsearch.CollectionName(collectionID)).
		WithFilter(fmt.Sprintf("relativePath == %q", path)).
		WithOutputFields("timestampUnix").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("Query failed: %v.", err)
	}
	if result.ResultCount != 1 {
		t.Fatalf("The query found %d rows at %s instead of 1.", result.ResultCount, path)
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

// TestRepairDatesRemainingRowsOnlyWithProof checks blank row date repair with repeated
// tool rows or text markers.
func TestRepairDatesRemainingRowsOnlyWithProof(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	server := httptest.NewServer(blankRepairEndpoint())
	t.Cleanup(server.Close)
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	client := openClient(t, address, vectorsearch.LayoutShared)
	repeated := remainingDocuments(blankRepairFirstText, "", blankRepairOtherDate, blankRepairCommand)
	testCases := []struct {
		name      string
		stored    []semsearch.SemDoc
		source    []semsearch.SemDoc
		matchText func(string, string) string
		blanks    []int32
		markers   []int32
		want      map[int]int64
	}{
		{
			name:   "each repeated tool row stores the date of its message",
			stored: repeated, source: repeated, blanks: []int32{1, remainingRepeatedMessage},
			want: map[int]int64{1: blankRepairToolDate, remainingRepeatedMessage: blankRepairOtherDate},
		},
		{
			name:   "the other repeated tool row stores a different date",
			stored: repeated, source: remainingDocuments(blankRepairFirstText, "", blankRepairOtherDate+1, blankRepairCommand),
			blanks: []int32{1, remainingRepeatedMessage}, want: map[int]int64{1: 0, remainingRepeatedMessage: 0},
		},
		{
			name:   "the other repeated message lacks a stored blank row",
			stored: repeated, source: repeated, blanks: []int32{1},
			want: map[int]int64{1: 0},
		},
		{
			name:      "a nearest text row matches the source after matchText",
			stored:    blankRepairDocuments(blankRepairFirstText+remainingMarker, blankRepairToolDate, blankRepairCommand),
			source:    blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			matchText: remainingMatchText, blanks: []int32{1},
			want: map[int]int64{1: blankRepairToolDate},
		},
		{
			name:   "a nearest text row differs from the source without text matching",
			stored: blankRepairDocuments(blankRepairFirstText+remainingMarker, blankRepairToolDate, blankRepairCommand),
			source: blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			blanks: []int32{1}, want: map[int]int64{1: 0},
		},
		{
			name:   "a nearest text row has another role",
			stored: blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source: blankRepairChangedSource(0, func(doc *semsearch.SemDoc) { doc.Role = blankRepairAssistant }),
			blanks: []int32{1}, want: map[int]int64{1: 0},
		},
		{
			name:      "a stored row with only a marker does not count as text",
			stored:    remainingDocuments(blankRepairFirstText, "", blankRepairOtherDate, blankRepairOtherCommand),
			source:    remainingDocuments(blankRepairFirstText, "", blankRepairOtherDate, blankRepairOtherCommand),
			matchText: remainingMatchText, blanks: []int32{1}, markers: []int32{remainingRepeatedMessage},
			want: map[int]int64{1: blankRepairToolDate},
		},
	}
	for position, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			collectionID := fmt.Sprintf("remaining-repair-%d", position)
			manifest := []semsearch.Fingerprint{{ConversationID: blankRepairConversationID, Value: "v1"}}
			if _, err := client.UpsertConversationDocuments(ctx, collectionID, testCase.stored, manifest); err != nil {
				t.Fatalf("UpsertConversationDocuments failed: %v.", err)
			}
			for _, message := range testCase.blanks {
				if err := vectorsearch.WriteBlankRowForTest(ctx, client, collectionID, blankRepairConversationID, blankRepairConversationID, message, blankRepairAssistant, " ", 0); err != nil {
					t.Fatalf("WriteBlankRowForTest failed: %v.", err)
				}
			}
			for _, message := range testCase.markers {
				if err := vectorsearch.WriteBlankRowForTest(ctx, client, collectionID, blankRepairConversationID, blankRepairConversationID, message, blankRepairAssistant, strings.TrimPrefix(remainingMarker, "\n"), 0); err != nil {
					t.Fatalf("WriteBlankRowForTest failed: %v.", err)
				}
			}
			source := vectorsearch.TimestampSourceRepairSource{ConversationID: blankRepairConversationID, Documents: testCase.source, MatchText: testCase.matchText}
			if _, err := client.RepairTimestampsFromSource(ctx, collectionID, []vectorsearch.TimestampSourceRepairSource{source}, false); err != nil {
				t.Fatalf("RepairTimestampsFromSource failed: %v.", err)
			}
			for message, want := range testCase.want {
				stamp, dated := remainingRowDate(t, address, collectionID, message)
				if want == 0 && dated {
					t.Fatalf("The repair assigned message %d the date %d.", message, stamp)
				}
				if want != 0 && (!dated || stamp != want) {
					t.Fatalf("Message %d has date %d with dated=%t; the expected date is %d.", message, stamp, dated, want)
				}
			}
		})
	}
}
