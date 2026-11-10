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
	"goodkind.io/clyde/internal/sandbox"
)

const (
	withheldLimit       = 2
	withheldQuery       = "archived question 0 archived answer 0 archived"
	withheldSearchEvent = "conversation.vectorsearch.search_completed"
	withheldDatedAfter  = "2000-01-01"
	withheldEpochAfter  = "1970-01-01T00:00:00Z"
	withheldDatedCalls  = 2
	withheldAnyCalls    = -1
	withheldSettings    = `milvus_address = %q
milvus_database = "default"
milvus_layout = "shared"
embedding_base_url = %q
embedding_model = %q
embedding_dimension = %d
`
)

// withheldSearchRecord stores one search_completed log record with its file path, trace ID, time, candidate count, and hit count.
type withheldSearchRecord struct {
	path       string
	traceID    string
	time       time.Time
	candidates int
	hits       int
}

// withheldRequest records one completed CLI or MCP search with the time before it started, the time after its response returned, and the expected engine call count. withheldAnyCalls skips the count check.
type withheldRequest struct {
	label string
	start time.Time
	end   time.Time
	calls int
}

// searchCompletedRecords returns each distinct search_completed record logged at or after since under the state roots. Records with the same trace ID and time in two log files count once.
func searchCompletedRecords(t *testing.T, stateRoots []string, since time.Time) []withheldSearchRecord {
	t.Helper()
	records := []withheldSearchRecord{}
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
					records = append(records, withheldSearchRecord{path: path, traceID: record.TraceID, time: record.Time, candidates: record.Candidates, hits: record.Hits})
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

// timedWithheldSearch runs one search and appends its start time, end time, label, and expected engine call count to requests.
func timedWithheldSearch(t *testing.T, surface localProofSurface, request localProofSearchRequest, label string, calls int, requests *[]withheldRequest) localProofSearchOutput {
	t.Helper()
	start := time.Now()
	result := mustLocalProofSearch(t, surface, request)
	*requests = append(*requests, withheldRequest{label: label, start: start, end: time.Now(), calls: calls})
	return result
}

// assertWithheldCalls assigns each record to a completed request with a time window containing it and fails for records outside every window. Each checked request must have exactly one trace ID with the expected record count.
func assertWithheldCalls(t *testing.T, records []withheldSearchRecord, requests []withheldRequest) {
	t.Helper()
	traces := make([]map[string]int, len(requests))
	for position := range requests {
		traces[position] = map[string]int{}
	}
	for _, record := range records {
		position := slices.IndexFunc(requests, func(request withheldRequest) bool {
			return !record.time.Before(request.start) && !record.time.After(request.end)
		})
		if position < 0 {
			t.Errorf("%s record at %s with trace %s falls outside every completed request (%s)", withheldSearchEvent, record.time.Format(time.RFC3339Nano), record.traceID, record.path)
			continue
		}
		traces[position][record.traceID]++
	}
	for position, request := range requests {
		if request.calls == withheldAnyCalls {
			continue
		}
		if len(traces[position]) != 1 {
			t.Errorf("%s logged %s records under %d traces, want 1: %v", request.label, withheldSearchEvent, len(traces[position]), traces[position])
			continue
		}
		for traceID, count := range traces[position] {
			if count != request.calls {
				t.Errorf("%s logged %d %s records under trace %s, want %d", request.label, count, withheldSearchEvent, traceID, request.calls)
			}
		}
	}
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
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--keep", "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	// The sandbox keeps its logs after the daemon exits. Cleanup stops the daemon, then deletes the kept root only if it is a sandbox temp directory.
	t.Cleanup(func() {
		sandboxDaemon.stop()
		base := sandboxDaemon.roots.Base
		if !sandbox.UnderTempRoot(base) || !strings.HasPrefix(filepath.Base(base), sandbox.RootPattern) {
			t.Errorf("kept sandbox root %s is not a sandbox temp directory; leaving it in place", base)
			return
		}
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("remove kept sandbox root %s: %v", base, err)
		}
	})
	waitForProofRowsFrom(t, sandboxDaemon, localProofSourceSemantic)

	stateRoots := []string{filepath.Join(home, ".local", "state"), sandboxDaemon.roots.State}
	request := localProofSearchRequest{Query: withheldQuery, Limit: withheldLimit, Offset: 0, IncludeArchived: false}
	archivedRequest := request
	archivedRequest.IncludeArchived = true
	requests := []withheldRequest{}
	for _, surface := range sandboxDaemon.surfaces() {
		archived := timedWithheldSearch(t, surface, archivedRequest, surface.name+" with archived conversations", withheldAnyCalls, &requests)
		if len(archived.Matches) != withheldLimit || !archived.Matches[0].Conversation.Archived {
			t.Fatalf("%s with archived conversations returned %v, want the archived rollout first", surface.name, localProofMatchKeys(archived.Matches))
		}
		result := timedWithheldSearch(t, surface, request, surface.name, 1, &requests)
		if result.Source != localProofSourceSemantic || len(result.Matches) != withheldLimit {
			t.Fatalf("%s returned source %q and %d matches, want %q and %d", surface.name, result.Source, len(result.Matches), localProofSourceSemantic, withheldLimit)
		}
		if slices.ContainsFunc(result.Matches, func(match localProofMatch) bool { return match.Conversation.Archived }) {
			t.Fatalf("%s returned an archived match without include-archived: %v", surface.name, localProofMatchKeys(result.Matches))
		}
		for _, after := range []string{withheldDatedAfter, withheldEpochAfter} {
			datedRequest := request
			datedRequest.After = after
			dated := timedWithheldSearch(t, surface, datedRequest, surface.name+" with --after "+after, withheldDatedCalls, &requests)
			if !slices.Equal(localProofMatchKeys(dated.Matches), localProofMatchKeys(result.Matches)) {
				t.Fatalf("%s with --after %s returned %v, want %v", surface.name, after, localProofMatchKeys(dated.Matches), localProofMatchKeys(result.Matches))
			}
		}
		t.Logf("%s returned %v; with archived conversations it returned %v", surface.name, localProofMatchKeys(result.Matches), localProofMatchKeys(archived.Matches))
	}
	sandboxDaemon.stop()
	records := searchCompletedRecords(t, stateRoots, requests[0].start)
	assertWithheldCalls(t, records, requests)
	t.Logf("read %d %s records after the sandbox daemon exited", len(records), withheldSearchEvent)
}
