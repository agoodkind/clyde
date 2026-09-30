package searchacceptance_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	"goodkind.io/clyde/internal/searchacceptance"
)

const sourceExportChildRequest = "CLYDE_SOURCE_EXPORT_TEST_REQUEST"

func TestFrozenSourceExportChild(t *testing.T) {
	path := os.Getenv(sourceExportChildRequest)
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request searchacceptance.FrozenSourceRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if _, err := searchacceptance.ExportFrozenSources(t.Context(), request, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestExportFrozenSourcesUsesPhysicalVirtualAndSelectedArtifacts(t *testing.T) {
	request, chat, originalClaude := createFrozenSourceExportFixture(t, 5)
	output, err := runFrozenSourceExportChild(t, request)
	if err != nil {
		t.Fatalf("real source export failed: %v\n%s", err, output)
	}
	rows, summary := decodeFrozenSourceExport(t, output)
	if !summary.Complete || summary.Records != 3 || summary.AdmittedRecords != 3 || summary.Occurrences != 7 || len(rows) != 7 {
		t.Fatalf("source summary = %+v, rows=%d", summary, len(rows))
	}
	spans := [][2]int64{{0, 3686}, {3686, 7372}, {7372, 8000}}
	for position, span := range spans {
		row := rows[position]
		if row.Identity.ConversationID != conversation.DerivedID(conversation.ProviderClaude, "", originalClaude) || row.OriginalArtifactPath != originalClaude || row.Identity.MessageIndex != 0 || row.Identity.ContentKind != "chat" || row.Identity.ToolIndex != -1 || row.Identity.SourceByteStart != span[0] || row.Identity.SourceByteEnd != span[1] || row.SourceDigest != searchacceptance.Digest([]byte(chat[span[0]:span[1]])) {
			t.Fatalf("original source identity or span drifted: %+v", row)
		}
	}
	expected := []string{"frozen composer question", "frozen composer answer", "subagent prompt", "subagent answer"}
	for index, text := range expected {
		row := rows[index+3]
		if row.SourceDigest != searchacceptance.Digest([]byte(text)) || row.Identity.SourceByteStart != 0 || row.Identity.SourceByteEnd != int64(len(text)) || row.Identity.MessageIndex != index%2 {
			t.Fatalf("provider source differs: %+v", row)
		}
		if index >= 2 && (row.Selector != "agent-1" || !row.Subagent || row.Identity.ConversationID != "copilot:session-1:agent:agent-1") {
			t.Fatalf("selected subagent identity differs: %+v", row)
		}
	}
}

func TestExportFrozenSourcesRejectsUnknownIndexWithoutCompletion(t *testing.T) {
	request, _, _ := createFrozenSourceExportFixture(t, 6)
	output, err := runFrozenSourceExportChild(t, request)
	if err == nil {
		t.Fatal("unknown index version accepted")
	}
	_, summary := decodeFrozenSourceExport(t, output)
	if summary.Complete || summary.ExcludedRecords != 0 || summary.Error == "" {
		t.Fatalf("failed export claimed completion or exclusion: %+v", summary)
	}
}

func TestExportFrozenSourcesRejectsUnreadableVirtualSourceWithoutExclusion(t *testing.T) {
	request, _, _ := createFrozenSourceExportFixture(t, 5)
	path := filepath.Join(request.SnapshotRoot, "home/Library/Application Support/Cursor/User/globalStorage/state.vscdb")
	writeFrozenSourceBytes(t, path, []byte("invalid sqlite database"))
	var files []string
	for relative := range request.ManifestFiles {
		files = append(files, strings.TrimPrefix(relative, "./"))
	}
	request.Verification, request.ManifestDigest = verifyFrozenSourceFixture(t, request.SnapshotRoot, files)
	output, err := runFrozenSourceExportChild(t, request)
	if err == nil {
		t.Fatal("unreadable virtual source accepted")
	}
	rows, summary := decodeFrozenSourceExport(t, output)
	if summary.Complete || summary.ExcludedRecords != 0 || summary.Records != 1 || summary.FailedRecordID != "cursor:33333333-3333-4333-8333-333333333333" || len(rows) != 3 || summary.Error == "" {
		t.Fatalf("partial export claimed completion or exclusion: %+v, rows=%d", summary, len(rows))
	}
}

func TestExportFrozenSourcesRejectsInvalidProvenanceAndModel(t *testing.T) {
	request, _, _ := createFrozenSourceExportFixture(t, 5)
	invalidDigest := request
	invalidDigest.ManifestDigest = "not-a-sha256"
	invalidDigest.Verification.ManifestDigest = invalidDigest.ManifestDigest
	incompleteModel := request
	incompleteModel.Model.Revision = ""
	mismatchedModel := request
	mismatchedModel.Model.Dimension++
	for name, candidate := range map[string]searchacceptance.FrozenSourceRequest{"digest": invalidDigest, "incomplete model": incompleteModel, "mismatched model": mismatchedModel} {
		t.Run(name, func(t *testing.T) {
			output, err := runFrozenSourceExportChild(t, candidate)
			if err == nil {
				t.Fatal("invalid source contract accepted")
			}
			rows, summary := decodeFrozenSourceExport(t, output)
			if summary.Complete || summary.Records != 0 || len(rows) != 0 || summary.Error == "" {
				t.Fatalf("invalid source contract emitted rows: %+v", summary)
			}
		})
	}
}

func createFrozenSourceExportFixture(t *testing.T, version int) (searchacceptance.FrozenSourceRequest, string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "original")
	chat := strings.Repeat("a", 4000) + "\x00" + strings.Repeat("b", 3999)
	encodedChat, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"home/.claude/projects/subagents/agent.jsonl", "home/Library/Application Support/Cursor/User/globalStorage/state.vscdb", "home/.copilot/session-state/session-1/events.jsonl", "clyde-cache/conversation-index.json"}
	manifest := createSourcePathManifest(t, root, files)
	claudePath := filepath.Join(home, ".claude/projects/subagents/agent.jsonl")
	claude := sourcePathRecord(conversation.ProviderClaude, claudePath)
	claude.ID = conversation.DerivedID(claude.Provider, "", claudePath)
	claude.NativeID = ""
	writeFrozenSourceBytes(t, filepath.Join(root, files[0]), []byte(fmt.Sprintf(`{"sessionId":"borrowed-parent","isSidechain":true,"uuid":"source","type":"user","timestamp":"2026-09-28T07:00:00Z","message":{"role":"user","content":%s}}
`, encodedChat)))
	createFrozenCursorExportDB(t, filepath.Join(root, files[1]))
	cursor := sourcePathRecord(conversation.ProviderCursor, cursorparser.BuildVirtualPath(cursorparser.RootHash(filepath.Join(home, "Library/Application Support/Cursor/User")), cursorparser.VirtualKindComposer, "33333333-3333-4333-8333-333333333333"))
	cursor.ID = "cursor:33333333-3333-4333-8333-333333333333"
	copilot := sourcePathRecord(conversation.ProviderCopilot, filepath.Join(home, ".copilot/session-state/session-1/events.jsonl"))
	copilot.ID = "copilot:session-1:agent:agent-1"
	copilot.NativeID = "session-1:agent:agent-1"
	copilot.Selector = "agent-1"
	writeFrozenSourceBytes(t, filepath.Join(root, files[2]), []byte(`{"id":"1","timestamp":"2026-09-28T07:00:00Z","type":"session.start","data":{"sessionId":"session-1","version":1,"context":{"cwd":"/source"}}}
{"id":"2","timestamp":"2026-09-28T07:00:01Z","type":"user.message","data":{"content":"root request"}}
{"id":"3","timestamp":"2026-09-28T07:00:02Z","agentId":"agent-1","type":"subagent.started","data":{"agentDisplayName":"Researcher","toolCallId":"call-agent"}}
{"id":"4","timestamp":"2026-09-28T07:00:03Z","agentId":"agent-1","type":"user.message","data":{"content":"subagent prompt","source":"agent-agent-1"}}
{"id":"5","timestamp":"2026-09-28T07:00:04Z","agentId":"agent-1","type":"assistant.message","data":{"content":"subagent answer","messageId":"answer"}}
`))
	index := struct {
		Version int                   `json:"version"`
		Records []conversation.Record `json:"records"`
	}{Version: version, Records: []conversation.Record{claude, cursor, copilot}}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	writeFrozenSourceBytes(t, filepath.Join(root, files[3]), indexData)
	verification, digest := verifyFrozenSourceFixture(t, root, files)
	semantic := config.NewConfigWithDefaults().Conversation.Semantic
	semantic.ProjectionProfile = config.ConversationProjectionProfileSourceSpan
	semantic.IncludeSubagents = true
	semantic.IncludeArchived = true
	semantic.EmbeddingModel = "NV-EmbedCode-7b-v1"
	semantic.EmbeddingRevision = "fixture"
	semantic.VectorDimension = 4096
	semantic.Normalization = "l2"
	request := searchacceptance.FrozenSourceRequest{SnapshotRoot: root, OriginalHome: home, ManifestFiles: manifest, ManifestDigest: digest, Verification: verification, Semantic: semantic, Model: searchacceptance.Model{Name: "NV-EmbedCode-7b-v1", Revision: "fixture", Dimension: 4096, Normalization: "l2"}, ArtifactSettled: true}
	return request, chat, claudePath
}

