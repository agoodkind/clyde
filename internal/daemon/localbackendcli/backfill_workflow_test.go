package localbackendcli_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	backfillWorkflowWorkspace = "/backfill/workflow"
	backfillWorkflowFirst     = "backfill-workflow-first"
	backfillWorkflowBackfill  = "backfill-workflow-second"
	backfillWorkflowDaemon    = "backfill-workflow-third"
	backfillWorkflowAge       = 2 * time.Minute
	backfillWorkflowWaitLimit = 5 * time.Minute
	backfillWorkflowPoll      = 100 * time.Millisecond
	backfillWorkflowChars     = 32
)

func backfillWorkflowToken(session string) string {
	return "zq" + strings.ReplaceAll(session, "-", "") + "tokenx"
}

func installBackfillWorkflowChat(t *testing.T, home string, session string, modified time.Time) {
	t.Helper()
	line := fmt.Sprintf(
		`{"type":"user","message":{"role":"user","content":%q},"sessionId":%q,"cwd":%q,"uuid":"00000000-0000-4000-8000-%012x","timestamp":%q}`+"\n",
		"The backfill workflow indexes this record: "+backfillWorkflowToken(session),
		session,
		backfillWorkflowWorkspace,
		len(session),
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	)
	folder := filepath.Join(
		home,
		".claude",
		"projects",
		strings.ReplaceAll(backfillWorkflowWorkspace, "/", "-"),
	)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatalf("The test could not create the transcript folder: %v.", err)
	}

	temporary := filepath.Join(home, session+".tmp")
	if err := os.WriteFile(temporary, []byte(line), 0o600); err != nil {
		t.Fatalf("The test could not write the transcript: %v.", err)
	}

	if err := os.Chtimes(temporary, modified, modified); err != nil {
		t.Fatalf("The test could not set the transcript modification time: %v.", err)
	}

	if err := os.Rename(temporary, filepath.Join(folder, session+".jsonl")); err != nil {
		t.Fatalf("The test could not rename the transcript into the transcript folder: %v.", err)
	}
}

func backfillWorkflowCheckpointed(t *testing.T, daemon *localProofDaemon, session string) bool {
	t.Helper()
	digest := sha256.Sum256([]byte(localProofProviderClaude + ":" + session))
	name := hex.EncodeToString(digest[:])[:backfillWorkflowChars] + ".json"
	matches, err := filepath.Glob(
		filepath.Join(
			daemon.roots.State,
			"clyde",
			"conversation-semantic",
			"checkpoints",
			"*",
			name,
		),
	)
	if err != nil {
		t.Fatalf("The test could not find checkpoint files: %v.", err)
	}

	return len(matches) > 0
}

func waitForBackfillWorkflow(
	t *testing.T,
	daemon *localProofDaemon,
	description string,
	check func() bool,
) {
	t.Helper()
	deadline := time.Now().Add(backfillWorkflowWaitLimit)

	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf(
				"The test did not observe %s before the wait limit. The daemon reported: %s.",
				description,
				daemon.stderr.String(),
			)
		}

		time.Sleep(backfillWorkflowPoll)
	}
}

// TestLocalBackfillAndDaemonKeepEachOthersRows runs backfill with an active daemon.
// Both processes use one local store. The daemon indexes the first chat.
// Backfill indexes a recent chat that the daemon defers. The daemon indexes a third
// chat. CLI and MCP searches find each chat's conversation ID and message token.
func TestLocalBackfillAndDaemonKeepEachOthersRows(t *testing.T) {
	home := t.TempDir()
	installBackfillWorkflowChat(
		t,
		home,
		backfillWorkflowFirst,
		time.Now().Add(-backfillWorkflowAge),
	)
	settingsPath := filepath.Join(t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(t, settingsPath, sandboxFastIntervals)
	localRoot := filepath.Join(t.TempDir(), "local-index")
	daemon := startLocalProofDaemon(
		t,
		buildClydeCLI(t),
		home,
		"--local",
		"--local-root",
		localRoot,
		"--ingestion-enabled",
		"--search-enabled",
		"--semantic-settings",
		settingsPath,
	)
	waitForBackfillWorkflow(
		t,
		daemon,
		"a checkpoint for the first conversation",
		func() bool { return backfillWorkflowCheckpointed(t, daemon, backfillWorkflowFirst) },
	)

	// The daemon defers this chat because its transcript modification time is recent.
	installBackfillWorkflowChat(t, home, backfillWorkflowBackfill, time.Now())
	ctx, cancel := context.WithTimeout(t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		daemon.binaryPath,
		"conversation",
		"index",
		"backfill-documents",
		"--conversation",
		localProofProviderClaude+":"+backfillWorkflowBackfill,
		"--execute",
	)
	command.Env = daemon.environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf(
			"The backfill command failed: %v. The command returned this output: %s.",
			err,
			output,
		)
	}

	installBackfillWorkflowChat(
		t,
		home,
		backfillWorkflowDaemon,
		time.Now().Add(-backfillWorkflowAge),
	)
	waitForBackfillWorkflow(
		t,
		daemon,
		"a checkpoint for the third conversation",
		func() bool { return backfillWorkflowCheckpointed(t, daemon, backfillWorkflowDaemon) },
	)

	for _, session := range []string{
		backfillWorkflowFirst,
		backfillWorkflowBackfill,
		backfillWorkflowDaemon,
	} {
		conversationID := localProofProviderClaude + ":" + session
		token := backfillWorkflowToken(session)

		for _, surface := range daemon.surfaces() {
			result := mustLocalProofSearch(
				t,
				surface,
				localProofSearchRequest{Query: token, Limit: 5, ConversationID: conversationID},
			)
			found := slices.ContainsFunc(
				result.Matches,
				func(match localProofMatch) bool {
					return match.Conversation.ID == conversationID &&
						strings.Contains(match.Snippet, token)
				},
			)
			if !found {
				t.Fatalf(
					"%s did not find conversation %s with its message token among matches %+v.",
					surface.name,
					conversationID,
					result.Matches,
				)
			}
		}
	}
}
