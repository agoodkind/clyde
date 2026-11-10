//go:build milvusintegration

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	nativeGapComposerID = "c7c7c7c7-0000-4000-8000-000000000001"
	nativeGapFirst      = "Inspect the tree."
	nativeGapSecond     = "Inspect it again."
	nativeGapThird      = "Run the tests."
	nativeGapSame       = "same"
	nativeGapNext       = "next"
	nativeGapAck        = "ack"
	nativeGapGit        = "git status --short"
	nativeGapTest       = "go test ./..."
	nativeGapReply      = "The tree is clean."
)

var nativeGapBase = time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC)

type nativeGapLine struct {
	user    string
	command string
	reply   string
}

type nativeGapScenario struct {
	name    string
	native  func(*testing.T) []userBoundaryNativeMessage
	omitted map[int]bool
	bubbles []userBoundaryBubble
	lines   []nativeGapLine
}

type nativeGapResult struct {
	seeded          map[string]sharedStoredRow
	seededVectors   map[string][]uint32
	repaired        map[string]sharedStoredRow
	repairedVectors map[string][]uint32
	evidence        []vectorsearch.SourceRepairEvidence
}

func nativeUser(text string) userBoundaryNativeMessage {
	return userBoundaryNativeMessage{
		Role: "user",
		Content: []userBoundaryNativePart{
			{Type: "text", Text: text, ToolCallID: "", ToolName: "", Args: nil},
		},
	}
}

func nativeReply(text string) userBoundaryNativeMessage {
	return userBoundaryNativeMessage{
		Role: "assistant",
		Content: []userBoundaryNativePart{
			{Type: "text", Text: text, ToolCallID: "", ToolName: "", Args: nil},
		},
	}
}

func nativeCommand(t *testing.T, callID string, command string) userBoundaryNativeMessage {
	t.Helper()
	marshal := combinedCopyJSON(t)
	args := json.RawMessage(marshal(json.Marshal(userBoundaryArgs{Command: command})))
	return userBoundaryNativeMessage{
		Role: "assistant",
		Content: []userBoundaryNativePart{
			{Type: "tool-call", Text: "", ToolCallID: callID, ToolName: "Shell", Args: args},
		},
	}
}

func nativeMinute(minute int) time.Time {
	return nativeGapBase.Add(time.Duration(minute) * time.Minute)
}

func nativeTextBubble(kind int, id string, text string, minute int) userBoundaryBubble {
	return userBoundaryBubble{
		Version:        3,
		Type:           kind,
		BubbleID:       id,
		Text:           text,
		CreatedAt:      nativeMinute(minute).Format(time.RFC3339Nano),
		ToolFormerData: nil,
	}
}

func nativeToolBubble(id string, callID string, minute int) userBoundaryBubble {
	return userBoundaryBubble{
		Version:   3,
		Type:      2,
		BubbleID:  id,
		Text:      "",
		CreatedAt: nativeMinute(minute).Format(time.RFC3339Nano),
		ToolFormerData: &userBoundaryToolFormer{
			Name:       "run_terminal_command_v2",
			RawArgs:    "",
			Status:     "completed",
			ToolCallID: callID,
		},
	}
}

func savedUser(text string) nativeGapLine {
	return nativeGapLine{user: text, command: "", reply: ""}
}

func savedCommand(command string) nativeGapLine {
	return nativeGapLine{user: "", command: command, reply: ""}
}

func savedReply(text string) nativeGapLine {
	return nativeGapLine{user: "", command: "", reply: text}
}

