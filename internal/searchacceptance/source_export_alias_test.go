package searchacceptance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	"goodkind.io/clyde/internal/searchacceptance"
	"goodkind.io/clyde/internal/transcript"
)

func TestProjectEmbeddedConversationAliasesRejectsCanceledExcludedSource(t *testing.T) {
	request, _ := createFrozenAliasFixture(t, "compatible", false)
	request.Semantic.IndexedProviders = []string{"claude"}
	record := sourcePathRecord(conversation.ProviderCodex, filepath.Join(request.SnapshotRoot, "home/.codex/sessions/alias-long.jsonl"))
	record.ID, record.NativeID = "codex:frozen-alias-thread", "frozen-alias-thread"
	registry := conversation.NewRegistry()
	registry.Register(codexparser.New())
	reader := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := daemon.ProjectEmbeddedConversationAliases(ctx, request.Semantic, []conversation.Record{record}, func(_ context.Context, source conversation.Record) ([]transcript.Message, bool, error) {
		messages, readErr := reader.LoadMessagesWithOptions(source, daemon.SemanticConversationLoadOptions(conversation.ContentKindSet{}))
		if readErr != nil {
			return nil, false, fmt.Errorf("read real canceled alias fixture: %w", readErr)
		}
		return messages, true, nil
	})
	if !errors.Is(err, context.Canceled) || result.Projection.Admitted || len(result.Projection.Occurrences) != 0 {
		t.Fatalf("canceled excluded source returned %+v, %v", result, err)
	}
}

func TestExportFrozenSourcesAcceptsCompatibleAliases(t *testing.T) {
	var first []searchacceptance.FrozenSourceOccurrence
	for _, reverse := range []bool{false, true} {
		request, longest := createFrozenAliasFixture(t, "compatible", reverse)
		output, err := runFrozenSourceExportChild(t, request)
		if err != nil {
			t.Fatalf("compatible aliases rejected: %v\n%s", err, output)
		}
		rows, summary := decodeFrozenSourceExport(t, output)
		if !summary.Complete || summary.Records != 3 || summary.Owners != 1 || summary.AdmittedRecords != 3 || summary.Occurrences != 2 || len(rows) != 2 {
			t.Fatalf("compatible alias summary differs: %+v, rows=%d", summary, len(rows))
		}
		for index, text := range []string{"first frozen alias message", "second frozen alias message"} {
			row := rows[index]
			if row.Identity.ConversationID != "codex:frozen-alias-thread" || row.OriginalArtifactPath != longest || row.Identity.MessageIndex != index || row.Identity.ContentKind != "chat" || row.Identity.ToolIndex != -1 || row.Identity.SourceByteStart != 0 || row.Identity.SourceByteEnd != int64(len(text)) || row.SourceDigest != searchacceptance.Digest([]byte(text)) {
				t.Fatalf("selected original occurrence differs: %+v", row)
			}
		}
		if rows[0].IdentityKey == rows[1].IdentityKey {
			t.Fatal("compatible aliases repeated an occurrence identity")
		}
		if first == nil {
			first = rows
		} else {
			for index := range rows {
				if rows[index].Identity != first[index].Identity || rows[index].SourceDigest != first[index].SourceDigest || rows[index].PreparedInputDigest != first[index].PreparedInputDigest {
					t.Fatal("index order changed occurrence identity or digest")
				}
			}
		}
		assertFrozenAliasProvenance(t, output, longest)
	}
}

func TestExportFrozenSourcesRejectsConflictingAliases(t *testing.T) {
	for scenario, reason := range map[string]string{
		"divergent":          "projected aliases diverge",
		"filtered-divergent": "projected aliases diverge",
		"parent":             "alias parent differs",
		"metadata":           "cold alias metadata differs",
		"unreadable":         "load alias source",
		"unsettled":          "open trailing field",
	} {
		t.Run(scenario, func(t *testing.T) {
			request, _ := createFrozenAliasFixture(t, scenario, false)
			output, err := runFrozenSourceExportChild(t, request)
			if err == nil {
				t.Fatal("conflicting aliases accepted")
			}
			rows, summary := decodeFrozenSourceExport(t, output)
			owner := "codex:frozen-alias-thread"
			if scenario == "unsettled" {
				owner = "cursor:frozen-alias-thread"
			}
			if summary.Complete || summary.FailedRecordID != owner || !strings.Contains(summary.Error, reason) || len(rows) != 0 || summary.Occurrences != 0 || summary.ExcludedRecords != 0 {
				t.Fatalf("conflicting owner emitted occurrences or wrong failure: %+v, rows=%d", summary, len(rows))
			}
			assertFrozenAliasConflict(t, scenario, request.OriginalHome, summary.Conflict)
		})
	}
}

