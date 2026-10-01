//go:build live

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/lm-semantic-search/library"
	"google.golang.org/grpc/codes"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

const embeddedQueryMilvusAddress = "localhost:39630"

// TestEmbeddedQueryBoundary searches the production source against a real
// SQLite catalog, Milvus vector pool, and local embedding endpoint.
func TestEmbeddedQueryBoundary(t *testing.T) {
	store, semantic := openEmbeddedQueryTestStore(t)
	owners := []embeddedConversationOwner{
		embeddedQueryTestOwner("codex:z-root", "/test/project", false, false),
		embeddedQueryTestOwner("codex:z-child", "/test/project/child", false, false),
		embeddedQueryTestOwner("codex:a-archived", "/test/project", true, false),
		embeddedQueryTestOwner("codex:a-subagent", "/test/project", false, true),
		embeddedQueryTestOwner("codex:a-sibling", "/test/project-sibling", false, false),
		embeddedQueryTestOwner("codex:a-old-profile", "/test/project", false, false),
		embeddedQueryTestOwner("codex:a-p2-profile", "/test/project", false, false),
	}
	owners[len(owners)-2].ProjectionProfile = "p1|" + owners[len(owners)-2].LoadRules
	owners[len(owners)-1].ProjectionProfile = "p2|" + owners[len(owners)-1].LoadRules
	for _, owner := range owners {
		publishEmbeddedQueryTestOwner(t, store.library, semantic.CollectionID, owner)
	}
	source := &embeddedConversationSearchSource{library: store.library, semantic: semantic}
	options := conversation.SearchConversationsOptions{Query: "embedded query boundary", Limit: 1, WorkspaceRoot: "/test/project"}
	first, err := source.SearchConversations(t.Context(), options)
	if err != nil || first.ReturnedCount != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page = %+v, %v, want one occurrence and continuation", first, err)
	}
	if first.Matches[0].ContextState != conversation.SearchContextStateUnavailable || first.Matches[0].ContextWindow != "embedded query boundary" {
		t.Fatalf("missing artifact excerpt/context = %+v", first.Matches[0])
	}
	// A later publication must not change the cursor's ordered occurrence set.
	publishEmbeddedQueryTestOwner(t, store.library, semantic.CollectionID, embeddedQueryTestOwner("codex:z-appended", "/test/project", false, false))
	all := append([]conversation.SearchMatch{}, first.Matches...)
	page := first
	for page.HasMore {
		options.Cursor = page.NextCursor
		options.Offset = page.NextOffset
		options.Limit = 2
		page, err = source.SearchConversations(t.Context(), options)
		if err != nil || len(page.Matches) == 0 {
			t.Fatalf("continued page = %+v, %v", page, err)
		}
		all = append(all, page.Matches...)
	}
	if len(all) != 6 {
		t.Fatalf("cursor traversal returned %d occurrences, want six committed before publication", len(all))
	}
	seen := make(map[string]bool, len(all))
	for _, match := range all {
		key := fmt.Sprintf("%s/%d", match.Record.ID, match.MessageIndex)
		if seen[key] || (match.Record.ID != "codex:z-root" && match.Record.ID != "codex:z-child") {
			t.Fatalf("unexpected or repeated occurrence %s", key)
		}
		seen[key] = true
	}
	options.Cursor, options.Offset, options.Limit = "", 0, 50
	fresh, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(fresh.Matches) != 9 || fresh.HasMore {
		t.Fatalf("fresh page = %+v, %v, want nine eligible occurrences", fresh, err)
	}
	options.Query = "embedded\x00query boundary"
	normalized, err := source.SearchConversations(t.Context(), options)
	if err != nil || !slices.EqualFunc(normalized.Matches, fresh.Matches, sameEmbeddedQueryMatch) {
		t.Fatalf("NUL query returned %d, %v, want the space-normalized ranking", len(normalized.Matches), err)
	}
	options.Query = "embedded query boundary"
	options.Offset, options.Limit = 2, 3
	offsetPage, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(offsetPage.Matches) != 3 || !slices.EqualFunc(offsetPage.Matches, fresh.Matches[2:5], sameEmbeddedQueryMatch) {
		t.Fatalf("offset page = %+v, %v, want the same ranked slice", offsetPage, err)
	}
	options.Offset, options.Limit, options.FromUnix, options.UntilUnix = 0, 50, 11, 12
	bounded, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(bounded.Matches) != 3 {
		t.Fatalf("half-open timestamp filter returned %d, %v, want three", len(bounded.Matches), err)
	}
	options.FromUnix, options.UntilUnix, options.PerConversationLimit = 0, 0, 1
	grouped, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(grouped.Matches) != 3 {
		t.Fatalf("group cap returned %d, %v, want three", len(grouped.Matches), err)
	}
	options.PerConversationLimit, options.IncludeArchived = 0, true
	archived, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(archived.Matches) != 12 {
		t.Fatalf("archive inclusion returned %d, %v, want twelve", len(archived.Matches), err)
	}
	options.IncludeArchived, options.ConversationID = false, "codex:z-root"
	scoped, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(scoped.Matches) != 3 {
		t.Fatalf("explicit owner filter returned %d, %v, want three", len(scoped.Matches), err)
	}
	options.Cursor = first.NextCursor
	_, err = source.SearchConversations(t.Context(), options)
	var failure conversationSearchSourceError
	if !errors.As(err, &failure) || failure.grpcCode() != codes.InvalidArgument || !errors.Is(err, library.ErrCursorMismatch) {
		t.Fatalf("changed filter cursor error = %v, want InvalidArgument wrapping ErrCursorMismatch", err)
	}
	if count := countLiveMilvusVectors(t, semantic); count != 1 {
		t.Fatalf("canonical vector count = %d, want one for repeated content", count)
	}
	semantic.IncludeSubagents = true
	source.semantic = semantic
	membership, err := source.SearchConversations(t.Context(), conversation.SearchConversationsOptions{Query: "embedded query boundary", Limit: 50, ConversationIDs: []string{"codex:z-root", "codex:z-child"}})
	if err != nil || len(membership.Matches) != 6 {
		t.Fatalf("multi-owner membership returned %d, %v", len(membership.Matches), err)
	}
	excluded, err := source.SearchConversations(t.Context(), conversation.SearchConversationsOptions{Query: "embedded query boundary", Limit: 50, ConversationID: "codex:a-subagent"})
	if err != nil || len(excluded.Matches) != 0 {
		t.Fatalf("default subagent exclusion returned %d, %v", len(excluded.Matches), err)
	}
	included, err := source.SearchConversations(t.Context(), conversation.SearchConversationsOptions{Query: "embedded query boundary", Limit: 50, ConversationID: "codex:a-subagent", IncludeSubagents: true})
	if err != nil || len(included.Matches) != 3 {
		t.Fatalf("explicit subagent inclusion returned %d, %v", len(included.Matches), err)
	}
	for _, profile := range []config.ConversationProjectionProfile{config.ConversationProjectionProfileLegacy, config.ConversationProjectionProfileOriginal} {
		legacySemantic := semantic
		legacySemantic.ProjectionProfile = profile
		legacy := &embeddedConversationSearchSource{library: store.library, semantic: legacySemantic}
		legacyPage, err := legacy.SearchConversations(t.Context(), conversation.SearchConversationsOptions{Query: "embedded query boundary", Limit: 10})
		if err != nil || len(legacyPage.Matches) != 3 {
			t.Fatalf("explicit %s search = %+v, %v, want three retained occurrences", profile, legacyPage, err)
		}
		expectedID := "codex:a-old-profile"
		if profile == config.ConversationProjectionProfileOriginal {
			expectedID = "codex:a-p2-profile"
		}
		for _, match := range legacyPage.Matches {
			if match.Record.ID != expectedID || match.ContextState != conversation.SearchContextStateUnavailable || match.SourceIdentity != nil || match.Snippet != "embedded query boundary" {
				t.Fatalf("legacy excerpt = %+v, want original stored excerpt without source identity", match)
			}
		}
	}
}

