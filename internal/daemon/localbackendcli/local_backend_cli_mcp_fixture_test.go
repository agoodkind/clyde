package localbackendcli_test

import (
	"crypto/sha256"
	"encoding/hex"
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

	"goodkind.io/clyde/internal/sandbox"
)

func assertLocalProofFilters(t *testing.T, surface localProofSurface) {
	t.Helper()
	alphaID := localProofProviderClaude + ":" + localProofAlphaSession
	archivedID := localProofProviderCodex + ":" + localProofArchivedRollout
	testCases := []struct {
		name     string
		request  localProofSearchRequest
		want     int
		eligible func(localProofMatch) bool
	}{
		{
			name:     "provider claude",
			request:  localProofSearchRequest{Query: localProofQuery, Provider: localProofProviderClaude, Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false},
			want:     localProofClaudeRows,
			eligible: func(match localProofMatch) bool { return match.Conversation.Provider == localProofProviderClaude },
		},
		{
			name:    "provider codex",
			request: localProofSearchRequest{Query: localProofQuery, Provider: localProofProviderCodex, Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false},
			want:    localProofRolloutRows,
			eligible: func(match localProofMatch) bool {
				return match.Conversation.Provider == localProofProviderCodex && !match.Conversation.Archived
			},
		},
		{
			name:     "role user",
			request:  localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: localProofRoleUser, Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false},
			want:     localProofVisibleUserRows,
			eligible: func(match localProofMatch) bool { return match.Role == localProofRoleUser },
		},
		{
			name:     "role assistant",
			request:  localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: localProofRoleAssistant, Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false},
			want:     localProofVisibleUserRows,
			eligible: func(match localProofMatch) bool { return match.Role == localProofRoleAssistant },
		},
		{
			name:    "workspace alpha",
			request: localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: localProofAlphaWorkspace, Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false},
			want:    localProofSessionRows,
			eligible: func(match localProofMatch) bool {
				return match.Conversation.ID == alphaID && match.Conversation.WorkspaceRoot == localProofAlphaWorkspace
			},
		},
		{
			name:     "archived excluded by default",
			request:  localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false},
			want:     localProofVisibleRows,
			eligible: func(match localProofMatch) bool { return !match.Conversation.Archived },
		},
		{
			name:     "archived workspace without include-archived",
			request:  localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: localProofArchiveWorkspace, Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false},
			want:     0,
			eligible: func(localProofMatch) bool { return false },
		},
		{
			name:    "archived workspace with include-archived",
			request: localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: localProofArchiveWorkspace, Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: true},
			want:    localProofRolloutRows,
			eligible: func(match localProofMatch) bool {
				return match.Conversation.ID == archivedID && match.Conversation.Archived
			},
		},
		{
			name:     "include-archived returns every row",
			request:  localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: true},
			want:     localProofAllRows,
			eligible: func(localProofMatch) bool { return true },
		},
	}
	requests := make([]localProofSearchRequest, 0, len(testCases))
	for _, testCase := range testCases {
		requests = append(requests, testCase.request)
	}
	results := localProofSearchAll(t, surface, requests)
	for position, testCase := range testCases {
		result := results[position]
		if result.Source != localProofSourceLocal {
			t.Errorf("%s %s: source = %q, want %q", surface.name, testCase.name, result.Source, localProofSourceLocal)
		}
		if len(result.Matches) != testCase.want {
			t.Errorf("%s %s: returned %d matches, want %d: %v", surface.name, testCase.name, len(result.Matches), testCase.want, localProofMatchKeys(result.Matches))
		}
		for _, match := range result.Matches {
			if !testCase.eligible(match) {
				t.Errorf("%s %s: returned ineligible match %+v", surface.name, testCase.name, match)
			}
		}
	}
}

