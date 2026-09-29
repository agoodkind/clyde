//go:build live

package live

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
)

const (
	// pagingQuery shares every word with the archived rows and no word with
	// the eligible rows. requireArchivedRowsRankEarly checks the resulting
	// rank of the archived rows.
	pagingQuery = "archived paging probe checkpoint"
	// pagingEligibleConversations times pagingEligibleMessages is the number
	// of rows the default search returns: 2,010, which pages past offset 2,000.
	pagingEligibleConversations = 10
	pagingEligibleMessages      = 201
	// pagingArchivedMessages is the number of rows in one archived Codex
	// rollout. The feeder indexes archived conversations, and the default
	// search withholds them after the engine ranks them.
	pagingArchivedMessages = 30
	// pagingLargePageSize is the largest page the search accepts. The test
	// pages at this size below pagingSinglePageFromOffset and at size 1 from
	// that offset on.
	pagingLargePageSize        = 50
	pagingSinglePageFromOffset = 1900
	// pagingMaxPages bounds the page count of one paging run.
	pagingMaxPages          = 400
	pagingArtifactAge       = 2 * time.Hour
	pagingReadyTimeout      = 15 * time.Minute
	pagingReadyPollInterval = 10 * time.Second
	pagingSearchTimeout     = 2 * time.Minute
	pagingArchivedThreadID  = "019de9bb-3a00-7010-bd9f-a6ee71559357"
	pagingArchivedID        = "codex:" + pagingArchivedThreadID
)

var (
	pagingVerbs      = []string{"reviewed", "renamed", "measured", "rebuilt", "documented", "profiled", "migrated"}
	pagingAdjectives = []string{"stale", "nested", "cached", "flaky", "sparse", "verbose", "atomic", "legacy", "sorted", "signed", "pinned"}
	pagingNouns      = []string{
		"parser", "socket", "schema", "worker", "ledger", "template", "manifest", "resolver", "listener", "tokenizer",
		"catalog", "exporter", "watcher", "registry", "renderer", "reducer", "scheduler", "validator", "adapter", "decoder",
		"encoder", "migrator", "collector", "throttle", "splitter", "fixture", "harness", "router", "profile",
	}
)

// pagingRowKey identifies one searchable row.
type pagingRowKey struct {
	conversationID string
	messageIndex   int64
}

// TestSearchPagingAdvancesPastHiddenRowsInTheOverfetchWindow pages the public
// daemon search over a corpus where archived rows rank inside the first 2,000
// engine rows. The stack is the Clyde daemon from this worktree, the
// lm-semantic-search daemon at the go.mod version, a Milvus database that the
// test creates, and the real embedding endpoint. Every page must return rows
// or end the results, next_offset must grow, and every eligible row must
// appear exactly once.
func TestSearchPagingAdvancesPastHiddenRowsInTheOverfetchWindow(t *testing.T) {
	t.Logf("embedding window start %s", time.Now().UTC().Format(time.RFC3339))
	database := createLiveMilvusDatabase(t)
	proxy := startEmbeddingCountingProxy(t, liveEmbeddingBaseURL)
	t.Cleanup(func() {
		counts := proxy.snapshot()
		t.Logf("embedding window end %s: %d embedding requests, %d embedding inputs",
			time.Now().UTC().Format(time.RFC3339), counts.Requests, counts.Inputs)
	})
	engine := startLMSMilvusDaemon(t, database, proxy.baseURL(liveEmbeddingPath))

	home := t.TempDir()
	eligible := writePagingEligibleTranscripts(t, home)
	writePagingArchivedRollout(t, home)
	h := newHarness(t)
	h.conversationSemantic.IngestionEnabled = true
	h.conversationSemantic.SearchEnabled = true
	h.extraEnv = []string{"HOME=" + home}
	h.writeConversationOnlyConfig(t, nil, engine.socketPath)
	h.boot(t)
	search := dialPagingSearch(t, h)

	totalRows := len(eligible) + pagingArchivedMessages
	waitForPagingCorpus(t, search, totalRows)
	counts := proxy.snapshot()
	t.Logf("corpus searchable: %d rows, %d embedding requests, %d embedding inputs so far", totalRows, counts.Requests, counts.Inputs)
	requireArchivedRowsRankEarly(t, search)

	seen := pageEveryEligibleRow(t, search)
	missing := 0
	for key := range eligible {
		if _, found := seen[key]; !found {
			missing++
			if missing <= 5 {
				t.Errorf("eligible row %s message %d never appeared", key.conversationID, key.messageIndex)
			}
		}
	}
	for key, offset := range seen {
		if _, found := eligible[key]; !found {
			t.Errorf("page at offset %d returned unexpected row %s message %d", offset, key.conversationID, key.messageIndex)
		}
	}
	if missing > 0 || len(seen) != len(eligible) {
		t.Fatalf("paging returned %d distinct rows with %d eligible rows missing, want all %d eligible rows", len(seen), missing, len(eligible))
	}
}

