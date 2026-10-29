package localbackendcli_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	deletedSourceDeltaSession   = "local-proof-delta-session"
	deletedSourceDeltaWorkspace = "/local-proof/delta"
	deletedSourceAfter          = "2026-07-01T12:00:00Z"
	deletedSourceBefore         = "2026-07-01T13:00:00Z"
)

// deletedSourceCase pairs a case name with its search request.
type deletedSourceCase struct {
	name    string
	request localProofSearchRequest
}

// deletedSourceCases returns the eight search cases.
func deletedSourceCases() []deletedSourceCase {
	request := func(provider string, workspace string, roles string, after string, before string, includeArchived bool) localProofSearchRequest {
		return localProofSearchRequest{Query: localProofQuery, Provider: provider, Workspace: workspace, Roles: roles, After: after, Before: before, Limit: localProofMaxLimit, Offset: 0, IncludeArchived: includeArchived}
	}
	return []deletedSourceCase{
		{name: "all with archived", request: request("", "", "", "", "", true)},
		{name: "all without archived", request: request("", "", "", "", "", false)},
		{name: "provider claude", request: request(localProofProviderClaude, "", "", "", "", false)},
		{name: "provider codex with archived", request: request(localProofProviderCodex, "", "", "", "", true)},
		{name: "workspace alpha", request: request("", localProofAlphaWorkspace, "", "", "", false)},
		{name: "workspace archived with archived", request: request("", localProofArchiveWorkspace, "", "", "", true)},
		{name: "role user", request: request("", "", localProofRoleUser, "", "", true)},
		{name: "date window", request: request("", "", "", deletedSourceAfter, deletedSourceBefore, true)},
	}
}

// deletedSourceMatch stores one returned search match with its conversation ID, message index, role, provider, workspace, archived flag, timestamp in Unix seconds, and snippet.
type deletedSourceMatch struct {
	key           localProofMatchKey
	provider      string
	workspace     string
	archived      bool
	conversation  string
	timestampUnix int64
	snippet       string
}

func deletedSourceMatches(matches []localProofMatch) []deletedSourceMatch {
	converted := make([]deletedSourceMatch, 0, len(matches))
	for _, match := range matches {
		converted = append(converted, deletedSourceMatch{
			key:           localProofMatchKey{conversationID: match.Conversation.ID, messageIndex: match.MessageIndex, role: match.Role},
			provider:      match.Conversation.Provider,
			workspace:     match.Conversation.WorkspaceRoot,
			archived:      match.Conversation.Archived,
			conversation:  match.Conversation.ID,
			timestampUnix: match.Timestamp.Unix(),
			snippet:       match.Snippet,
		})
	}
	return converted
}

// deletedSourceSnapshot runs every case on the CLI and MCP surfaces and returns matches by surface and case name.
func deletedSourceSnapshot(t *testing.T, sandboxDaemon *localProofDaemon) map[string][]deletedSourceMatch {
	t.Helper()
	snapshot := make(map[string][]deletedSourceMatch)
	for _, surface := range sandboxDaemon.surfaces() {
		cases := deletedSourceCases()
		requests := make([]localProofSearchRequest, 0, len(cases))
		for _, testCase := range cases {
			requests = append(requests, testCase.request)
		}
		results := localProofSearchAll(t, surface, requests)
		for position, testCase := range cases {
			snapshot[surface.name+" / "+testCase.name] = deletedSourceMatches(results[position].Matches)
		}
	}
	return snapshot
}

