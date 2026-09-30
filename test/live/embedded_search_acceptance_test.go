//go:build live

package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/sandbox"
	"goodkind.io/clyde/internal/searchacceptance"
)

func TestLiveEmbeddedSearchMeasurementCollector(t *testing.T) {
	home, _, _ := writeEmbeddedPublicCorpus(t)
	h, configuration := newEmbeddedPublicHarness(t, home)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("measurement failure logs: %s", h.dumpLogsOnFailure(t))
		}
	})
	h.boot(t)
	waitEmbeddedPublicPublication(t, h)
	h.teardown(t)
	assertEmbeddedSourceMeasurements(t, h.stateRoot)
	initialRunID := assertEmbeddedIngestionOperations(t, h.stateRoot)
	h.boot(t)
	if !h.waitForDaemonLog(`"projection_unchanged_fields":36`, 20*time.Second) {
		t.Fatal("unchanged ingestion did not finish its real source pass")
	}
	h.teardown(t)
	assertEmbeddedUnchangedOperations(t, h.stateRoot, initialRunID)
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
	h.teardown(t)
	assertEmbeddedQueryOperations(t, h.stateRoot)
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

func assertEmbeddedSourceMeasurements(t *testing.T, stateRoot string) {
	t.Helper()
	found := false
	err := filepath.WalkDir(stateRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, filepath.Join("conversation", "semantic.jsonl")) {
			return nil
		}
		present, err := readEmbeddedSourceMeasurement(t, path)
		found = found || present
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("real ingestion did not publish complete source operation measurements")
	}
}

func readEmbeddedSourceMeasurement(t *testing.T, path string) (bool, error) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	found := false
	for scanner.Scan() {
		var event map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			slog.Warn("decode ingestion measurement log failed", "path", path, "err", err)
			return false, fmt.Errorf("decode measurement log %s: %w", path, err)
		}
		if !isEmbeddedSourceMeasurement(event) {
			continue
		}
		assertEmbeddedSourceDurations(t, event)
		found = true
	}
	return found, scanner.Err()
}

func isEmbeddedSourceMeasurement(event map[string]json.RawMessage) bool {
	var message string
	if err := json.Unmarshal(event["msg"], &message); err != nil || message != "daemon.conversation_semantic_sync.pass_completed" {
		return false
	}
	var rows int
	return json.Unmarshal(event["projection_rows"], &rows) == nil && rows == embeddedPublicRows
}

func assertEmbeddedSourceDurations(t *testing.T, event map[string]json.RawMessage) {
	t.Helper()
	for _, field := range []string{
		"source_read_us", "projection_us", "policy_selection_us", "committed_field_read_us",
		"field_selection_us", "occurrence_preparation_us", "outbox_preparation_us",
	} {
		var duration int64
		if err := json.Unmarshal(event[field], &duration); err != nil || duration < 0 {
			t.Fatalf("published ingestion measurement %s is absent or invalid", field)
		}
		if (field == "source_read_us" || field == "occurrence_preparation_us" || field == "outbox_preparation_us") && duration == 0 {
			t.Fatalf("published ingestion measurement %s recorded no duration", field)
		}
	}
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
