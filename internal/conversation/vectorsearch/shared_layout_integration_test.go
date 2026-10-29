//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	embeddingBaseURLEnv      = "CLYDE_TEST_EMBEDDING_BASE_URL"
	defaultEmbeddingBaseURL  = "http://[::1]:5400/v1"
	integrationEmbeddingKey  = "clyde-milvus-integration"
	integrationModel         = "nvidia/NV-EmbedCode-7b-v1"
	integrationDimension     = 4096
	sourceCollectionID       = "integration-single"
	sharedCollectionID       = "integration-shared"
	integrationSearchLimit   = 8
	integrationScoreEpsilon  = 1e-6
	integrationVerifySamples = 200
	visibilityWait           = 3 * time.Second
)

func openClient(t *testing.T, address string, layout vectorsearch.Layout) *vectorsearch.Client {
	t.Helper()
	baseURL := os.Getenv(embeddingBaseURLEnv)
	if baseURL == "" {
		baseURL = defaultEmbeddingBaseURL
	}
	var client *vectorsearch.Client
	var err error
	deadline := time.Now().Add(time.Minute)
	for {
		client, err = vectorsearch.Open(context.Background(), vectorsearch.Options{
			Backend:            vectorsearch.BackendMilvus,
			MilvusAddress:      address,
			MilvusDatabase:     "default",
			MilvusLayout:       layout,
			EmbeddingBaseURL:   baseURL,
			EmbeddingModel:     integrationModel,
			EmbeddingDimension: integrationDimension,
			EmbeddingAPIKey:    integrationEmbeddingKey,
			EmbeddingTimeout:   2 * time.Minute,
			CheckpointDir:      t.TempDir(),
		})
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatalf("open %s client: %v", layout, err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

// integrationDocuments returns six conversations. Every conversation repeats
// two message texts, which produces rows that store equal content.
func integrationDocuments() ([]semsearch.SemDoc, []semsearch.Fingerprint) {
	topics := []string{"Milvus segment compaction", "Go module version", "SwiftUI view model", "WireGuard peer routing", "MinIO bucket retention", "BM25 ranking"}
	repeated := []string{"The build passes on the main branch.", "The test suite reports zero failures."}
	docs := make([]semsearch.SemDoc, 0)
	manifest := make([]semsearch.Fingerprint, 0)
	for conversationIndex, topic := range topics {
		conversationID := fmt.Sprintf("codex:integration-%02d", conversationIndex)
		texts := []string{
			"The " + topic + " setting fails with a stale configuration value.",
			"The " + topic + " configuration file sets an outdated value.",
			repeated[0],
			"The " + topic + " logs report one warning.",
			repeated[1],
			"The " + topic + " logs list zero errors.",
		}
		for messageIndex, text := range texts {
			role := "user"
			if messageIndex%2 == 1 {
				role = "assistant"
			}
			docs = append(docs, semsearch.SemDoc{
				ConversationID:       conversationID,
				ParentConversationID: "",
				MessageIndex:         int32(messageIndex),
				Role:                 role,
				TimestampUnix:        1_790_000_000 + int64(conversationIndex*100+messageIndex),
				Text:                 text,
				Tools:                nil,
				Thinking:             "",
				WorkspaceRoot:        fmt.Sprintf("/work/%d", conversationIndex%2),
				Archived:             false,
				LoadRules:            "",
			})
		}
		manifest = append(manifest, semsearch.Fingerprint{ConversationID: conversationID, Value: "v1"})
	}
	return docs, manifest
}

type searchCase struct {
	name   string
	query  string
	filter semsearch.SearchFilter
}

func integrationSearchCases() []searchCase {
	cases := make([]searchCase, 0)
	for _, query := range []string{"segment compaction configuration", "test suite failures", "logs report warning", "build main branch", "WireGuard routing value"} {
		cases = append(cases,
			searchCase{name: "all", query: query, filter: semsearch.SearchFilter{}},
			searchCase{name: "assistant", query: query, filter: semsearch.SearchFilter{Roles: []string{"assistant"}}},
			searchCase{name: "conversations", query: query, filter: semsearch.SearchFilter{ConversationIDs: []string{"codex:integration-01", "codex:integration-04"}}},
			searchCase{name: "time", query: query, filter: semsearch.SearchFilter{FromUnix: 1_790_000_100, UntilUnix: 1_790_000_400}},
		)
	}
	return cases
}

func hitIdentity(hit semsearch.SemHit) string {
	return hit.ConversationID + "#" + strconv.Itoa(int(hit.MessageIndex)) + "#" + hit.Content
}

// requireSameRanking checks that both hit lists have the same score at every
// position and the same rows at every score above the last one. Rows with the
// last score can differ, because Milvus keeps equal-score rows at the cut in
// segment order.
func requireSameRanking(t *testing.T, label string, single []semsearch.SemHit, shared []semsearch.SemHit) {
	t.Helper()
	if len(single) != len(shared) {
		t.Fatalf("%s: single layout returned %d hits, shared layout %d", label, len(single), len(shared))
	}
	if len(single) == 0 {
		t.Fatalf("%s: both layouts returned zero hits", label)
	}
	for position := range single {
		if math.Abs(single[position].Score-shared[position].Score) > integrationScoreEpsilon {
			t.Fatalf("%s: score at %d is %.9f in single layout and %.9f in shared layout", label, position, single[position].Score, shared[position].Score)
		}
	}
	lastScore := single[len(single)-1].Score
	singleRows := make([]string, 0)
	sharedRows := make([]string, 0)
	for position := range single {
		if single[position].Score-lastScore > integrationScoreEpsilon {
			singleRows = append(singleRows, hitIdentity(single[position]))
			sharedRows = append(sharedRows, hitIdentity(shared[position]))
		}
	}
	slices.Sort(singleRows)
	slices.Sort(sharedRows)
	if !slices.Equal(singleRows, sharedRows) {
		t.Fatalf("%s: rows above the last score differ: single %v, shared %v", label, singleRows, sharedRows)
	}
}

func requireIndexPasses(t *testing.T, stage string, client *vectorsearch.Client) {
	t.Helper()
	check, err := client.VerifyIndex(context.Background(), sharedCollectionID, integrationVerifySamples)
	if err != nil {
		t.Fatalf("%s: verify shared index: %v", stage, err)
	}
	if check.Checked == 0 || check.Found != check.Checked {
		t.Fatalf("%s: shared index check %+v, want every sample to pass", stage, check)
	}
}

func requireSearchesMatch(t *testing.T, single *vectorsearch.Client, shared *vectorsearch.Client) {
	t.Helper()
	ctx := context.Background()
	for _, test := range integrationSearchCases() {
		label := test.name + " " + strconv.Quote(test.query)
		singleHits, err := single.SearchConversations(ctx, sourceCollectionID, test.query, integrationSearchLimit, test.filter, 0)
		if err != nil {
			t.Fatalf("%s: single layout search: %v", label, err)
		}
		sharedHits, err := shared.SearchConversations(ctx, sharedCollectionID, test.query, integrationSearchLimit, test.filter, 0)
		if err != nil {
			t.Fatalf("%s: shared layout search: %v", label, err)
		}
		requireSameRanking(t, label, singleHits, sharedHits)
	}
}

func requireSharedWriteSearchable(t *testing.T, shared *vectorsearch.Client) {
	t.Helper()
	ctx := context.Background()
	added := []semsearch.SemDoc{{
		ConversationID: "codex:integration-new", ParentConversationID: "", MessageIndex: 0, Role: "user",
		TimestampUnix: 1_790_001_000, Text: "The etcd snapshot count setting equals fifty thousand.", Tools: nil, Thinking: "",
		WorkspaceRoot: "/work/0", Archived: false, LoadRules: "",
	}}
	if _, err := shared.UpsertConversationDocuments(ctx, sharedCollectionID, added, []semsearch.Fingerprint{{ConversationID: "codex:integration-new", Value: "v1"}}); err != nil {
		t.Fatalf("write shared layout: %v", err)
	}
	time.Sleep(visibilityWait)
	hits, err := shared.SearchConversations(ctx, sharedCollectionID, "etcd snapshot count setting", 3, semsearch.SearchFilter{}, 0)
	if err != nil {
		t.Fatalf("search new shared row: %v", err)
	}
	if len(hits) == 0 || hits[0].ConversationID != "codex:integration-new" {
		t.Fatalf("search for the new row returned %+v, want codex:integration-new first", hits)
	}
}

func requireSharedDeleteRemovesRows(t *testing.T, shared *vectorsearch.Client) {
	t.Helper()
	ctx := context.Background()
	removed, err := shared.DeleteConversation(ctx, sharedCollectionID, "codex:integration-02")
	if err != nil {
		t.Fatalf("delete shared conversation: %v", err)
	}
	if removed != 6 {
		t.Fatalf("delete removed %d rows, want 6", removed)
	}
	time.Sleep(visibilityWait)
	remaining, err := shared.CountConversationRows(ctx, sharedCollectionID, "codex:integration-02")
	if err != nil {
		t.Fatalf("count deleted conversation rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("deleted conversation keeps %d rows", remaining)
	}
}

// TestSharedLayoutCopiesAndSearchesLikeSingleLayout writes conversations with
// the single-collection layout on a throwaway Milvus and the real embedding
// endpoint, copies them into the shared vector layout, and checks the copy row
// by row with exact vector bytes. It then checks that 20 searches return the
// same scores and rows in both layouts, that index verification passes in the
// shared layout, and that new writes and a conversation delete work in the
// shared layout.
func TestSharedLayoutCopiesAndSearchesLikeSingleLayout(t *testing.T) {
	milvustest.RequireEnabled(t)
	ctx := context.Background()
	address := milvustest.Start(t)
	single := openClient(t, address, vectorsearch.LayoutSingle)
	shared := openClient(t, address, vectorsearch.LayoutShared)
	docs, manifest := integrationDocuments()
	if _, err := single.UpsertConversationDocuments(ctx, sourceCollectionID, docs, manifest); err != nil {
		t.Fatalf("write single layout: %v", err)
	}
	copied, err := shared.CopyToShared(ctx, sourceCollectionID, sharedCollectionID, false, nil)
	if err != nil {
		t.Fatalf("copy to shared layout: %v", err)
	}
	if copied != int64(len(docs)) {
		t.Fatalf("copy wrote %d rows, want %d", copied, len(docs))
	}
	time.Sleep(visibilityWait)
	resumed, err := shared.CopyToShared(ctx, sourceCollectionID, sharedCollectionID, true, nil)
	if err != nil {
		t.Fatalf("resume copy to shared layout: %v", err)
	}
	if resumed != 0 {
		t.Fatalf("resumed copy wrote %d rows after a complete copy, want 0", resumed)
	}
	report, err := shared.VerifySharedCopy(ctx, sourceCollectionID, sharedCollectionID, nil)
	if err != nil {
		t.Fatalf("verify shared copy: %v", err)
	}
	if !report.Equal() || report.SourceRows != int64(len(docs)) {
		t.Fatalf("shared copy report %+v, want %d equal rows", report, len(docs))
	}
	if report.TargetVectors >= report.TargetRows {
		t.Fatalf("shared copy stores %d vectors for %d rows, want fewer vectors than rows", report.TargetVectors, report.TargetRows)
	}
	t.Logf("copied %d rows into %d distinct vectors", report.TargetRows, report.TargetVectors)
	requireSearchesMatch(t, single, shared)
	requireIndexPasses(t, "copied collection", shared)
	requireSharedWriteSearchable(t, shared)
	requireSharedDeleteRemovesRows(t, shared)
	requireIndexPasses(t, "collection without codex:integration-02", shared)
}
