package localbackendcli_test

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	sideChatParentID    = "50000000-0000-4000-8000-000000000001"
	sideChatAgentID     = "50000000-0000-4000-8000-000000000002"
	sideChatRequestID   = "50000000-0000-4000-8000-000000000003"
	sideChatAmbiguousID = "50000000-0000-4000-8000-000000000004"
	sideChatQuotedID    = "50000000-0000-4000-8000-000000000005"
	sideChatText        = "side chat export probe"
	sideChatParentText  = "parent conversation export probe"
	sideChatTwinText    = "top level twin export probe"
)

func TestCursorSideChatExportIgnoresSubagentIngestionSetting(t *testing.T) {
	binaryPath := buildClydeCLI(t)

	for _, include := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("include_subagents=%t", include),
			func(t *testing.T) {
				home := t.TempDir()
				path := writeSideChatExportFixture(t, home)
				providerRoot := filepath.Join(home, "providers")
				before := localProofStoreDigests(t, providerRoot)
				settings := writeSandboxSemanticSettings(
					t,
					fmt.Sprintf("[conversation]\ninclude_subagent_conversations = %t\n", include),
				)
				d := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settings)
				waitForInjectedProbeMCP(t, binaryPath, d.environment, "cursor", sideChatParentID)

				t.Setenv(
					"CLYDE_CURSOR_DATA_DIRS",
					filepath.Join(providerRoot, "CLYDE_CURSOR_DATA_DIRS"),
				)
				t.Setenv(
					"CLYDE_CURSOR_PROJECTS_DIRS",
					filepath.Join(providerRoot, "CLYDE_CURSOR_PROJECTS_DIRS"),
				)
				t.Setenv("XDG_CACHE_HOME", t.TempDir())
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				registry := conversation.NewRegistry()
				registry.Register(cursorparser.New())
				index := conversation.NewIndex(
					registry,
					config.ConversationConfig{
						IncludeSubagentConversations: include,
					},
				)
				if err := index.Refresh(t.Context()); err != nil {
					t.Fatal(err)
				}

				assertSideChatVisibility(t, index, include)

				for _, selector := range []string{sideChatAgentID, sideChatRequestID, path} {
					t.Run(
						selector,
						func(t *testing.T) {
							text, err := lookupProbeCLI(t, d, "export", selector, "")
							if err != nil {
								t.Fatalf("CLI export: %v\n%s", err, text)
							}

							assertSideChatExport(t, text, selector == path)
							result := lookupProbeMCP(t, d, "export", selector, "")
							if result.IsError {
								t.Fatalf("MCP export: %s", result.text())
							}

							assertSideChatExport(t, result.text(), selector == path)
						},
					)
				}

				for _, selector := range []string{"cursor:" + sideChatAgentID, path} {
					record, err := index.ResolveForExport(t.Context(), selector)
					if err != nil || record.NativeID != sideChatAgentID {
						t.Fatalf("explicit identity = %+v, error = %v", record, err)
					}

					if selector == path && record.ArtifactPath != path {
						t.Fatalf("path selected %s instead of %s", record.ArtifactPath, path)
					}
				}

				assertSideChatVisibility(t, index, include)
				assertSideChatExportMisses(t, index, d, include)

				outputPath := filepath.Join(t.TempDir(), "side-chat.md")
				receipt, err := lookupProbeCLI(t, d, "export", path, outputPath)
				if err != nil || !strings.Contains(receipt, "tokens") {
					t.Fatalf("export receipt omitted token count: %v\n%s", err, receipt)
				}

				body, err := os.ReadFile(outputPath)
				if err != nil {
					t.Fatal(err)
				}

				assertSideChatExport(t, string(body), true)
				d.stop()
				if after := localProofStoreDigests(t, providerRoot); !maps.Equal(before, after) {
					t.Fatalf("provider artifacts changed:\nbefore %v\nafter %v", before, after)
				}
			},
		)
	}
}