// pageEveryEligibleRow follows next_offset from offset 0 until has_more is
// false and returns each row with the offset of the page that returned it. It
// fails on a page with no row and has_more true, on a next_offset that does not
// grow, on an archived row, and on a row returned twice.
func pageEveryEligibleRow(t *testing.T, search clydev1.ClydeServiceClient) map[pagingRowKey]int {
	t.Helper()

	seen := make(map[pagingRowKey]int)
	offset := 0
	for pageCount := 1; ; pageCount++ {
		if pageCount > pagingMaxPages {
			t.Fatalf("paging did not end after %d pages; last offset %d", pagingMaxPages, offset)
		}
		pageSize := pagingLargePageSize
		if offset >= pagingSinglePageFromOffset {
			pageSize = 1
		}
		response := searchPagingPage(t, search, offset, pageSize, false)
		matches := response.GetMatches()
		hasMore := response.GetHasMore()
		nextOffset := int(response.GetNextOffset())
		if len(matches) == 0 && hasMore {
			t.Fatalf("page at offset %d limit %d returned 0 rows with has_more true and next_offset %d", offset, pageSize, nextOffset)
		}
		if nextOffset != offset+len(matches) {
			t.Fatalf("page at offset %d returned %d rows and next_offset %d, want %d", offset, len(matches), nextOffset, offset+len(matches))
		}
		if hasMore && nextOffset <= offset {
			t.Fatalf("page at offset %d returned has_more true and next_offset %d, want next_offset above the request offset", offset, nextOffset)
		}
		for _, match := range matches {
			key := pagingRowKey{conversationID: match.GetConversation().GetId(), messageIndex: match.GetMessageIndex()}
			if key.conversationID == pagingArchivedID {
				t.Fatalf("page at offset %d returned archived row %s message %d", offset, key.conversationID, key.messageIndex)
			}
			if firstOffset, repeated := seen[key]; repeated {
				t.Fatalf("page at offset %d repeated row %s message %d first returned at offset %d", offset, key.conversationID, key.messageIndex, firstOffset)
			}
			seen[key] = offset
		}
		if !hasMore {
			t.Logf("paging ended after %d pages at offset %d with %d rows", pageCount, offset, len(seen))
			return seen
		}
		offset = nextOffset
	}
}

// waitForPagingCorpus polls the search with archived rows included until the
// engine ranks every fixture row, then checks that no further row exists.
func waitForPagingCorpus(t *testing.T, search clydev1.ClydeServiceClient, totalRows int) {
	t.Helper()

	deadline := time.Now().Add(pagingReadyTimeout)
	for {
		// The engine refuses a search until the feeder delivers the first
		// documents. The loop retries a refused search until the deadline.
		response, err := trySearchPagingPage(t, search, totalRows-1, 1, true)
		if err == nil && len(response.GetMatches()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine did not rank all %d fixture rows within %s; last error: %v", totalRows, pagingReadyTimeout, err)
		}
		time.Sleep(pagingReadyPollInterval)
	}
	response := searchPagingPage(t, search, totalRows, 1, true)
	if len(response.GetMatches()) != 0 || response.GetHasMore() {
		t.Fatalf("search past the %d fixture rows returned %d rows and has_more %t, want 0 rows and has_more false",
			totalRows, len(response.GetMatches()), response.GetHasMore())
	}
}

// requireArchivedRowsRankEarly fails unless the first page of the search with
// archived rows included returns an archived row. That page lies inside the
// first 2,000 ranked rows.
func requireArchivedRowsRankEarly(t *testing.T, search clydev1.ClydeServiceClient) {
	t.Helper()

	response := searchPagingPage(t, search, 0, pagingLargePageSize, true)
	archived := 0
	for _, match := range response.GetMatches() {
		if match.GetConversation().GetId() == pagingArchivedID {
			archived++
		}
	}
	if archived == 0 {
		t.Fatalf("the first %d ranked rows include no archived row; the corpus must rank hidden rows inside the over-fetch window", pagingLargePageSize)
	}
	t.Logf("the first %d ranked rows include %d archived rows", pagingLargePageSize, archived)
}

func dialPagingSearch(t *testing.T, h *harness) clydev1.ClydeServiceClient {
	t.Helper()

	target := "unix://" + filepath.Join(h.runtimeRoot, "clyde", "daemon.sock")
	connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("connect to the daemon search RPC: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return clydev1.NewClydeServiceClient(connection)
}

func searchPagingPage(t *testing.T, search clydev1.ClydeServiceClient, offset int, limit int, includeArchived bool) *clydev1.SearchConversationsResponse {
	t.Helper()

	response, err := trySearchPagingPage(t, search, offset, limit, includeArchived)
	if err != nil {
		t.Fatalf("search at offset %d limit %d include_archived %t: %v", offset, limit, includeArchived, err)
	}
	return response
}

func trySearchPagingPage(t *testing.T, search clydev1.ClydeServiceClient, offset int, limit int, includeArchived bool) (*clydev1.SearchConversationsResponse, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), pagingSearchTimeout)
	defer cancel()
	response, err := search.SearchConversations(ctx, &clydev1.SearchConversationsRequest{
		Query:           pagingQuery,
		Limit:           int64(limit),
		Offset:          int64(offset),
		IncludeArchived: includeArchived,
	})
	if err != nil {
		return nil, fmt.Errorf("search at offset %d limit %d include_archived %t: %w", offset, limit, includeArchived, err)
	}
	return response, nil
}

