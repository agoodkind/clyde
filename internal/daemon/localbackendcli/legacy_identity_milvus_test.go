//go:build milvusintegration

package localbackendcli_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	legacyConversationColumn = "conversationId"
	legacyVectorSuffix       = "_vectors"
	legacyAdminTimeout       = 5 * time.Minute
)

// occurrenceCollection returns the Milvus collection without the _vectors suffix and fails the test unless exactly one exists.
func occurrenceCollection(t *testing.T, ctx context.Context, client *milvusclient.Client) string {
	t.Helper()
	names, err := client.ListCollections(ctx, milvusclient.NewListCollectionOption())
	if err != nil {
		t.Fatalf("list Milvus collections: %v", err)
	}
	occurrences := []string{}
	for _, name := range names {
		if !strings.HasSuffix(name, legacyVectorSuffix) {
			occurrences = append(occurrences, name)
		}
	}
	if len(occurrences) != 1 {
		t.Fatalf("Milvus lists occurrence collections %v, want exactly one", occurrences)
	}
	return occurrences[0]
}

// clearConversationIdentity reads every row of one conversation with Strong consistency, stores the rows again with a null conversationId, and returns the row count. The rows retain their identity only in metadata.
func clearConversationIdentity(t *testing.T, ctx context.Context, client *milvusclient.Client, collectionName string, conversationID string) int {
	t.Helper()
	expression := fmt.Sprintf("%s == %q", legacyConversationColumn, conversationID)
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(collectionName).
		WithFilter(expression).
		WithOutputFields("*").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("read the rows of %s: %v", conversationID, err)
	}
	if resultSet.ResultCount == 0 {
		t.Fatalf("collection %s stores no rows of %s", collectionName, conversationID)
	}
	columns := make([]column.Column, 0, len(resultSet.Fields))
	for _, field := range resultSet.Fields {
		if field.Name() != legacyConversationColumn {
			columns = append(columns, field)
			continue
		}
		nulls, err := column.NewNullableColumnVarChar(legacyConversationColumn, []string{}, make([]bool, resultSet.ResultCount))
		if err != nil {
			t.Fatalf("build the null %s column: %v", legacyConversationColumn, err)
		}
		columns = append(columns, nulls)
	}
	if _, err := client.Upsert(ctx, milvusclient.NewColumnBasedInsertOption(collectionName, columns...)); err != nil {
		t.Fatalf("store the rows of %s with a null %s: %v", conversationID, legacyConversationColumn, err)
	}
	return resultSet.ResultCount
}

// nullIdentityCount reads rows with Strong consistency and returns the count of rows with a null conversationId.
func nullIdentityCount(t *testing.T, ctx context.Context, client *milvusclient.Client, collectionName string) int64 {
	t.Helper()
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(collectionName).
		WithFilter(legacyConversationColumn+" is null").
		WithOutputFields("count(*)").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("count rows with a null %s: %v", legacyConversationColumn, err)
	}
	count, err := resultSet.GetColumn("count(*)").GetAsInt64(0)
	if err != nil {
		t.Fatalf("read the null %s count: %v", legacyConversationColumn, err)
	}
	return count
}

// conversationKeys returns the match keys of one conversation in message index order.
func conversationKeys(matches []localProofMatch, conversationID string) []localProofMatchKey {
	keys := []localProofMatchKey{}
	for _, key := range localProofMatchKeys(matches) {
		if key.conversationID == conversationID {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(left localProofMatchKey, right localProofMatchKey) int {
		return left.messageIndex - right.messageIndex
	})
	return keys
}

// TestMilvusSearchKeepsRowsWithMetadataOnlyIdentity runs the sandbox daemon with a throwaway Milvus, records alpha matches from CLI and MCP search, and clears the rows' conversationId values. It requires both searches to return the same alpha matches when identity remains only in metadata.
func TestMilvusSearchKeepsRowsWithMetadataOnlyIdentity(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	baseURL := os.Getenv(deletedSourceEmbeddingEnv)
	if baseURL == "" {
		baseURL = deletedSourceEmbeddingDefault
	}
	settingsPath := writeSandboxSemanticSettings(t, fmt.Sprintf(eligibleSettings, address, baseURL, deletedSourceEmbeddingModel, deletedSourceEmbeddingDim))
	binaryPath := buildClydeCLI(t)
	home := writeLocalProofHome(t)
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	waitForProofRowsFrom(t, sandboxDaemon, localProofSourceSemantic)

	alphaID := localProofProviderClaude + ":" + localProofAlphaSession
	request := localProofSearchRequest{Query: localProofQuery, Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false}
	before := map[string][]localProofMatchKey{}
	for _, surface := range sandboxDaemon.surfaces() {
		before[surface.name] = conversationKeys(mustLocalProofSearch(t, surface, request).Matches, alphaID)
		if len(before[surface.name]) != localProofSessionRows {
			t.Fatalf("%s returned %d matches of %s before the change, want %d", surface.name, len(before[surface.name]), alphaID, localProofSessionRows)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), legacyAdminTimeout)
	defer cancel()
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("dial Milvus: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	collectionName := occurrenceCollection(t, ctx, client)
	cleared := clearConversationIdentity(t, ctx, client, collectionName, alphaID)
	if count := nullIdentityCount(t, ctx, client, collectionName); count != int64(cleared) {
		t.Fatalf("collection %s stores %d rows with a null %s, want %d", collectionName, count, legacyConversationColumn, cleared)
	}

	for _, surface := range sandboxDaemon.surfaces() {
		deadline := time.Now().Add(localProofReadyTimeout)
		var after []localProofMatchKey
		for {
			after = conversationKeys(mustLocalProofSearch(t, surface, request).Matches, alphaID)
			if slices.Equal(after, before[surface.name]) || time.Now().After(deadline) {
				break
			}
			time.Sleep(localProofPollDelay)
		}
		if !slices.Equal(after, before[surface.name]) {
			t.Fatalf("%s returned %v for %s after its rows lost the %s value, want %v", surface.name, after, alphaID, legacyConversationColumn, before[surface.name])
		}
	}
	if count := nullIdentityCount(t, ctx, client, collectionName); count != int64(cleared) {
		t.Fatalf("collection %s stores %d rows with a null %s after the searches, want %d", collectionName, count, legacyConversationColumn, cleared)
	}
	t.Logf("CLI and MCP search returned all %d matches of %s after its %d rows kept the identity only in metadata", localProofSessionRows, alphaID, cleared)
}
