//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	blankRepairConversationID    = "cursor:blank-row"
	blankRepairOtherConversation = "cursor:other-row"
	blankRepairFirstDate         = 1_790_000_000
	blankRepairToolDate          = 1_790_000_060
	blankRepairLastDate          = 1_790_000_120
	blankRepairOtherDate         = 1_790_000_061
	blankRepairBlankMessage      = 1
	blankRepairAssistant         = "assistant"
	blankRepairUser              = "user"
	blankRepairFirstText         = "Inspect the tree."
	blankRepairOtherFirstText    = "Inspect a different tree."
	blankRepairLastText          = "The tree is clean."
	blankRepairOtherLastText     = "The tree has changes."
	blankRepairCommand           = "git status --short"
	blankRepairOtherCommand      = "git log --oneline"
	blankRepairToolName          = "Shell"
	blankRepairInputField        = "input"
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

// blankRepairChangedSource returns source documents that differ from the matching
// source documents in one way.
func blankRepairChangedSource(position int, change func(doc *semsearch.SemDoc)) []semsearch.SemDoc {
	docs := blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand)
	change(&docs[position])
	return docs
}

// blankRepairRow is one stored row at the blank message path.
type blankRepairRow struct {
	role             string
	content          string
	splitPart        int32
	pathConversation string
}

// blankRepairMiddleDocuments returns documents with text at the first and last
// positions and a message with blank text after the first position. With middleText,
// the position before the last also has text.
func blankRepairMiddleDocuments(middleText string) []semsearch.SemDoc {
	docs := []semsearch.SemDoc{
		blankRepairDocument(0, blankRepairUser, blankRepairFirstDate, blankRepairFirstText, ""),
		blankRepairDocument(blankRepairBlankMessage, blankRepairAssistant, blankRepairToolDate, "", blankRepairCommand),
	}
	if middleText != "" {
		docs = append(docs, blankRepairDocument(2, blankRepairAssistant, blankRepairOtherDate, middleText, ""))
	}
	return append(docs, blankRepairDocument(3, blankRepairAssistant, blankRepairLastDate, blankRepairLastText, ""))
}

// blankRowDates returns the known dates of the stored rows at the blank message path.
func blankRowDates(t *testing.T, address string, collectionID string, wantRows int) []int64 {
	t.Helper()
	ctx := context.Background()
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("milvusclient.New failed: %v.", err)
	}
	defer func() { _ = reader.Close(ctx) }()
	path := fmt.Sprintf("conv/%s/%d", blankRepairConversationID, blankRepairBlankMessage)
	otherPath := fmt.Sprintf("conv/%s/%d", blankRepairOtherConversation, blankRepairBlankMessage)
	result, err := reader.Query(ctx, milvusclient.NewQueryOption(vectorsearch.CollectionName(collectionID)).
		WithFilter(fmt.Sprintf("relativePath in [%q, %q]", path, otherPath)).
		WithOutputFields("timestampUnix").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("Query failed: %v.", err)
	}
	if result.ResultCount != wantRows {
		t.Fatalf("The query found %d blank rows instead of %d.", result.ResultCount, wantRows)
	}
	column := result.GetColumn("timestampUnix")
	dates := make([]int64, 0, wantRows)
	unknownDate := time.Time{}.Unix()
	for position := range result.ResultCount {
		isNull, err := column.IsNull(position)
		if err != nil {
			t.Fatalf("IsNull failed: %v.", err)
		}
		if isNull {
			continue
		}
		stamp, err := column.GetAsInt64(position)
		if err != nil {
			t.Fatalf("GetAsInt64 failed: %v.", err)
		}
		if stamp != unknownDate {
			dates = append(dates, stamp)
		}
	}
	return dates
}