// repairAcrossNativeGap stores the scenario's composer and transcript, seeds rows
// from the transcript, and runs source repair.
func repairAcrossNativeGap(t *testing.T, scenario nativeGapScenario) nativeGapResult {
	t.Helper()
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	dataRoot := filepath.Join(home, "cursor-data")
	projects := filepath.Join(home, "cursor-projects")
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", dataRoot)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projects)
	marshal := combinedCopyJSON(t)
	writeNativeComposerStoreOmitting(
		t,
		dataRoot,
		nativeGapComposerID,
		scenario.native(t),
		scenario.omitted,
		scenario.bubbles,
		nil,
	)

	lines := make([]string, 0, len(scenario.lines))
	for _, entry := range scenario.lines {
		var line userBoundaryLine
		switch {
		case entry.user != "":
			line.Role = "user"
			line.Message.Content = []userBoundaryLinePart{
				{Type: "text", Text: entry.user, Name: "", Input: nil},
			}
		case entry.command != "":
			line.Role = "assistant"
			input := json.RawMessage(
				marshal(json.Marshal(userBoundaryArgs{Command: entry.command})),
			)
			line.Message.Content = []userBoundaryLinePart{
				{Type: "tool_use", Text: "", Name: "Shell", Input: input},
			}
		default:
			line.Role = "assistant"
			line.Message.Content = []userBoundaryLinePart{
				{Type: "text", Text: entry.reply, Name: "", Input: nil},
			}
		}
		lines = append(lines, marshal(json.Marshal(line)))
	}
	transcriptPath := filepath.Join(
		projects,
		repairProjectKey,
		"agent-transcripts",
		nativeGapComposerID,
		nativeGapComposerID+".jsonl",
	)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("cannot create transcript directory: %v", err)
	}
	if err := os.WriteFile(
		transcriptPath,
		[]byte(strings.Join(lines, "\n")+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("cannot write transcript: %v", err)
	}

	ctx := context.Background()
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("cannot refresh conversation index: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("cannot list indexed conversations: %v", err)
	}
	var record conversation.Record
	for _, entry := range stamped {
		if entry.Record.NativeID == nativeGapComposerID &&
			strings.HasSuffix(entry.Record.ArtifactPath, ".jsonl") {
			record = entry.Record
		}
	}
	if record.ID == "" {
		t.Fatalf("index did not discover the transcript; indexed conversations: %d", len(stamped))
	}
	kinds := backfillTestContentKinds(t)
	messages, err := index.LoadMessagesWithOptions(
		record,
		daemonsvc.SemanticConversationLoadOptions(kinds),
	)
	if err != nil {
		t.Fatalf("cannot load transcript messages: %v", err)
	}
	built, err := daemonsvc.BuildSemanticConversationDocuments(record, messages, kinds)
	if err != nil {
		t.Fatalf("cannot build semantic conversation documents: %v", err)
	}
	unknown := time.Time{}.Unix()
	docs := make([]semsearch.SemDoc, 0, len(built.Docs))
	for _, doc := range built.Docs {
		doc.TimestampUnix = unknown
		tools := make([]semsearch.SemToolCall, 0, len(doc.Tools))
		for _, tool := range doc.Tools {
			tool.TimestampUnix = nil
			tool.TimestampUnknown = true
			tools = append(tools, tool)
		}
		doc.Tools = tools
		docs = append(docs, doc)
	}
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := []semsearch.Fingerprint{{ConversationID: record.ID, Value: "native-gap"}}
	if _, err := client.UpsertConversationDocuments(
		ctx,
		repairCollectionID,
		docs,
		manifest,
	); err != nil {
		t.Fatalf("cannot seed conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, seededVectors := sharedSnapshot(t, address)
	var evidence bytes.Buffer
	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   kinds,
		index:          index,
		client:         client,
		out:            nil,
		sourceTotals:   newSourceRepairTotals(),
		evidenceOut:    &evidence,
	}
	options := repairTimestampsOptions{
		DryRun:         false,
		Provider:       "",
		ConversationID: record.ID,
		Limit:          0,
		MaxRows:        100,
		After:          "",
		Resume:         false,
		ProgressPath:   filepath.Join(home, "progress.json"),
		SourceStore:    true,
		EvidencePath:   "",
	}
	if _, err := runRepair(t, ctx, base, options); err != nil {
		t.Fatalf("cannot repair timestamps from source store: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	entries := make([]vectorsearch.SourceRepairEvidence, 0)
	for line := range strings.SplitSeq(strings.TrimSpace(evidence.String()), "\n") {
		var entry vectorsearch.SourceRepairEvidence
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("cannot decode source repair evidence %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return nativeGapResult{
		seeded:          seeded,
		seededVectors:   seededVectors,
		repaired:        repaired,
		repairedVectors: repairedVectors,
		evidence:        entries,
	}
}

// requireStoredFieldsPreserved checks that repair preserves row counts, vector counts and bits, content, paths, vector keys, and unrelated scalar fields.
func requireStoredFieldsPreserved(t *testing.T, result nativeGapResult) {
	t.Helper()
	if len(result.repaired) != len(result.seeded) ||
		len(result.repairedVectors) != len(result.seededVectors) {
		t.Fatalf(
			"repair changed row or vector counts; repaired rows, seeded rows, repaired vectors, seeded vectors: %d, %d, %d, %d",
			len(result.repaired),
			len(result.seeded),
			len(result.repairedVectors),
			len(result.seededVectors),
		)
	}
	for key, bits := range result.seededVectors {
		if !slices.Equal(bits, result.repairedVectors[key]) {
			t.Fatalf("repair changed vector bits; vector key: %s", key)
		}
	}
	for id, after := range result.repaired {
		before := result.seeded[id]
		if after.content != before.content || after.relativePath != before.relativePath ||
			after.vectorKey != before.vectorKey ||
			after.others != before.others {
			t.Fatalf(
				"repair changed stored fields unrelated to timestamps; row ID, before, after: %s: %+v, %+v",
				id,
				before,
				after,
			)
		}
	}
}

// TestRepairKeepsEveryRowUnknownWhenANativeRecordIsMissing checks that repair keeps every row unknown, reports native_gap_unproven, and preserves other stored fields and vectors when a native record is missing.
func TestRepairKeepsEveryRowUnknownWhenANativeRecordIsMissing(t *testing.T) {
	scenarios := []nativeGapScenario{
		{
			name: "identical tool call missing",
			native: func(t *testing.T) []userBoundaryNativeMessage {
				t.Helper()
				return []userBoundaryNativeMessage{
					nativeUser(nativeGapFirst),
					nativeCommand(t, "call_a", nativeGapGit),
					nativeCommand(t, "call_b", nativeGapGit),
				}
			},
			omitted: map[int]bool{2: true},
			bubbles: []userBoundaryBubble{
				nativeTextBubble(1, "u1", nativeGapFirst, 0),
				nativeToolBubble("t1", "call_a", 1),
				nativeToolBubble("t2", "call_b", 3),
			},
			lines: []nativeGapLine{savedUser(nativeGapFirst), savedCommand(nativeGapGit)},
		},
		{
			name: "repeated user text aligns the saved history to earlier native turns",
			native: func(t *testing.T) []userBoundaryNativeMessage {
				t.Helper()
				return []userBoundaryNativeMessage{
					nativeUser(nativeGapSame),
					nativeCommand(t, "call_a", nativeGapGit),
					nativeUser(nativeGapSame),
					nativeCommand(t, "call_b", nativeGapGit),
					nativeUser(nativeGapSame + "\n"),
					nativeReply(nativeGapAck),
					nativeUser(nativeGapNext),
					nativeCommand(t, "call_c", nativeGapTest),
				}
			},
			omitted: map[int]bool{4: true},
			bubbles: []userBoundaryBubble{
				nativeTextBubble(1, "u1", nativeGapSame, 0),
				nativeToolBubble("t1", "call_a", 1),
				nativeTextBubble(1, "u2", nativeGapSame, 2),
				nativeToolBubble("t2", "call_b", 3),
				nativeTextBubble(2, "a1", nativeGapAck, 4),
				nativeTextBubble(1, "u3", nativeGapNext, 5),
				nativeToolBubble("t3", "call_c", 6),
			},
			lines: []nativeGapLine{
				savedUser(nativeGapSame),
				savedCommand(nativeGapGit),
				savedUser(nativeGapSame),
				savedReply(nativeGapAck),
				savedUser(nativeGapNext),
				savedCommand(nativeGapTest),
			},
		},
		{
			name: "complete user segments around the missing record",
			native: func(t *testing.T) []userBoundaryNativeMessage {
				t.Helper()
				return []userBoundaryNativeMessage{
					nativeUser(nativeGapFirst),
					nativeCommand(t, "call_a", nativeGapGit),
					nativeUser(nativeGapSecond),
					nativeCommand(t, "call_lost", nativeGapGit),
					nativeUser(nativeGapThird),
					nativeCommand(t, "call_b", nativeGapTest),
				}
			},
			omitted: map[int]bool{3: true},
			bubbles: []userBoundaryBubble{
				nativeTextBubble(1, "u1", nativeGapFirst, 0),
				nativeToolBubble("t1", "call_a", 1),
				nativeTextBubble(1, "u2", nativeGapSecond, 2),
				nativeTextBubble(1, "u3", nativeGapThird, 4),
				nativeToolBubble("t2", "call_b", 5),
			},
			lines: []nativeGapLine{
				savedUser(nativeGapFirst),
				savedCommand(nativeGapGit),
				savedUser(nativeGapSecond),
				savedCommand(nativeGapGit),
				savedUser(nativeGapThird),
				savedCommand(nativeGapTest),
			},
		},
		{
			name: "identical assistant text missing",
			native: func(*testing.T) []userBoundaryNativeMessage {
				return []userBoundaryNativeMessage{
					nativeUser(nativeGapFirst),
					nativeReply(nativeGapReply),
					nativeReply(nativeGapReply + "\n"),
				}
			},
			omitted: map[int]bool{2: true},
			bubbles: []userBoundaryBubble{
				nativeTextBubble(1, "u1", nativeGapFirst, 0),
				nativeTextBubble(2, "a1", nativeGapReply, 1),
			},
			lines: []nativeGapLine{savedUser(nativeGapFirst), savedReply(nativeGapReply)},
		},
	}
	for _, scenario := range scenarios {
		t.Run(
			scenario.name,
			func(t *testing.T) {
				result := repairAcrossNativeGap(t, scenario)
				requireStoredFieldsPreserved(t, result)
				if len(result.repaired) == 0 || len(result.evidence) == 0 {
					t.Fatalf(
						"repair did not produce both rows and evidence; row count, evidence count: %d, %d",
						len(result.repaired),
						len(result.evidence),
					)
				}
				for _, row := range result.repaired {
					if !row.timestampNull {
						t.Fatalf("repair dated a row that must remain unknown; row: %+v", row)
					}
				}
				for _, entry := range result.evidence {
					if entry.Outcome != vectorsearch.SourceOutcomeNativeGap ||
						entry.ToolCallID != "" ||
						entry.Chosen != 0 {
						t.Fatalf(
							"repair evidence differs from the expected native gap result; evidence: %+v",
							entry,
						)
					}
				}
			},
		)
	}
}