func assertLocalProofPages(t *testing.T, surface localProofSurface) {
	t.Helper()
	const pageCount = 3
	requests := []localProofSearchRequest{{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: pageCount * localProofPageLimit, Offset: 0, IncludeArchived: false}}
	for page := range pageCount {
		requests = append(requests, localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofPageLimit, Offset: page * localProofPageLimit, IncludeArchived: false})
	}
	results := localProofSearchAll(t, surface, requests)
	ranking := localProofMatchKeys(results[0].Matches)
	if len(ranking) != pageCount*localProofPageLimit {
		t.Fatalf("%s limit %d returned %d matches, want %d", surface.name, pageCount*localProofPageLimit, len(ranking), pageCount*localProofPageLimit)
	}
	paged := make([]localProofMatchKey, 0, len(ranking))
	seen := make(map[localProofMatchKey]int, len(ranking))
	for page, result := range results[1:] {
		offset := page * localProofPageLimit
		if result.Source != localProofSourceLocal {
			t.Errorf("%s offset %d source = %q, want %q", surface.name, offset, result.Source, localProofSourceLocal)
		}
		keys := localProofMatchKeys(result.Matches)
		if len(keys) != localProofPageLimit {
			t.Errorf("%s offset %d returned %d matches, want %d", surface.name, offset, len(keys), localProofPageLimit)
		}
		for _, key := range keys {
			if firstOffset, duplicate := seen[key]; duplicate {
				t.Errorf("%s offset %d repeats %+v from offset %d", surface.name, offset, key, firstOffset)
			}
			seen[key] = offset
		}
		paged = append(paged, keys...)
	}
	for _, key := range ranking {
		if _, found := seen[key]; !found {
			t.Errorf("%s pages omit %+v", surface.name, key)
		}
	}
	if !slices.Equal(paged, ranking) {
		t.Errorf("%s pages differ from the limit %d ranking:\npages   %v\nranking %v", surface.name, pageCount*localProofPageLimit, paged, ranking)
	}
}

func assertLocalProofRawText(t *testing.T, sandboxDaemon *localProofDaemon) {
	t.Helper()
	request := localProofSearchRequest{Query: "watcher classifies", Provider: "", Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: false}
	for _, surface := range sandboxDaemon.surfaces() {
		deadline := time.Now().Add(localProofReadyTimeout)
		for {
			result, err := surface.search(request)
			if err == nil && (result.Source == localProofSourceLocal || result.Source == localProofSourceSemantic) {
				t.Fatalf("%s source = %q, want %q or an error", surface.name, result.Source, localProofSourceRawText)
			}
			if err == nil && result.Source == localProofSourceRawText && len(result.Matches) == localProofVisibleSessions {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not return %d %s matches within %s; last result %+v, last error %v; stderr:\n%s", surface.name, localProofVisibleSessions, localProofSourceRawText, localProofReadyTimeout, result, err, sandboxDaemon.stderr.String())
			}
			time.Sleep(localProofPollDelay)
		}
	}
}

func waitForLocalProofRows(t *testing.T, sandboxDaemon *localProofDaemon) {
	t.Helper()
	request := localProofSearchRequest{Query: localProofQuery, Provider: "", Workspace: "", Roles: "", Limit: localProofMaxLimit, Offset: 0, IncludeArchived: true}
	surface := sandboxDaemon.surfaces()[0]
	deadline := time.Now().Add(localProofReadyTimeout)
	for {
		result, err := surface.search(request)
		if err == nil && result.Source == localProofSourceLocal && len(result.Matches) == localProofAllRows {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the local index did not return %d rows within %s; last result %+v, last error %v; stderr:\n%s", localProofAllRows, localProofReadyTimeout, result, err, sandboxDaemon.stderr.String())
		}
		time.Sleep(localProofPollDelay)
	}
}

func mustLocalProofSearch(t *testing.T, surface localProofSurface, request localProofSearchRequest) localProofSearchOutput {
	t.Helper()
	result, err := surface.search(request)
	if err != nil {
		t.Fatalf("%s search %+v: %v", surface.name, request, err)
	}
	return result
}

func localProofMatchKeys(matches []localProofMatch) []localProofMatchKey {
	keys := make([]localProofMatchKey, 0, len(matches))
	for _, match := range matches {
		keys = append(keys, localProofMatchKey{conversationID: match.Conversation.ID, messageIndex: match.MessageIndex, role: match.Role})
	}
	return keys
}

func writeLocalProofHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	claudeSessions := []struct {
		session   string
		workspace string
		topic     string
		minute    int
	}{
		{session: localProofAlphaSession, workspace: localProofAlphaWorkspace, topic: "alpha", minute: 0},
		{session: localProofBetaSession, workspace: localProofBetaWorkspace, topic: "beta", minute: 1},
	}
	for _, claudeSession := range claudeSessions {
		var body strings.Builder
		for turn := range localProofClaudeTurns {
			for part, role := range []string{localProofRoleUser, localProofRoleAssistant} {
				index := turn*localProofMessagesPerTurn + part
				head := fmt.Sprintf(`"sessionId":%q,"cwd":%q,"uuid":"%02d%06d-0000-4000-8000-000000000000","timestamp":"2026-07-01T12:%02d:%02dZ"`, claudeSession.session, claudeSession.workspace, claudeSession.minute, index, claudeSession.minute, index)
				text := localProofMessageText(claudeSession.topic, role, turn)
				if role == localProofRoleUser {
					fmt.Fprintf(&body, `{"type":"user","message":{"role":"user","content":%q},%s}`+"\n", text, head)
				} else {
					fmt.Fprintf(&body, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`+"\n", text, head)
				}
			}
		}
		projectDir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(claudeSession.workspace, "/", "-"))
		writeLocalProofFile(t, filepath.Join(projectDir, claudeSession.session+".jsonl"), body.String())
	}
	rollouts := []struct {
		id        string
		workspace string
		topic     string
		directory string
	}{
		{id: localProofActiveRollout, workspace: localProofActiveWorkspace, topic: "gamma", directory: filepath.Join("sessions", "2026", "07", "01")},
		{id: localProofArchivedRollout, workspace: localProofArchiveWorkspace, topic: "archived", directory: "archived_sessions"},
	}
	for _, rollout := range rollouts {
		var body strings.Builder
		fmt.Fprintf(&body, `{"timestamp":"2026-07-01T11:00:00.000Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-07-01T11:00:00.000Z","cwd":%q,"originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`+"\n", rollout.id, rollout.workspace)
		for turn := range localProofCodexTurns {
			for part, role := range []string{localProofRoleUser, localProofRoleAssistant} {
				second := turn*localProofMessagesPerTurn + part + 1
				eventType := "user_message"
				if role == localProofRoleAssistant {
					eventType = "agent_message"
				}
				fmt.Fprintf(&body, `{"timestamp":"2026-07-01T11:00:%02d.000Z","type":"event_msg","payload":{"type":%q,"message":%q}}`+"\n", second, eventType, localProofMessageText(rollout.topic, role, turn))
			}
		}
		path := filepath.Join(home, ".codex", rollout.directory, "rollout-2026-07-01T11-00-00-"+rollout.id+".jsonl")
		writeLocalProofFile(t, path, body.String())
	}
	return home
}

func localProofMessageText(topic string, role string, turn int) string {
	if role == localProofRoleUser {
		return fmt.Sprintf("%s question %d: why does the listener rebind on reload", topic, turn)
	}
	return fmt.Sprintf("%s answer %d: the watcher classifies the config change before the daemon rebinds", topic, turn)
}

func writeLocalProofFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readLocalProofUpserts(t *testing.T, home string, roots sandbox.Roots, since time.Time) []localProofUpsertRecord {
	t.Helper()
	records := make([]localProofUpsertRecord, 0)
	for _, stateRoot := range []string{filepath.Join(home, ".local", "state"), roots.State} {
		walkErr := filepath.WalkDir(stateRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if entry.IsDir() || entry.Name() != localProofDaemonLogName {
				return nil
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for line := range strings.Lines(string(content)) {
				if !strings.HasSuffix(line, "\n") || !strings.Contains(line, localProofUpsertEvent) {
					continue
				}
				var record localProofUpsertRecord
				if decodeErr := json.Unmarshal([]byte(line), &record); decodeErr != nil {
					return fmt.Errorf("decode %s record in %s: %w", localProofUpsertEvent, path, decodeErr)
				}
				if record.Message == localProofUpsertEvent && !record.Time.Before(since) {
					records = append(records, record)
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

func localProofStoreDigests(t *testing.T, localRoot string) map[string]string {
	t.Helper()
	digests := make(map[string]string)
	walkErr := filepath.WalkDir(localRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		relativePath, relErr := filepath.Rel(localRoot, path)
		if relErr != nil {
			return relErr
		}
		sum := sha256.Sum256(content)
		digests[relativePath] = hex.EncodeToString(sum[:])
		return nil
	})
	if walkErr != nil {
		t.Fatalf("read local store files under %s: %v", localRoot, walkErr)
	}
	return digests
}
