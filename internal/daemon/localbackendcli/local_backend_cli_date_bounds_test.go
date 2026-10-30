package localbackendcli_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	dateBoundEpochSession   = "e0e0e0e0-0000-4000-8000-000000000001"
	dateBoundUnknownSession = "e0e0e0e0-0000-4000-8000-000000000002"
	dateBoundWorkspace      = "/repo-date-bounds"
	dateBoundQuery          = "quarterly restic retention audit"
)

func writeDateBoundHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	projectDir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(dateBoundWorkspace, "/", "-"))
	for _, session := range []struct {
		id        string
		timestamp string
	}{
		{id: dateBoundEpochSession, timestamp: `,"timestamp":"1970-01-01T00:00:00Z"`},
		{id: dateBoundUnknownSession, timestamp: ""},
	} {
		var body strings.Builder
		for index, role := range []string{localProofRoleUser, localProofRoleAssistant} {
			head := fmt.Sprintf(`"sessionId":%q,"cwd":%q,"uuid":"%s-%02d"%s`, session.id, dateBoundWorkspace, session.id[:8], index, session.timestamp)
			text := fmt.Sprintf("The %s %s discusses the %s.", role, session.id[len(session.id)-1:], dateBoundQuery)
			if role == localProofRoleUser {
				fmt.Fprintf(&body, `{"type":"user","message":{"role":"user","content":%q},%s}`+"\n", text, head)
			} else {
				fmt.Fprintf(&body, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`+"\n", text, head)
			}
		}
		installLocalProofTranscript(t, filepath.Join(projectDir, session.id+".jsonl"), body.String())
	}
	return home
}

func dateBoundConversations(result localProofSearchOutput) []string {
	conversations := make([]string, 0, len(result.Matches))
	for _, match := range result.Matches {
		if !slices.Contains(conversations, match.Conversation.ID) {
			conversations = append(conversations, match.Conversation.ID)
		}
	}
	slices.Sort(conversations)
	return conversations
}

func TestDateBoundsKeepEpochZeroAndExcludeUnknownTimes(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := writeDateBoundHome(t)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--local", "--local-root", localRoot, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	epoch := "claude:" + dateBoundEpochSession
	unknown := "claude:" + dateBoundUnknownSession
	request := func(after string, before string) localProofSearchRequest {
		return localProofSearchRequest{Query: dateBoundQuery, Provider: "", Workspace: "", Roles: "", After: after, Before: before, Limit: localProofMaxLimit, Offset: 0, IncludeArchived: true}
	}
	surface := sandboxDaemon.surfaces()[0]
	deadline := time.Now().Add(localProofReadyTimeout)
	for {
		result, err := surface.search(request("", ""))
		if err == nil && result.Source == localProofSourceLocal && slices.Equal(dateBoundConversations(result), []string{epoch, unknown}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Local search did not return both conversations before the deadline. Result, error, and daemon output: %+v, %v, %s", result, err, sandboxDaemon.stderr.String())
		}
		time.Sleep(localProofPollDelay)
	}
	cases := []struct {
		name    string
		request localProofSearchRequest
		want    []string
	}{
		{name: "omitted bounds", request: request("", ""), want: []string{epoch, unknown}},
		{name: "after epoch zero", request: request("1970-01-01T00:00:00Z", ""), want: []string{epoch}},
		{name: "before one second after epoch", request: request("", "1970-01-01T00:00:01Z"), want: []string{epoch}},
		{name: "negative lower bound", request: request("1969-12-31T23:59:00Z", "1970-01-01T00:00:01Z"), want: []string{epoch}},
		{name: "before epoch zero", request: request("", "1970-01-01T00:00:00Z"), want: []string{}},
	}
	for _, searchSurface := range sandboxDaemon.surfaces() {
		for _, testCase := range cases {
			result := mustLocalProofSearch(t, searchSurface, testCase.request)
			if got := dateBoundConversations(result); !slices.Equal(got, testCase.want) {
				t.Errorf("The search returned unexpected conversations. Surface, case, actual, and expected: %s %s: %v, %v", searchSurface.name, testCase.name, got, testCase.want)
			}
			for _, match := range result.Matches {
				if match.Conversation.ID == epoch && match.Timestamp.Unix() != 0 {
					t.Errorf("The search returned an unexpected epoch timestamp. Surface, case, and timestamp: %s %s: %v", searchSurface.name, testCase.name, match.Timestamp)
				}
			}
		}
	}
}
