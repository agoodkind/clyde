package localbackendcli_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubagentIngestionSettingRetainsSemanticContent(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	localRoot := filepath.Join(t.TempDir(), "local-index")
	project := filepath.Join(home, ".claude", "projects", "-subagent-ingestion")
	agentPath := filepath.Join(project, "parent", "subagents", "agent-original.jsonl")
	const original = "original subagent listener rebind"
	writeSubagentIngestionTranscript(t, agentPath, original, true)
	enabled := writeSandboxSemanticSettings(
		t,
		"[conversation]\ninclude_subagent_conversations = true\n",
	)
	seed := startLocalProofDaemon(
		t,
		binaryPath,
		home,
		"--local",
		"--local-root",
		localRoot,
		"--ingestion-enabled",
		"--search-enabled",
		"--semantic-settings",
		enabled,
	)
	waitForSubagentSemanticRows(t, seed, 1)
	seed.stop()

	writeSubagentIngestionTranscript(t, agentPath, "changed subagent listener rebind", true)
	writeSubagentIngestionTranscript(
		t,
		filepath.Join(project, "parent", "subagents", "agent-new.jsonl"),
		"new subagent listener rebind",
		true,
	)
	const ordinary = "ordinary conversation listener rebind"
	writeSubagentIngestionTranscript(t, filepath.Join(project, "ordinary.jsonl"), ordinary, false)
	disabled := writeSandboxSemanticSettings(
		t,
		"[conversation]\ninclude_subagent_conversations = false\n",
	)
	restarted := startLocalProofDaemon(
		t,
		binaryPath,
		home,
		"--local",
		"--local-root",
		localRoot,
		"--ingestion-enabled",
		"--search-enabled",
		"--semantic-settings",
		disabled,
	)
	waitForSubagentSemanticRows(t, restarted, 2)

	for _, surface := range restarted.surfaces() {
		result := mustLocalProofSearch(
			t,
			surface,
			localProofSearchRequest{
				Query: "listener rebind",
				Limit: localProofMaxLimit,
			},
		)
		if result.Source != localProofSourceLocal || len(result.Matches) != 2 {
			t.Fatalf("%s semantic matches = %+v", surface.name, result)
		}

		for _, match := range result.Matches {
			if match.Snippet != original && match.Snippet != ordinary {
				t.Fatalf("%s ingested a new or changed subagent: %+v", surface.name, match)
			}
		}
	}
}

func writeSubagentIngestionTranscript(t *testing.T, path string, text string, subagent bool) {
	t.Helper()
	body := fmt.Sprintf(
		`{"sessionId":%q,"cwd":"/subagent-ingestion","isSidechain":%t,"type":"user","timestamp":"2026-07-01T10:00:00Z","message":{"role":"user","content":%q}}`+"\n",
		strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		subagent,
		text,
	)
	installLocalProofTranscript(t, path, body)
}

func waitForSubagentSemanticRows(t *testing.T, daemon *localProofDaemon, count int) {
	t.Helper()
	deadline := time.Now().Add(localProofReadyTimeout)
	for {
		result, err := daemon.searchCLI(localProofSearchRequest{
			Query: "listener rebind",
			Limit: localProofMaxLimit,
		})
		if err == nil && result.Source == localProofSourceLocal && len(result.Matches) == count {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf(
				"semantic search did not return %d rows: %+v, %v\n%s",
				count,
				result,
				err,
				daemon.stderr.String(),
			)
		}

		time.Sleep(localProofPollDelay)
	}
}
