//go:build live

package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"goodkind.io/clyde/internal/sandbox"
	"goodkind.io/clyde/internal/searchacceptance"
)

func TestLiveEmbeddedSearchMeasurementCollector(t *testing.T) {
	home, _, _ := writeEmbeddedPublicCorpus(t)
	h, configuration := newEmbeddedPublicHarness(t, home)
	h.boot(t)
	waitEmbeddedPublicPublication(t, h)
	h.teardown(t)
	configuration.Conversation.Semantic.IngestionEnabled = false
	configuration.Conversation.Semantic.SearchEnabled = true
	writeEmbeddedLifecycleConfig(t, h, configuration)
	h.boot(t)
	waitEmbeddedPublicSearch(t, h, home)
	roots := sandbox.Roots{
		Base: filepath.Dir(h.stateRoot), State: h.stateRoot, Config: h.configRoot,
		Cache: h.cacheRoot, Runtime: h.runtimeRoot,
	}
	query := searchacceptance.Query{
		ID: "public-source-spans", Query: embeddedPublicQuery, PageSize: 3,
		Filter:                searchacceptance.Filter{ConversationIDs: []string{embeddedPublicConversation}},
		ExpectedOccurrenceIDs: acceptanceSourceIdentities(t), ExpectedTotal: embeddedPublicRows,
	}
	var traversals []searchacceptance.Traversal
	var previous []string
	for _, pageSize := range []int{3, 5} {
		query.PageSize = pageSize
		traversal, err := searchacceptance.ReadCLITraversal(t.Context(), h.binPath, roots, home, query, false, 30000)
		if err != nil {
			t.Fatal(err)
		}
		var ordered []string
		for _, page := range traversal.Pages {
			ordered = append(ordered, page.OccurrenceIDs...)
		}
		if previous != nil && !slices.Equal(previous, ordered) {
			t.Fatal("measurement collector changed source order across page sizes")
		}
		previous = ordered
		traversals = append(traversals, traversal)
	}
	body, err := json.MarshalIndent(traversals, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.stateRoot, "measured-public-traversals.json")
	if requested := os.Getenv("CLYDE_MEASUREMENT_REPORT"); requested != "" {
		if !filepath.IsAbs(requested) || !sandbox.UnderTempRoot(requested) {
			t.Fatal("measurement artifact requires an absolute temporary path")
		}
		path = requested
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("measured warm public traversal artifact: %s", path)
}

func acceptanceSourceIdentities(t *testing.T) []string {
	t.Helper()
	identities := make([]searchacceptance.SourceIdentity, 0, embeddedPublicRows)
	for index := range embeddedPublicChatRows {
		text := fmt.Sprintf("%s source %03d", embeddedPublicQuery, index)
		identities = append(identities, searchacceptance.SourceIdentity{
			ConversationID: embeddedPublicConversation, MessageIndex: index,
			ContentKind: "chat", ToolIndex: -1, SourceByteEnd: int64(len(text)),
		})
	}
	for _, span := range [][2]int64{{0, 3686}, {3686, 7372}, {7372, 8000}} {
		identities = append(identities, searchacceptance.SourceIdentity{
			ConversationID: embeddedPublicConversation, MessageIndex: 32, ContentKind: "chat",
			ToolIndex: -1, SourceByteStart: span[0], SourceByteEnd: span[1],
		})
	}
	for index := range 2 {
		identities = append(identities, searchacceptance.SourceIdentity{
			ConversationID: embeddedPublicConversation, MessageIndex: 33, ContentKind: "tool_call",
			ToolIndex: index, SourceByteEnd: int64(len("printf 'public embedded search checkpoint'")),
		})
	}
	identities = append(identities, searchacceptance.SourceIdentity{
		ConversationID: embeddedPublicConversation, MessageIndex: 34, ContentKind: "chat",
		ToolIndex: -1, SourceByteEnd: int64(len("public embedded search checkpoint closed")),
	})
	keys := make([]string, 0, len(identities))
	for _, identity := range identities {
		key, err := searchacceptance.IdentityKey(identity)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	return keys
}