func assertSideChatExport(t *testing.T, text string, nestedPath bool) {
	t.Helper()
	if !strings.Contains(text, sideChatText) || strings.Contains(text, sideChatParentText) ||
		(nestedPath && strings.Contains(text, sideChatTwinText)) {
		t.Fatalf("export did not select the side chat: %s", text)
	}
}

func assertSideChatVisibility(t *testing.T, index *conversation.Index, include bool) {
	t.Helper()
	records, err := index.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	stamped, err := index.ListWithStamps(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	feederRecords := make([]conversation.Record, 0, len(stamped))

	for _, record := range stamped {
		feederRecords = append(feederRecords, record.Record)
	}

	for _, records := range [][]conversation.Record{records, feederRecords} {
		found := false

		for _, record := range records {
			if record.NativeID == sideChatAgentID {
				found = true
			}
		}

		if found != include {
			t.Fatalf("side chat visibility = %t, want %t", found, include)
		}
	}
}

func assertSideChatExportMisses(
	t *testing.T,
	index *conversation.Index,
	d *localProofDaemon,
	include bool,
) {
	t.Helper()

	for _, selector := range []string{sideChatAmbiguousID, sideChatQuotedID} {
		_, err := index.ResolveForExport(t.Context(), selector)
		missing, ok := errors.AsType[*conversation.NotFoundError](err)
		if !ok {
			t.Fatalf("selector %s error = %v, want typed miss", selector, err)
		}

		if selector == sideChatAmbiguousID &&
			missing.Reason != conversation.RequestNotFoundReasonAmbiguousConversation {
			t.Fatalf("ambiguous request reason = %s", missing.Reason)
		}

		text, failure := lookupProbeCLI(t, d, "export", selector, "")
		if failure == nil || !strings.Contains(text, "not found") {
			t.Fatalf("CLI accepted request %s: %v\n%s", selector, failure, text)
		}

		if result := lookupProbeMCP(t, d, "export", selector, ""); !result.IsError {
			t.Fatalf("MCP accepted request %s: %s", selector, result.text())
		}
	}

	if !include {
		if _, err := index.ResolveForExport(t.Context(), sideChatText); err == nil {
			t.Fatal("export selected a hidden conversation by title")
		}
	}
}

func writeSideChatExportFixture(t *testing.T, home string) string {
	t.Helper()
	providerRoot := filepath.Join(home, "providers")
	transcripts := filepath.Join(
		providerRoot,
		"CLYDE_CURSOR_PROJECTS_DIRS",
		"probe",
		"agent-transcripts",
	)
	path := filepath.Join(transcripts, sideChatParentID, "subagents", sideChatAgentID+".jsonl")

	for transcriptPath, text := range map[string]string{
		path: sideChatText,
		filepath.Join(transcripts, sideChatParentID, sideChatParentID+".jsonl"): sideChatParentText,
		filepath.Join(
			transcripts,
			sideChatAgentID,
			sideChatAgentID+".jsonl",
		): sideChatText + " " + sideChatTwinText,
	} {
		installLocalProofTranscript(
			t,
			transcriptPath,
			`{"role":"user","message":{"role":"user","content":[{"type":"text","text":"`+text+`"}]}}`+"\n",
		)
	}

	dataRoot := filepath.Join(providerRoot, "CLYDE_CURSOR_DATA_DIRS")

	for fixture, databasePath := range map[string]string{
		"cursor_side_chat_global.sql": filepath.Join(dataRoot, "globalStorage", "state.vscdb"),
		"cursor_side_chat_workspace.sql": filepath.Join(
			dataRoot,
			"workspaceStorage",
			"probe",
			"state.vscdb",
		),
	} {
		statements, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}

		if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
			t.Fatal(err)
		}

		database, err := sql.Open("sqlite3", "file:"+databasePath)
		if err != nil {
			t.Fatal(err)
		}

		_, executeErr := database.Exec(string(statements))
		if err := errors.Join(executeErr, database.Close()); err != nil {
			t.Fatal(err)
		}
	}

	writeLocalProofFile(
		t,
		filepath.Join(dataRoot, "workspaceStorage", "probe", "workspace.json"),
		`{"folder":"file:///side-chat-export-probe"}`,
	)

	return path
}