func sameEmbeddedQueryMatch(left, right conversation.SearchMatch) bool {
	return left.Record.ID == right.Record.ID && left.MessageIndex == right.MessageIndex && left.Score == right.Score
}

func embeddedQueryTestOwner(id, workspace string, archived, subagent bool) embeddedConversationOwner {
	loadRules := conversation.LoadRulesTag(defaultSemanticContentKinds())
	return embeddedConversationOwner{
		ConversationID: id, Provider: "codex", WorkspaceRoot: workspace,
		Archived: archived, Subagent: subagent, LoadRules: loadRules,
		ProjectionProfile: searchbackend.ProjectionProfile(loadRules),
	}
}

func publishEmbeddedQueryTestOwner(t *testing.T, store *library.Library, namespace string, owner embeddedConversationOwner) {
	t.Helper()
	var rows []library.Occurrence
	for index := range 3 {
		parts, err := embeddedFieldOccurrences(t.Context(), owner, searchbackend.Field{
			Key: fmt.Sprintf("%s/m%d/chat", owner.ProjectionProfile, index), MessageIndex: index,
			Role: "assistant", Timestamp: time.Unix(int64(10+index), 0), Kind: searchbackend.FieldKindChat,
			Text: "embedded query boundary", ToolIndex: -1,
		})
		if err != nil {
			t.Fatalf("prepare owner occurrences: %v", err)
		}
		rows = append(rows, parts...)
	}
	_, err := store.Apply(t.Context(), library.Batch{
		Namespace: namespace, OwnerID: owner.ConversationID, GenerationOrder: 1,
		IdempotencyToken: owner.ConversationID, Mode: library.Append, Rows: rows,
	})
	if err != nil {
		t.Fatalf("publish owner %s: %v", owner.ConversationID, err)
	}
}