// TestRepairDatesBlankRowOnlyWithExactStoredRows checks that source repair assigns
// a date to a blank text row only when its stored tool row and nearest text rows
// equal the source rows and store the source dates.
// It checks that repair does not assign a date under these conditions:
//
//   - A stored row differs from the source.
//   - A nearest text row stores a date that differs from the source date at its position.
//   - A nearest text row does not store a date.
//   - The stored tool row does not exist.
//   - The source message has another role or has text.
//   - The source has text between the nearest stored text rows.
//   - Another source message between the nearest stored text rows generates the text in
//     the stored tool row.
//   - The blank family has two whole rows.
//   - The blank row has a split part.
//   - The blank row path contains another conversation ID.
func TestRepairDatesBlankRowOnlyWithExactStoredRows(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	server := httptest.NewServer(blankRepairEndpoint())
	t.Cleanup(server.Close)
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	client := openClient(t, address, vectorsearch.LayoutShared)
	oneBlankRow := []blankRepairRow{{role: blankRepairAssistant, content: " ", splitPart: 0}}
	testCases := []struct {
		name       string
		stored     []semsearch.SemDoc
		source     []semsearch.SemDoc
		blankRows  []blankRepairRow
		wantMatch  bool
		wantReason string
	}{
		{
			name:       "the following text row differs from the source",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairChangedSource(2, func(doc *semsearch.SemDoc) { doc.Text = blankRepairOtherLastText }),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the source message has another role",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairChangedSource(blankRepairBlankMessage, func(doc *semsearch.SemDoc) { doc.Role = blankRepairUser }),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the source message has text",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairChangedSource(blankRepairBlankMessage, func(doc *semsearch.SemDoc) { doc.Text = blankRepairOtherLastText }),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:   "the blank family has two whole rows",
			stored: blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source: blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			blankRows: []blankRepairRow{
				{role: blankRepairAssistant, content: " ", splitPart: 0},
				{role: blankRepairUser, content: "\t", splitPart: 0},
			},
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the blank row has a split part",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			blankRows:  []blankRepairRow{{role: blankRepairAssistant, content: " ", splitPart: 1}},
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the blank row path contains another conversation ID",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			blankRows:  []blankRepairRow{{role: blankRepairAssistant, content: " ", pathConversation: blankRepairOtherConversation}},
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the preceding text row stores a date that differs from the source date at its position",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairChangedSource(0, func(doc *semsearch.SemDoc) { doc.TimestampUnix = blankRepairFirstDate + 1 }),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the following text row stores a date that differs from the source date at its position",
			stored:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			source:     blankRepairChangedSource(2, func(doc *semsearch.SemDoc) { doc.TimestampUnix = blankRepairOtherDate }),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name: "a nearest text row does not store a date",
			stored: func() []semsearch.SemDoc {
				docs := blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand)
				docs[0].TimestampUnix = time.Time{}.Unix()
				return docs
			}(),
			source:     blankRepairDocuments(blankRepairFirstText, blankRepairToolDate, blankRepairCommand),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:   "another blank source message generates the text in the stored tool row",
			stored: blankRepairMiddleDocuments(""),
			source: append(blankRepairMiddleDocuments(""),
				blankRepairDocument(2, blankRepairAssistant, blankRepairOtherDate, "", blankRepairCommand)),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
		{
			name:       "the source has text between the nearest stored text rows",
			stored:     blankRepairMiddleDocuments(""),
			source:     blankRepairMiddleDocuments(blankRepairOtherLastText),
			wantReason: vectorsearch.SourceOutcomeEmpty,
		},
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
			blankRows := testCase.blankRows
			if blankRows == nil {
				blankRows = oneBlankRow
			}
			for _, row := range blankRows {
				pathConversation := row.pathConversation
				if pathConversation == "" {
					pathConversation = blankRepairConversationID
				}
				if err := vectorsearch.WriteBlankRowForTest(ctx, client, collectionID, blankRepairConversationID, pathConversation, blankRepairBlankMessage, row.role, row.content, row.splitPart); err != nil {
					t.Fatalf("WriteBlankRowForTest failed: %v.", err)
				}
			}
			source := vectorsearch.TimestampSourceRepairSource{ConversationID: blankRepairConversationID, Documents: testCase.source}
			report, err := client.RepairTimestampsFromSource(ctx, collectionID, []vectorsearch.TimestampSourceRepairSource{source}, false)
			if err != nil {
				t.Fatalf("RepairTimestampsFromSource failed: %v.", err)
			}
			dates := blankRowDates(t, address, collectionID, len(blankRows))
			if testCase.wantMatch {
				if !slices.Equal(dates, []int64{blankRepairToolDate}) {
					t.Fatalf("The blank row has dates %v instead of %d.", dates, blankRepairToolDate)
				}
				return
			}
			if len(dates) != 0 {
				t.Fatalf("The repair dated the blank rows with %v.", dates)
			}
			if report.Outcomes[testCase.wantReason] != len(blankRows) {
				t.Fatalf("The repair returned outcomes %v; the expected count for %s is %d.", report.Outcomes, testCase.wantReason, len(blankRows))
			}
		})
	}
}