func assertFrozenAliasConflict(t *testing.T, scenario, originalHome string, conflict *daemon.EmbeddedAliasConflictError) {
	t.Helper()
	if scenario == "unreadable" || scenario == "unsettled" {
		if conflict != nil {
			t.Fatalf("noncomparison failure returned comparison proof: %+v", conflict)
		}
		return
	}
	if conflict == nil || conflict.OwnerID != "codex:frozen-alias-thread" || conflict.LeftSourceKey != "codex\x00"+filepath.Join(originalHome, ".codex/sessions/alias-long.jsonl")+"\x00frozen-alias-thread\x00" || conflict.RightSourceKey != "codex\x00"+filepath.Join(originalHome, ".codex/sessions/alias-prefix.jsonl")+"\x00frozen-alias-thread\x00" {
		t.Fatalf("comparison source attribution differs: %+v", conflict)
	}
	switch scenario {
	case "parent":
		if conflict.Reason != daemon.EmbeddedAliasConflictParent || !reflect.DeepEqual(conflict.Properties, []string{"parent_conversation_id"}) || conflict.LeftParent == nil || !conflict.LeftParent.Null || conflict.RightParent == nil || conflict.RightParent.Null || conflict.RightParent.String != "codex:different-parent" || conflict.FieldIndex != nil || conflict.LeftField != nil || conflict.LeftMetadata != nil {
			t.Fatalf("parent comparison proof differs: %+v", conflict)
		}
	case "metadata":
		if conflict.Reason != daemon.EmbeddedAliasConflictColdMetadata || !reflect.DeepEqual(conflict.Properties, []string{"workspace_root"}) || conflict.LeftMetadata == nil || conflict.LeftMetadata.WorkspaceRoot != "/source" || conflict.RightMetadata == nil || conflict.RightMetadata.WorkspaceRoot != "/different-workspace" || conflict.LeftParent != nil || conflict.LeftField != nil {
			t.Fatalf("metadata comparison proof differs: %+v", conflict)
		}
	default:
		index, role, timestamp := 0, "user", "2026-09-28T07:00:01Z"
		if scenario == "filtered-divergent" {
			index, role, timestamp = 1, "assistant", "2026-09-28T07:00:02Z"
		}
		if conflict.Reason != daemon.EmbeddedAliasConflictFieldPrefix || conflict.FieldIndex == nil || *conflict.FieldIndex != index || !reflect.DeepEqual(conflict.Properties, []string{"digest"}) || conflict.LeftField == nil || conflict.RightField == nil || conflict.LeftParent != nil || conflict.LeftMetadata != nil {
			t.Fatalf("field comparison proof differs: %+v", conflict)
		}
		for _, field := range []*daemon.EmbeddedAliasFieldProof{conflict.LeftField, conflict.RightField} {
			if field.Key == "" || field.Digest == "" || field.MessageID != "" || field.MessageIndex != index || field.ToolIndex != -1 || field.Role != role || string(field.Kind) != "chat" || field.Timestamp.Format("2006-01-02T15:04:05Z07:00") != timestamp {
				t.Fatalf("original projected coordinates differ: %+v", field)
			}
		}
		if conflict.LeftField.Key != conflict.RightField.Key || conflict.LeftField.Digest == conflict.RightField.Digest {
			t.Fatal("field comparison omitted same-key differing full-field seals")
		}
	}
}