func createFrozenCursorExportDB(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	statements, err := os.ReadFile("testdata/source-export-cursor.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(statements)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeFrozenSourceBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func verifyFrozenSourceFixture(t *testing.T, root string, files []string) (searchacceptance.SnapshotVerification, string) {
	t.Helper()
	var manifest strings.Builder
	for _, relative := range files {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&manifest, "%s  ./%s\n", searchacceptance.Digest(data), relative)
	}
	path := filepath.Join(root, "MANIFEST.sha256")
	writeFrozenSourceBytes(t, path, []byte(manifest.String()))
	digest := searchacceptance.Digest([]byte(manifest.String()))
	verification, err := searchacceptance.VerifySnapshot(t.Context(), root, path, digest)
	if err != nil {
		t.Fatal(err)
	}
	return verification, digest
}

func runFrozenSourceExportChild(t *testing.T, request searchacceptance.FrozenSourceRequest) ([]byte, error) {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "request.json")
	writeFrozenSourceBytes(t, path, data)
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFrozenSourceExportChild$")
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, sourceExportChildRequest+"=") || strings.HasPrefix(value, "CLYDE_CURSOR_DATA_DIRS=") || strings.HasPrefix(value, "CLYDE_ZED_DATA_DIRS=") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env, sourceExportChildRequest+"="+path)
	for _, provider := range []conversation.Provider{conversation.ProviderCursor, conversation.ProviderZed} {
		environment, err := searchacceptance.FrozenProviderEnvironment(request.SnapshotRoot, provider)
		if err != nil {
			t.Fatal(err)
		}
		command.Env = append(command.Env, environment.Name+"="+environment.Value)
	}
	var errors bytes.Buffer
	command.Stderr = &errors
	output, err := command.Output()
	if err != nil {
		t.Log(errors.String())
	}
	return output, err
}

func decodeFrozenSourceExport(t *testing.T, output []byte) ([]searchacceptance.FrozenSourceOccurrence, searchacceptance.FrozenSourceSummary) {
	t.Helper()
	var rows []searchacceptance.FrozenSourceOccurrence
	var summary searchacceptance.FrozenSourceSummary
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &kind); err != nil {
			t.Fatalf("invalid source JSONL: %v: %s", err, line)
		}
		switch kind.Type {
		case "occurrence":
			var row searchacceptance.FrozenSourceOccurrence
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		case "summary":
			if err := json.Unmarshal(line, &summary); err != nil {
				t.Fatal(err)
			}
		case "header", "record":
		default:
			t.Fatalf("unexpected source event: %q", kind.Type)
		}
	}
	return rows, summary
}
