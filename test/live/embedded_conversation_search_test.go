//go:build live

package live

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
)

func TestLiveEmbeddedConversationPublicSearch(t *testing.T) {
	home, path, expected := writeEmbeddedPublicCorpus(t)
	h, configuration := newEmbeddedPublicHarness(t, home)
	h.boot(t)
	waitEmbeddedPublicPublication(t, h)
	if _, err := embeddedPublicCLI(t, h, home, "", 50); err == nil {
		t.Fatal("ingestion-only configuration accepted semantic search")
	}
	h.teardown(t)
	configuration.Conversation.Semantic.IngestionEnabled = false
	configuration.Conversation.Semantic.SearchEnabled = true
	writeEmbeddedLifecycleConfig(t, h, configuration)
	h.boot(t)
	waitEmbeddedPublicSearch(t, h, home)
	cli, cliHits := readEmbeddedPublicCLI(t, h, home, expected)
	connection := embeddedPublicMCP(t, h)
	cursor := ""
	mcpIDs := make([]string, 0, len(expected))
	mcpHits := make([]embeddedPublicHit, 0, len(expected))
	for pageIndex := 0; pageIndex <= len(expected); pageIndex++ {
		page := embeddedPublicMCPPage(t, connection, cursor, 3)
		mcpIDs = append(mcpIDs, validateEmbeddedPublicPage(t, page, expected)...)
		mcpHits = append(mcpHits, page.Matches...)
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			t.Fatal("MCP cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(cli, mcpIDs) {
		t.Fatalf("CLI/MCP ordered identities differ: %v / %v", cli, mcpIDs)
	}
	if !reflect.DeepEqual(cliHits, mcpHits) {
		t.Fatalf("CLI/MCP source identities, scores, excerpts or context differ: %+v / %+v", cliHits, mcpHits)
	}
	verifyEmbeddedPublicFilters(t, h, connection, expected)
	closeEmbeddedPublicMCP(t, connection)
	appendEmbeddedPublicSource(t, path)
	// A read through the public transcript boundary verifies that the appended
	// source exists independently of the retained search publication.
	transcript, err := h.runCLI(t, home, "conversation", "search", embeddedPublicConversation)
	if err != nil || !strings.Contains(transcript, "search-only unpublished append") {
		t.Fatalf("public transcript did not read append: %v: %s", err, transcript)
	}
	if got := traverseEmbeddedPublicCLI(t, h, home, expected); !slices.Equal(cli, got) {
		t.Fatalf("search-only mode changed committed search: %v / %v", cli, got)
	}
	h.teardown(t)
	h.boot(t)
	waitEmbeddedPublicSearch(t, h, home)
	if got := traverseEmbeddedPublicCLI(t, h, home, expected); !slices.Equal(cli, got) {
		t.Fatalf("restart changed ordered search identities: %v / %v", cli, got)
	}
}

func waitEmbeddedPublicSearch(t *testing.T, h *harness, home string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lastErr = embeddedPublicCLI(t, h, home, "", 1)
		if lastErr == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("public search did not start: %v; logs: %s", lastErr, h.dumpLogsOnFailure(t))
}

func waitEmbeddedPublicPublication(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	marker := fmt.Sprintf(`"searchable_rows":%d`, embeddedPublicRows)
	for time.Now().Before(deadline) {
		if h.logContains(marker) && h.logContains(embeddedPublicConversation) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("ingestion-only publication did not complete; logs: %s", h.dumpLogsOnFailure(t))
}

func traverseEmbeddedPublicCLI(t *testing.T, h *harness, home string, expected map[string]string) []string {
	t.Helper()
	identities, _ := readEmbeddedPublicCLI(t, h, home, expected)
	return identities
}

func readEmbeddedPublicCLI(t *testing.T, h *harness, home string, expected map[string]string) ([]string, []embeddedPublicHit) {
	t.Helper()
	cursor := ""
	identities := make([]string, 0, len(expected))
	hits := make([]embeddedPublicHit, 0, len(expected))
	seen := make(map[string]bool, len(expected))
	for pageIndex := 0; pageIndex <= len(expected); pageIndex++ {
		page, err := embeddedPublicCLI(t, h, home, cursor, 5)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, page.Matches...)
		for _, identity := range validateEmbeddedPublicPage(t, page, expected) {
			if seen[identity] {
				t.Fatalf("cursor repeated %s", identity)
			}
			seen[identity] = true
			identities = append(identities, identity)
		}
		if !page.HasMore {
			if len(identities) != len(expected) {
				t.Fatalf("traversal returned %d of %d expected occurrences", len(identities), len(expected))
			}
			return identities, hits
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			t.Fatal("CLI cursor did not advance")
		}
		cursor = page.NextCursor
	}
	t.Fatal("cursor traversal did not terminate")
	return nil, nil
}

func validateEmbeddedPublicPage(t *testing.T, page embeddedPublicPage, expected map[string]string) []string {
	t.Helper()
	if page.ReturnedCount != len(page.Matches) || (page.HasMore && len(page.Matches) == 0) {
		t.Fatalf("invalid public page: %+v", page)
	}
	identities := make([]string, 0, len(page.Matches))
	for _, hit := range page.Matches {
		identity := hit.SourceIdentity
		if identity == nil {
			t.Fatalf("public hit omitted source identity: %+v", hit)
		}
		key := embeddedPublicIdentity(identity.MessageIndex, identity.ContentKind, identity.ToolIndex, int(identity.SourceByteStart), int(identity.SourceByteEnd))
		snippet, found := expected[key]
		if !found || hit.Snippet != snippet || hit.MessageIndex != identity.MessageIndex || identity.ConversationID != hit.Conversation.ID || hit.Conversation.ID != embeddedPublicConversation || hit.LoadRules != "v1;" {
			t.Fatalf("unexpected source occurrence: %+v", hit)
		}
		identities = append(identities, key)
	}
	return identities
}

func appendEmbeddedPublicSource(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(file, pagingClaudeRecord(embeddedPublicSession, 35, "search-only unpublished append")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func verifyEmbeddedPublicFilters(t *testing.T, h *harness, connection *client.Client, expected map[string]string) {
	t.Helper()
	for _, selected := range []struct {
		ids   string
		cap   string
		count int
	}{
		{ids: "", cap: "0", count: 0},
		{ids: "claude:not-stored", cap: "0", count: 0},
		{ids: embeddedPublicConversation + ",claude:not-stored", cap: "0", count: len(expected)},
		{ids: embeddedPublicConversation, cap: "2", count: 2},
	} {
		page, err := embeddedPublicCLIArguments(t, h, []string{"conversation", "search", "--query", embeddedPublicQuery, "--limit", "100", "--conversation-ids=" + selected.ids, "--per-conversation-limit=" + selected.cap, "--output-format", "json"})
		if err != nil || len(page.Matches) != selected.count || page.HasMore {
			t.Fatalf("CLI membership %q/cap %s returned %+v, %v", selected.ids, selected.cap, page, err)
		}
		validateEmbeddedPublicPage(t, page, expected)
		ids := "[]"
		if selected.ids != "" {
			values := strings.Split(selected.ids, ",")
			ids = "["
			for index, value := range values {
				if index > 0 {
					ids += ","
				}
				ids += fmt.Sprintf("%q", value)
			}
			ids += "]"
		}
		arguments := fmt.Sprintf(`{"query":%q,"conversation_ids":%s,"per_conversation_limit":%s,"limit":100}`, embeddedPublicQuery, ids, selected.cap)
		mcpPage := embeddedPublicMCPArguments(t, connection, []byte(arguments))
		if len(mcpPage.Matches) != selected.count || mcpPage.HasMore {
			t.Fatalf("MCP membership %q/cap %s returned %+v", selected.ids, selected.cap, mcpPage)
		}
		if !slices.Equal(validateEmbeddedPublicPage(t, page, expected), validateEmbeddedPublicPage(t, mcpPage, expected)) {
			t.Fatal("filtered CLI/MCP identities differ")
		}
	}
}