func createFrozenAliasFixture(t *testing.T, scenario string, reverse bool) (searchacceptance.FrozenSourceRequest, string) {
	t.Helper()
	request, _, _ := createFrozenSourceExportFixture(t, 5)
	files := []string{"home/.codex/sessions/alias-empty.jsonl", "home/.codex/sessions/alias-prefix.jsonl", "home/.codex/sessions/alias-long.jsonl", "clyde-cache/conversation-index.json"}
	manifest := createSourcePathManifest(t, request.SnapshotRoot, files)
	header := `{"timestamp":"2026-09-28T07:00:00Z","type":"session_meta","payload":{"id":"frozen-alias-thread","timestamp":"2026-09-28T07:00:00Z","cwd":"/source","source":"cli"}}` + "\n"
	first := frozenAliasEvent(t, "2026-09-28T07:00:01Z", "first frozen alias message")
	second := frozenAliasEvent(t, "2026-09-28T07:00:02Z", "second frozen alias message")
	contents := []string{header, header + first, header + first + second}
	provider := conversation.ProviderCodex
	if scenario == "unsettled" {
		provider = conversation.ProviderCursor
		files = []string{"home/.cursor/projects/project/agent-transcripts/empty/empty.jsonl", "home/.cursor/projects/project/agent-transcripts/prefix/prefix.jsonl", "home/.cursor/projects/project/agent-transcripts/long/long.jsonl", "clyde-cache/conversation-index.json"}
		manifest = createSourcePathManifest(t, request.SnapshotRoot, files)
		user := `{"role":"user","message":{"content":[{"type":"text","text":"first frozen alias message"}]}}` + "\n"
		assistant := `{"role":"assistant","message":{"content":[{"type":"text","text":"open trailing message"}]}}` + "\n"
		contents = []string{"", user, user + assistant}
	}
	if scenario == "divergent" {
		contents[1] = header + frozenAliasEvent(t, "2026-09-28T07:00:01Z", "changed frozen alias message")
	}
	if scenario == "filtered-divergent" {
		request.Semantic.IndexedRoles = []string{"user"}
		assistant := `{"timestamp":"2026-09-28T07:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"%s"}}` + "\n"
		contents[1] = header + first + fmt.Sprintf(assistant, "different excluded assistant message")
		contents[2] = header + first + fmt.Sprintf(assistant, "original excluded assistant message")
	}
	var records []conversation.Record
	for index, relative := range files[:3] {
		writeFrozenSourceBytes(t, filepath.Join(request.SnapshotRoot, relative), []byte(contents[index]))
		original := filepath.Join(request.OriginalHome, strings.TrimPrefix(relative, "home/"))
		record := sourcePathRecord(provider, original)
		record.ID, record.NativeID = provider.String()+":frozen-alias-thread", "frozen-alias-thread"
		if scenario == "unsettled" {
			record.ArtifactKind = string(conversation.ArtifactKindCursorAgentTranscript)
		}
		record.WorkspaceRoot = "/source"
		records = append(records, record)
	}
	if scenario == "parent" {
		records[1].Lineage = &conversation.Lineage{ParentProvider: conversation.ProviderCodex, ParentNativeID: "different-parent"}
	}
	if scenario == "metadata" {
		records[1].WorkspaceRoot = "/different-workspace"
	}
	longest := records[2].ArtifactPath
	if reverse {
		records[0], records[2] = records[2], records[0]
	}
	index := struct {
		Version int                   `json:"version"`
		Records []conversation.Record `json:"records"`
	}{Version: 5, Records: records}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	writeFrozenSourceBytes(t, filepath.Join(request.SnapshotRoot, files[3]), data)
	request.ManifestFiles = manifest
	request.Verification, request.ManifestDigest = verifyFrozenSourceFixture(t, request.SnapshotRoot, files)
	request.ArtifactSettled = scenario != "unsettled"
	if scenario == "unreadable" {
		path := filepath.Join(request.SnapshotRoot, files[1])
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Error(err)
			}
		})
	}
	return request, longest
}

func frozenAliasEvent(t *testing.T, timestamp, text string) string {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":%s}}`+"\n", timestamp, encoded)
}

func assertFrozenAliasProvenance(t *testing.T, output []byte, longest string) {
	t.Helper()
	type aliasStatus struct {
		Type     string `json:"type"`
		Selected struct {
			ArtifactPath string `json:"artifact_path"`
		} `json:"selected_source"`
		Aliases []struct {
			ArtifactPath string `json:"artifact_path"`
		} `json:"aliases"`
	}
	var statuses []aliasStatus
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		var status aliasStatus
		if err := json.Unmarshal([]byte(line), &status); err != nil {
			t.Fatal(err)
		}
		if status.Type == "record" {
			statuses = append(statuses, status)
		}
	}
	if len(statuses) != 1 || statuses[0].Selected.ArtifactPath != longest || len(statuses[0].Aliases) != 3 {
		t.Fatalf("alias provenance differs: %+v", statuses)
	}
	paths := []string{statuses[0].Aliases[0].ArtifactPath, statuses[0].Aliases[1].ArtifactPath, statuses[0].Aliases[2].ArtifactPath}
	expected := []string{strings.Replace(longest, "alias-long", "alias-empty", 1), longest, strings.Replace(longest, "alias-long", "alias-prefix", 1)}
	if !reflect.DeepEqual(paths, expected) {
		t.Fatalf("alias provenance order differs: %v", paths)
	}
}