// waitForDeletedSourceDelta waits until CLI search returns a match for the delta session.
func waitForDeletedSourceDelta(t *testing.T, sandboxDaemon *localProofDaemon) {
	t.Helper()
	deltaID := localProofProviderClaude + ":" + deletedSourceDeltaSession
	request := localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", After: "", Before: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: true}
	surface := sandboxDaemon.surfaces()[0]
	deadline := time.Now().Add(localProofReadyTimeout)
	for {
		result, err := surface.search(request)
		if err == nil {
			for _, match := range result.Matches {
				if match.Conversation.ID == deltaID {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("delta conversation %s did not appear in search results within %s; last result: %+v; last error: %v; daemon stderr: %s", deltaID, localProofReadyTimeout, result, err, sandboxDaemon.stderr.String())
		}
		time.Sleep(localProofPollDelay)
	}
}

// requireDeletedSourceMatchesKept fails for each deleted conversation match that a case returned before deletion but does not return at the given stage.
func requireDeletedSourceMatchesKept(t *testing.T, stage string, before map[string][]deletedSourceMatch, after map[string][]deletedSourceMatch, deletedIDs []string) {
	t.Helper()
	for name, beforeMatches := range before {
		afterMatches := after[name]
		for _, match := range beforeMatches {
			if !slices.Contains(deletedIDs, match.conversation) {
				continue
			}
			if !slices.Contains(afterMatches, match) {
				t.Errorf("at stage %s, %s did not return a match for the deleted conversation that it returned before deletion: conversation ID %s, message index %d, role %s, provider %s, workspace %s, archived %t, timestamp in Unix seconds %d, snippet %q", stage, name, match.key.conversationID, match.key.messageIndex, match.key.role, match.provider, match.workspace, match.archived, match.timestampUnix, match.snippet)
			}
		}
	}
}

// TestLocalBackendKeepsDeletedSourceSearchable checks that CLI and MCP search retain deleted transcript matches after a rescan and daemon restart.
func TestLocalBackendKeepsDeletedSourceSearchable(t *testing.T) {
	localRoot := filepath.Join(t.TempDir(), "local-index")
	checkDeletedSourceSearchable(t, []string{"--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled"}, localProofSourceLocal)
}

// checkDeletedSourceSearchable checks that CLI and MCP searches report the given source and retain deleted conversation matches and values after a rescan and restart.
func checkDeletedSourceSearchable(t *testing.T, flags []string, source string) {
	t.Helper()
	binaryPath := buildClydeCLI(t)
	home := writeLocalProofHome(t)

	first := startLocalProofDaemon(t, binaryPath, home, append(slices.Clone(flags), "--keep")...)
	t.Cleanup(func() { _ = os.RemoveAll(first.roots.Base) })
	waitForProofRowsFrom(t, first, source)
	before := deletedSourceSnapshot(t, first)
	alphaID := localProofProviderClaude + ":" + localProofAlphaSession
	archivedID := localProofProviderCodex + ":" + localProofArchivedRollout
	for _, required := range []string{alphaID, archivedID} {
		found := false
		for _, matches := range before {
			for _, match := range matches {
				if match.conversation == required {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("conversation %s did not appear in any case before deletion", required)
		}
	}

	alphaPath := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(localProofAlphaWorkspace, "/", "-"), localProofAlphaSession+".jsonl")
	archivedPath := filepath.Join(home, ".codex", "archived_sessions", "rollout-2026-07-01T11-00-00-"+localProofArchivedRollout+".jsonl")
	for _, path := range []string{alphaPath, archivedPath} {
		if err := os.Remove(path); err != nil {
			t.Fatalf("failed to remove transcript %s: %v", path, err)
		}
	}
	deltaDir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(deletedSourceDeltaWorkspace, "/", "-"))
	deltaBody := `{"type":"user","message":{"role":"user","content":"delta question 0: why does the listener rebind on reload"},"sessionId":"` + deletedSourceDeltaSession + `","cwd":"` + deletedSourceDeltaWorkspace + `","uuid":"05000000-0000-4000-8000-000000000000","timestamp":"2026-07-02T09:00:00Z"}` + "\n"
	writeLocalProofFile(t, filepath.Join(deltaDir, deletedSourceDeltaSession+".jsonl"), deltaBody)

	waitForDeletedSourceDelta(t, first)
	deletedIDs := []string{alphaID, archivedID}
	requireDeletedSourceMatchesKept(t, "after the rescan", before, deletedSourceSnapshot(t, first), deletedIDs)
	first.stop()

	// The second daemon reuses the first daemon's state and cache directories during a normal restart.
	second := startLocalProofDaemon(t, binaryPath, home, append(slices.Clone(flags), "--root", first.roots.Base)...)
	waitForDeletedSourceDelta(t, second)
	requireDeletedSourceMatchesKept(t, "after the restart", before, deletedSourceSnapshot(t, second), deletedIDs)
}
