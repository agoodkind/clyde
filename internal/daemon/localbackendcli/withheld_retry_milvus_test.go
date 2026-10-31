//go:build milvusintegration

package localbackendcli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	withheldLimit       = 2
	withheldQuery       = "archived question 0 archived answer 0 archived"
	withheldSearchEvent = "conversation.vectorsearch.search_completed"
	withheldDatedAfter  = "2000-01-01"
	withheldEpochAfter  = "1970-01-01T00:00:00Z"
	withheldDatedCalls  = 2
	withheldLogWait     = 10 * time.Second
	withheldLogSettle   = 2 * time.Second
	withheldSettings    = `milvus_address = %q
milvus_database = "default"
milvus_layout = "shared"
embedding_base_url = %q
embedding_model = %q
embedding_dimension = %d
`
)

// searchCompletedRecords returns one description per distinct search_completed record logged at or after since under the state roots. Records with the same trace ID and time count once across log files.
func searchCompletedRecords(t *testing.T, stateRoots []string, since time.Time) []string {
	t.Helper()
	records := []string{}
	seen := map[string]bool{}
	for _, stateRoot := range stateRoots {
		walkErr := filepath.WalkDir(stateRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for line := range strings.Lines(string(content)) {
				if !strings.Contains(line, withheldSearchEvent) {
					continue
				}
				var record struct {
					Time       time.Time `json:"time"`
					Message    string    `json:"msg"`
					TraceID    string    `json:"trace_id"`
					Candidates int       `json:"candidates"`
					Hits       int       `json:"hits"`
				}
				if decodeErr := json.Unmarshal([]byte(line), &record); decodeErr != nil {
					return fmt.Errorf("decode %s record in %s: %w", withheldSearchEvent, path, decodeErr)
				}
				call := record.TraceID + " " + record.Time.Format(time.RFC3339Nano)
				if record.Message == withheldSearchEvent && !record.Time.Before(since) && !seen[call] {
					seen[call] = true
					records = append(records, fmt.Sprintf("%s trace=%s candidates=%d hits=%d time=%s", path, record.TraceID, record.Candidates, record.Hits, record.Time.Format(time.RFC3339Nano)))
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("read daemon logs under %s: %v", stateRoot, walkErr)
		}
	}
	return records
}

// settledSearchRecords waits up to withheldLogWait for the first search_completed record, then waits withheldLogSettle and returns the records.
func settledSearchRecords(t *testing.T, stateRoots []string, since time.Time) []string {
	t.Helper()
	deadline := time.Now().Add(withheldLogWait)
	records := searchCompletedRecords(t, stateRoots, since)
	for len(records) == 0 && time.Now().Before(deadline) {
		time.Sleep(localProofPollDelay)
		records = searchCompletedRecords(t, stateRoots, since)
	}
	time.Sleep(withheldLogSettle)
	return searchCompletedRecords(t, stateRoots, since)
}

// TestMilvusSearchSkipsFirstRetryForWithheldHits checks that CLI and MCP
// searches return a full page of matches excluding archived conversations in one
// engine call, and the same matches with an after date bound in two engine calls.
func TestMilvusSearchSkipsFirstRetryForWithheldHits(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	baseURL := os.Getenv(deletedSourceEmbeddingEnv)
	if baseURL == "" {
		baseURL = deletedSourceEmbeddingDefault
	}
	settingsPath := writeSandboxSemanticSettings(t, fmt.Sprintf(withheldSettings, address, baseURL, deletedSourceEmbeddingModel, deletedSourceEmbeddingDim))
	binaryPath := buildClydeCLI(t)
	home := writeLocalProofHome(t)
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	waitForProofRowsFrom(t, sandboxDaemon, localProofSourceSemantic)

	stateRoots := []string{filepath.Join(home, ".local", "state"), sandboxDaemon.roots.State}
	request := localProofSearchRequest{Query: withheldQuery, Limit: withheldLimit, Offset: 0, IncludeArchived: false}
	archivedRequest := request
	archivedRequest.IncludeArchived = true
	for _, surface := range sandboxDaemon.surfaces() {
		archived := mustLocalProofSearch(t, surface, archivedRequest)
		if len(archived.Matches) != withheldLimit || !archived.Matches[0].Conversation.Archived {
			t.Fatalf("%s with archived conversations returned %v, want the archived rollout first", surface.name, localProofMatchKeys(archived.Matches))
		}
		since := time.Now()
		result := mustLocalProofSearch(t, surface, request)
		searchRecords := settledSearchRecords(t, stateRoots, since)
		if result.Source != localProofSourceSemantic || len(result.Matches) != withheldLimit {
			t.Fatalf("%s returned source %q and %d matches, want %q and %d", surface.name, result.Source, len(result.Matches), localProofSourceSemantic, withheldLimit)
		}
		if slices.ContainsFunc(result.Matches, func(match localProofMatch) bool { return match.Conversation.Archived }) {
			t.Fatalf("%s returned an archived match without include-archived: %v", surface.name, localProofMatchKeys(result.Matches))
		}
		if len(searchRecords) != 1 {
			t.Fatalf("%s logged %d %s records for one request, want 1: %v", surface.name, len(searchRecords), withheldSearchEvent, searchRecords)
		}
		for _, after := range []string{withheldDatedAfter, withheldEpochAfter} {
			datedRequest := request
			datedRequest.After = after
			since = time.Now()
			dated := mustLocalProofSearch(t, surface, datedRequest)
			datedRecords := settledSearchRecords(t, stateRoots, since)
			if !slices.Equal(localProofMatchKeys(dated.Matches), localProofMatchKeys(result.Matches)) {
				t.Fatalf("%s with --after %s returned %v, want %v", surface.name, after, localProofMatchKeys(dated.Matches), localProofMatchKeys(result.Matches))
			}
			if len(datedRecords) != withheldDatedCalls {
				t.Fatalf("%s with --after %s logged %d %s records, want %d: %v", surface.name, after, len(datedRecords), withheldSearchEvent, withheldDatedCalls, datedRecords)
			}
			t.Logf("%s with --after %s made %d engine calls", surface.name, after, withheldDatedCalls)
		}
		t.Logf("%s returned %v with 1 engine call; with archived conversations it returned %v", surface.name, localProofMatchKeys(result.Matches), localProofMatchKeys(archived.Matches))
	}
}