func openEmbeddedQueryTestStore(t *testing.T) (*embeddedConversationStore, config.ConversationSemanticConfig) {
	t.Helper()
	admin, err := milvusclient.New(t.Context(), &milvusclient.ClientConfig{Address: embeddedQueryMilvusAddress})
	if err != nil {
		t.Fatalf("connect to isolated Milvus: %v", err)
	}
	t.Cleanup(func() { closeLiveMilvusAdmin(t, admin) })
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate isolated database name: %v", err)
	}
	database := "clyde_query_" + hex.EncodeToString(random)
	existing, err := admin.ListDatabase(t.Context(), milvusclient.NewListDatabaseOption())
	if err != nil || slices.Contains(existing, database) {
		t.Fatalf("verify isolated query database absence: %v, exists=%t", err, slices.Contains(existing, database))
	}
	registerLiveMilvusDatabase(t, database, embeddedQueryMilvusAddress)
	t.Cleanup(func() { dropLiveMilvusDatabase(t, admin, database) })
	if err := admin.CreateDatabase(t.Context(), milvusclient.NewCreateDatabaseOption(database)); err != nil {
		t.Fatalf("create isolated query database: %v", err)
	}
	root := t.TempDir()
	semantic := config.ConversationSemanticConfig{
		ProjectionProfile: config.ConversationProjectionProfileSourceSpan,
		SearchEnabled:     true, CollectionID: liveCollectionID,
		CatalogPath: filepath.Join(root, "catalog.sqlite"), LockPath: filepath.Join(root, "catalog.lock"), PoolID: "query-live",
		MilvusAddress: embeddedQueryMilvusAddress, MilvusDatabase: database, MilvusCollection: "query_vectors",
		EmbeddingBaseURL: liveEmbeddingBaseURL, EmbeddingModel: liveEmbeddingModel, EmbeddingRevision: "query-live",
		VectorDimension: liveEmbeddingDimension, Normalization: "l2", QueryTimeout: config.Duration(time.Minute),
	}
	outboxPath := filepath.Join(root, "outbox.sqlite")
	lock, err := lockConversationSemanticOutbox(t.Context(), outboxPath)
	if err != nil {
		t.Fatalf("lock isolated outbox: %v", err)
	}
	store, err := openEmbeddedConversationStore(t.Context(), semantic, outboxPath, lock, slog.Default())
	if err != nil {
		t.Fatalf("open isolated query store: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		if err := store.close(ctx); err != nil {
			t.Errorf("close isolated query store: %v", err)
		}
	})
	return store, semantic
}