// writePagingEligibleTranscripts writes Claude transcripts for the rows the
// default search returns and returns the row set. Each message has distinct
// text and its own embedding.
func writePagingEligibleTranscripts(t *testing.T, home string) map[pagingRowKey]struct{} {
	t.Helper()

	projectDir := filepath.Join(home, ".claude", "projects", "-tmp-search-paging-live")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir fixture project dir: %v", err)
	}
	rows := make(map[pagingRowKey]struct{}, pagingEligibleConversations*pagingEligibleMessages)
	for conversationNumber := range pagingEligibleConversations {
		sessionID := fmt.Sprintf("aaaaaaaa-bbbb-4ccc-8ddd-%012d", conversationNumber)
		lines := make([]string, 0, pagingEligibleMessages)
		for messageIndex := range pagingEligibleMessages {
			rowNumber := conversationNumber*pagingEligibleMessages + messageIndex
			lines = append(lines, pagingClaudeRecord(sessionID, messageIndex, pagingEligibleText(rowNumber)))
			rows[pagingRowKey{conversationID: "claude:" + sessionID, messageIndex: int64(messageIndex)}] = struct{}{}
		}
		path := filepath.Join(projectDir, sessionID+".jsonl")
		writeAgedFixture(t, path, lines)
	}
	return rows
}

// writePagingArchivedRollout writes one Codex rollout under the archived
// sessions directory. Clyde marks it archived, the feeder indexes its rows,
// and the default search withholds them.
func writePagingArchivedRollout(t *testing.T, home string) {
	t.Helper()

	archivedDir := filepath.Join(home, ".codex", "archived_sessions")
	if err := os.MkdirAll(archivedDir, 0o700); err != nil {
		t.Fatalf("mkdir archived Codex sessions dir: %v", err)
	}
	lines := []string{
		`{"timestamp":"2026-05-02T18:00:00.000Z","type":"session_meta","payload":{"id":"` + pagingArchivedThreadID + `","timestamp":"2026-05-02T18:00:00.000Z","cwd":"/tmp/search-paging-archived","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`,
	}
	for messageNumber := range pagingArchivedMessages {
		stamp := time.Date(2026, 5, 2, 18, 1, messageNumber, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
		kind := "user_message"
		if messageNumber%2 == 1 {
			kind = "agent_message"
		}
		lines = append(lines, fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":%q,"message":"archived paging probe checkpoint %d"}}`, stamp, kind, messageNumber))
	}
	path := filepath.Join(archivedDir, "rollout-2026-05-02T11-00-00-"+pagingArchivedThreadID+".jsonl")
	writeAgedFixture(t, path, lines)
}

// pagingClaudeRecord returns one Claude transcript record. Even indexes are
// user messages and odd indexes are assistant messages.
func pagingClaudeRecord(sessionID string, messageIndex int, text string) string {
	uuid := fmt.Sprintf("%08d-0000-4000-8000-000000000000", messageIndex)
	parent := ""
	if messageIndex > 0 {
		parent = fmt.Sprintf("%08d-0000-4000-8000-000000000000", messageIndex-1)
	}
	stamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(messageIndex) * time.Second).Format(time.RFC3339)
	head := fmt.Sprintf(`"sessionId":%q,"cwd":"/tmp/search-paging-live","uuid":%q,"parentUuid":%q,"timestamp":%q`, sessionID, uuid, parent, stamp)
	if messageIndex%2 == 0 {
		return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},%s}`, text, head)
	}
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`, text, head)
}

// pagingEligibleText returns distinct text for one eligible row. The text
// shares no word with pagingQuery.
func pagingEligibleText(rowNumber int) string {
	verb := pagingVerbs[rowNumber%len(pagingVerbs)]
	adjective := pagingAdjectives[(rowNumber/len(pagingVerbs))%len(pagingAdjectives)]
	noun := pagingNouns[(rowNumber/(len(pagingVerbs)*len(pagingAdjectives)))%len(pagingNouns)]
	return fmt.Sprintf("worklog %d: %s the %s %s", rowNumber, verb, adjective, noun)
}

// writeAgedFixture writes lines as a JSONL file and sets its modification
// time pagingArtifactAge in the past. The feeder defers a transcript that
// changed within its last interval.
func writeAgedFixture(t *testing.T, path string, lines []string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
	aged := time.Now().Add(-pagingArtifactAge)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatalf("age fixture %s: %v", path, err)
	}
}
