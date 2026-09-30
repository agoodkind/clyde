package daemon

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
	"goodkind.io/clyde/internal/transcript"
)

// TestEmbeddedProjectionBoundary writes real provider artifacts into temporary
// stores, loads them through the production parser registry and index, and
// projects them with projectEmbeddedConversationFields. Each subtest grows or
// rewrites the artifact and compares the second projection with the fields the
// first one would have committed.
func TestEmbeddedProjectionBoundary(t *testing.T) {
	t.Run("codex_append_keeps_committed_fields", testEmbeddedProjectionCodexAppend)
	t.Run("default_kinds_select_no_tool_output_or_thinking", testEmbeddedProjectionDefaultKinds)
	t.Run("cursor_trailing_turn_waits_for_close", testEmbeddedProjectionCursorTrailingTurn)
	t.Run("zed_rewrite_keeps_committed_occurrence", testEmbeddedProjectionZedRewrite)
}

const (
	embeddedProjectionCodexThreadID      = "019de9aa-3a00-7010-bd9f-a6ee71559357"
	embeddedProjectionCursorConversation = "66666666-6666-4666-8666-666666666666"
	embeddedProjectionCursorProjectKey   = "Users-alice-source-cursor-repo"
	embeddedProjectionZedThreadID        = "thread-rewrite"
	embeddedProjectionShellCommand       = "cd /repo && make test"
	embeddedProjectionShellProgram       = "make"
	embeddedProjectionCodexToolName      = "exec_command"
	embeddedProjectionCodexToolOutput    = "PASS ok"
	embeddedProjectionCodexThinking      = "Run the test target next."
)

// The rollout lines follow the Codex parser fixtures in
// internal/providers/codex/parser/parser_test.go and content_test.go. Message 1
// contains only reasoning. The default content kinds select no field from it.
var embeddedProjectionCodexInitialLines = []string{
	`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"` + embeddedProjectionCodexThreadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`,
	`{"timestamp":"2026-05-02T17:09:05.000Z","type":"event_msg","payload":{"type":"user_message","message":"run the test suite"}}`,
	`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"` + embeddedProjectionCodexThinking + `"}}`,
	`{"timestamp":"2026-05-02T17:09:07.000Z","type":"response_item","payload":{"type":"function_call","name":"` + embeddedProjectionCodexToolName + `","arguments":"{\"cmd\":\"` + embeddedProjectionShellCommand + `\"}","call_id":"call_one"}}`,
	`{"timestamp":"2026-05-02T17:09:08.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_one","output":"` + embeddedProjectionCodexToolOutput + `"}}`,
	`{"timestamp":"2026-05-02T17:09:09.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The suite passed."}]}}`,
}

var embeddedProjectionCodexAppendedLines = []string{
	`{"timestamp":"2026-05-02T17:10:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"now run the linter"}}`,
	`{"timestamp":"2026-05-02T17:10:05.000Z","type":"event_msg","payload":{"type":"agent_message","message":"The linter reported nothing."}}`,
}

func testEmbeddedProjectionCodexAppend(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	rolloutPath := writeEmbeddedProjectionCodexRollout(t, stores)
	index := newEmbeddedProjectionIndex()
	kinds := defaultSemanticContentKinds()

	first := loadEmbeddedProjectionConversation(t, index, conversation.ProviderCodex, kinds)
	if conversation.TrailingMessageMayGrow(first.record) {
		t.Fatalf("codex record artifact kind %q reports a growing trailing message", first.record.ArtifactKind)
	}
	if len(first.messages) != 4 {
		t.Fatalf("codex messages = %d, want 4: %s", len(first.messages), describeEmbeddedProjectionMessages(first.messages))
	}
	reasoningOnly := first.messages[1]
	if reasoningOnly.Thinking == "" || reasoningOnly.Text != "" || len(reasoningOnly.Tools) != 0 {
		t.Fatalf("codex message 1 = %s, want a reasoning-only message", describeEmbeddedProjectionMessages(first.messages[1:2]))
	}
	if len(first.messages[2].Tools) != 1 || first.messages[2].Tools[0].DisplayLang != "bash" {
		t.Fatalf("codex message 2 tools = %+v, want one shell tool call", first.messages[2].Tools)
	}

	firstProjection := projectEmbeddedProjectionFields(t, first, kinds, false)
	if firstProjection.WithheldOpenFields != 0 {
		t.Fatalf("codex withheld fields = %d, want 0", firstProjection.WithheldOpenFields)
	}
	// Message 1 selects no content. Fields for messages 2 and 3 report their
	// loaded positions, not their positions among projected messages.
	assertEmbeddedProjectionLayout(t, firstProjection.Fields, []embeddedProjectionFieldShape{
		{messageIndex: 0, kind: searchbackend.FieldKindChat, toolIndex: -1},
		{messageIndex: 2, kind: searchbackend.FieldKindToolCall, toolIndex: 0},
		{messageIndex: 3, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})
	assertEmbeddedProjectionChatText(t, firstProjection.Fields, first.messages)

	toolField := embeddedProjectionFieldAt(t, firstProjection.Fields, 2, searchbackend.FieldKindToolCall)
	if !strings.HasPrefix(toolField.DocumentPrefix, embeddedProjectionCodexToolName+"\n") {
		t.Fatalf("tool call prefix = %q, want tool attribution", toolField.DocumentPrefix)
	}
	toolLines := strings.Split(toolField.DocumentPrefix, "\n")
	if !slices.Contains(toolLines, embeddedProjectionShellProgram) {
		t.Fatalf("tool call text lines = %q, want the shell program %q", toolLines, embeddedProjectionShellProgram)
	}
	if toolField.Text != first.messages[2].Tools[0].Display {
		t.Fatalf("tool source = %q, want original displayed command %q", toolField.Text, first.messages[2].Tools[0].Display)
	}

	appendEmbeddedProjectionLines(t, rolloutPath, embeddedProjectionCodexAppendedLines)
	second := loadEmbeddedProjectionConversation(t, index, conversation.ProviderCodex, kinds)
	if len(second.messages) != 6 {
		t.Fatalf("codex messages after append = %d, want 6: %s", len(second.messages), describeEmbeddedProjectionMessages(second.messages))
	}
	secondProjection := projectEmbeddedProjectionFields(t, second, kinds, false)
	secondDigests := embeddedProjectionDigests(secondProjection.Fields)
	for _, committedField := range firstProjection.Fields {
		digest, found := secondDigests[committedField.Key]
		if !found {
			t.Fatalf("field key %q from the first load is missing after the append: %q", committedField.Key, embeddedProjectionKeys(secondProjection.Fields))
		}
		if digest != committedField.Digest {
			t.Fatalf("field %q digest changed after an append that did not change its message", committedField.Key)
		}
	}
	selection := searchbackend.SelectNewFields(secondProjection.Fields, embeddedProjectionDigests(firstProjection.Fields))
	if selection.Unchanged != len(firstProjection.Fields) || selection.ChangedCommitted != 0 {
		t.Fatalf("selection unchanged/changed = %d/%d, want %d/0", selection.Unchanged, selection.ChangedCommitted, len(firstProjection.Fields))
	}
	assertEmbeddedProjectionLayout(t, selection.New, []embeddedProjectionFieldShape{
		{messageIndex: 4, kind: searchbackend.FieldKindChat, toolIndex: -1},
		{messageIndex: 5, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})
	assertEmbeddedProjectionChatText(t, selection.New, second.messages)
}

func testEmbeddedProjectionDefaultKinds(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	writeEmbeddedProjectionCodexRollout(t, stores)
	index := newEmbeddedProjectionIndex()
	defaultKinds := defaultSemanticContentKinds()

	loaded := loadEmbeddedProjectionConversation(t, index, conversation.ProviderCodex, defaultKinds)
	// These messages include the tool outputs. The projection is the only step
	// that can exclude them from the fields.
	options := SemanticConversationLoadOptions(defaultKinds)
	options.IncludeToolOutputs = true
	messages, err := index.LoadMessagesWithOptions(loaded.record, options)
	if err != nil {
		t.Fatalf("load codex messages with tool outputs: %v", err)
	}
	withOutputs := embeddedProjectionLoad{record: loaded.record, messages: messages}
	if len(messages) != 4 || len(messages[2].Tools) != 1 || messages[2].Tools[0].Output != embeddedProjectionCodexToolOutput {
		t.Fatalf("codex messages with outputs = %s, want the tool output attached to message 2", describeEmbeddedProjectionMessages(messages))
	}
	if messages[1].Thinking != embeddedProjectionCodexThinking {
		t.Fatalf("codex message 1 thinking = %q, want %q", messages[1].Thinking, embeddedProjectionCodexThinking)
	}

	defaultProjection := projectEmbeddedProjectionFields(t, withOutputs, defaultKinds, false)
	for _, field := range defaultProjection.Fields {
		if field.Kind == searchbackend.FieldKindToolOutput || field.Kind == searchbackend.FieldKindThinking {
			t.Fatalf("default content kinds produced field %q of kind %q", field.Key, field.Kind)
		}
	}
	assertEmbeddedProjectionLayout(t, defaultProjection.Fields, []embeddedProjectionFieldShape{
		{messageIndex: 0, kind: searchbackend.FieldKindChat, toolIndex: -1},
		{messageIndex: 2, kind: searchbackend.FieldKindToolCall, toolIndex: 0},
		{messageIndex: 3, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})

	// Kinds that select thinking and tool outputs produce a thinking field and a
	// tool output field from the same messages.
	widerKinds := conversation.NewContentKindSet(
		conversation.ContentKindChat,
		conversation.ContentKindThinking,
		conversation.ContentKindToolOutputs,
	)
	widerProjection := projectEmbeddedProjectionFields(t, withOutputs, widerKinds, false)
	thinkingField := embeddedProjectionFieldAt(t, widerProjection.Fields, 1, searchbackend.FieldKindThinking)
	if thinkingField.Text != embeddedProjectionCodexThinking {
		t.Fatalf("thinking field text = %q, want %q", thinkingField.Text, embeddedProjectionCodexThinking)
	}
	outputField := embeddedProjectionFieldAt(t, widerProjection.Fields, 2, searchbackend.FieldKindToolOutput)
	if outputField.Text != embeddedProjectionCodexToolOutput || outputField.DocumentPrefix != embeddedProjectionCodexToolName+"\n" {
		t.Fatalf("tool output field prefix/text = %q/%q", outputField.DocumentPrefix, outputField.Text)
	}
}

// The transcript lines follow the Cursor parser fixtures in
// internal/providers/cursor/parser/turns_test.go, where consecutive lines with
// one role form one turn.
func testEmbeddedProjectionCursorTrailingTurn(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	transcriptPath := filepath.Join(
		stores.cursorProjects,
		embeddedProjectionCursorProjectKey,
		"agent-transcripts",
		embeddedProjectionCursorConversation,
		embeddedProjectionCursorConversation+".jsonl",
	)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("create cursor transcript dir: %v", err)
	}
	appendEmbeddedProjectionLines(t, transcriptPath, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"open the config"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"part one"}]}}`,
	})
	index := newEmbeddedProjectionIndex()
	kinds := defaultSemanticContentKinds()

	first := loadEmbeddedProjectionConversation(t, index, conversation.ProviderCursor, kinds)
	if first.record.ArtifactKind != string(conversation.ArtifactKindCursorAgentTranscript) {
		t.Fatalf("cursor artifact kind = %q, want %q", first.record.ArtifactKind, conversation.ArtifactKindCursorAgentTranscript)
	}
	if len(first.messages) != 2 || first.messages[1].Text != "part one" {
		t.Fatalf("cursor messages = %s, want a user turn and the open assistant turn", describeEmbeddedProjectionMessages(first.messages))
	}
	firstOpen := projectEmbeddedProjectionFields(t, first, kinds, false)
	assertEmbeddedProjectionLayout(t, firstOpen.Fields, []embeddedProjectionFieldShape{
		{messageIndex: 0, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})
	if firstOpen.WithheldOpenFields != 1 {
		t.Fatalf("cursor withheld fields = %d, want the trailing turn withheld", firstOpen.WithheldOpenFields)
	}
	firstSettled := projectEmbeddedProjectionFields(t, first, kinds, true)
	assertEmbeddedProjectionLayout(t, firstSettled.Fields, []embeddedProjectionFieldShape{
		{messageIndex: 0, kind: searchbackend.FieldKindChat, toolIndex: -1},
		{messageIndex: 1, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})
	if firstSettled.WithheldOpenFields != 0 {
		t.Fatalf("settled cursor withheld fields = %d, want 0", firstSettled.WithheldOpenFields)
	}

	// A later line in the same assistant turn extends that turn in place.
	appendEmbeddedProjectionLines(t, transcriptPath, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"part two"}]}}`,
	})
	second := loadEmbeddedProjectionConversation(t, index, conversation.ProviderCursor, kinds)
	if len(second.messages) != 2 || second.messages[1].Text != "part one\npart two" {
		t.Fatalf("cursor messages after the same-turn append = %s, want the assistant turn extended", describeEmbeddedProjectionMessages(second.messages))
	}
	secondOpen := projectEmbeddedProjectionFields(t, second, kinds, false)
	if !slices.Equal(embeddedProjectionKeys(secondOpen.Fields), embeddedProjectionKeys(firstOpen.Fields)) ||
		secondOpen.Fields[0].Digest != firstOpen.Fields[0].Digest || secondOpen.WithheldOpenFields != 1 {
		t.Fatalf("open cursor fields after the same-turn append = %q withheld %d, want only the unchanged user field", embeddedProjectionKeys(secondOpen.Fields), secondOpen.WithheldOpenFields)
	}
	// A settled first load committed the shorter turn. The extended turn keeps
	// that committed occurrence under its key.
	secondSettled := projectEmbeddedProjectionFields(t, second, kinds, true)
	settledSelection := searchbackend.SelectNewFields(secondSettled.Fields, embeddedProjectionDigests(firstSettled.Fields))
	if len(settledSelection.New) != 0 || settledSelection.Unchanged != 1 || settledSelection.ChangedCommitted != 1 {
		t.Fatalf("settled selection new/unchanged/changed = %q/%d/%d, want none/1/1",
			embeddedProjectionKeys(settledSelection.New), settledSelection.Unchanged, settledSelection.ChangedCommitted)
	}

	// A user line closes the assistant turn. The next load offers the closed turn.
	appendEmbeddedProjectionLines(t, transcriptPath, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"now apply it"}]}}`,
	})
	third := loadEmbeddedProjectionConversation(t, index, conversation.ProviderCursor, kinds)
	if len(third.messages) != 3 {
		t.Fatalf("cursor messages after the closing user line = %s, want 3", describeEmbeddedProjectionMessages(third.messages))
	}
	thirdOpen := projectEmbeddedProjectionFields(t, third, kinds, false)
	if thirdOpen.WithheldOpenFields != 1 {
		t.Fatalf("cursor withheld fields after the closing user line = %d, want the new user turn withheld", thirdOpen.WithheldOpenFields)
	}
	selection := searchbackend.SelectNewFields(thirdOpen.Fields, embeddedProjectionDigests(firstOpen.Fields))
	if selection.Unchanged != 1 || selection.ChangedCommitted != 0 {
		t.Fatalf("selection unchanged/changed = %d/%d, want 1/0", selection.Unchanged, selection.ChangedCommitted)
	}
	assertEmbeddedProjectionLayout(t, selection.New, []embeddedProjectionFieldShape{
		{messageIndex: 1, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})
	if selection.New[0].Text != "part one\npart two" {
		t.Fatalf("closed turn text = %q, want the complete assistant turn", selection.New[0].Text)
	}
}

// The thread rows follow the Zed parser fixtures in
// internal/providers/zed/parser/parser_test.go.
func testEmbeddedProjectionZedRewrite(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	firstUpdatedAt := time.Date(2026, time.June, 27, 14, 0, 0, 0, time.UTC)
	writeEmbeddedProjectionZedThread(t, stores, firstUpdatedAt, `{
		"version":"0.3.0",
		"title":"Rewrite thread",
		"updated_at":"2026-06-27T14:00:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"hello"}]}},
			{"Agent":{"content":[{"Text":"answer one"}],"tool_results":{}}}
		]
	}`)
	index := newEmbeddedProjectionIndex()
	kinds := defaultSemanticContentKinds()

	first := loadEmbeddedProjectionConversation(t, index, conversation.ProviderZed, kinds)
	if conversation.TrailingMessageMayGrow(first.record) {
		t.Fatalf("zed record artifact kind %q reports a growing trailing message", first.record.ArtifactKind)
	}
	firstProjection := projectEmbeddedProjectionFields(t, first, kinds, false)
	assertEmbeddedProjectionLayout(t, firstProjection.Fields, []embeddedProjectionFieldShape{
		{messageIndex: 0, kind: searchbackend.FieldKindChat, toolIndex: -1},
		{messageIndex: 1, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})

	// Zed rewrites the stored thread. The agent message at index 1 changes and a
	// new user message follows it.
	secondUpdatedAt := firstUpdatedAt.Add(time.Hour)
	rewriteEmbeddedProjectionZedThread(t, stores, secondUpdatedAt, `{
		"version":"0.3.0",
		"title":"Rewrite thread",
		"updated_at":"2026-06-27T15:00:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"hello"}]}},
			{"Agent":{"content":[{"Text":"answer rewritten"}],"tool_results":{}}},
			{"User":{"id":"user-2","content":[{"Text":"follow up"}]}}
		]
	}`)
	second := loadEmbeddedProjectionConversation(t, index, conversation.ProviderZed, kinds)
	if len(second.messages) != 3 || second.messages[1].Text != "answer rewritten" {
		t.Fatalf("zed messages after the rewrite = %s, want the rewritten agent message at index 1", describeEmbeddedProjectionMessages(second.messages))
	}
	secondProjection := projectEmbeddedProjectionFields(t, second, kinds, false)
	committed := embeddedProjectionFieldAt(t, firstProjection.Fields, 1, searchbackend.FieldKindChat)
	rewritten := embeddedProjectionFieldAt(t, secondProjection.Fields, 1, searchbackend.FieldKindChat)
	if rewritten.Key != committed.Key || rewritten.Digest == committed.Digest {
		t.Fatalf("rewritten field key/digest = %q/%s, want key %q with a new digest", rewritten.Key, rewritten.Digest, committed.Key)
	}
	selection := searchbackend.SelectNewFields(secondProjection.Fields, embeddedProjectionDigests(firstProjection.Fields))
	if selection.ChangedCommitted != 1 || selection.Unchanged != 1 {
		t.Fatalf("selection unchanged/changed = %d/%d, want 1/1", selection.Unchanged, selection.ChangedCommitted)
	}
	if slices.Contains(embeddedProjectionKeys(selection.New), committed.Key) {
		t.Fatalf("selection offered a new field for committed key %q", committed.Key)
	}
	assertEmbeddedProjectionLayout(t, selection.New, []embeddedProjectionFieldShape{
		{messageIndex: 2, kind: searchbackend.FieldKindChat, toolIndex: -1},
	})
}

// embeddedProjectionStores lists the temporary provider roots that one subtest
// writes artifacts into.
type embeddedProjectionStores struct {
	codexHome      string
	cursorProjects string
	zedData        string
}

// isolateEmbeddedProjectionStores sets the home directory, the XDG roots, and
// every registered provider root to temporary directories. A refresh then
// reads only the artifacts the subtest writes.
func isolateEmbeddedProjectionStores(t *testing.T) embeddedProjectionStores {
	t.Helper()
	root := t.TempDir()
	stores := embeddedProjectionStores{
		codexHome:      filepath.Join(root, "codex"),
		cursorProjects: filepath.Join(root, "cursor-projects"),
		zedData:        filepath.Join(root, "zed"),
	}
	homeDir := filepath.Join(root, "home")
	cursorData := filepath.Join(root, "cursor-data")
	copilotHome := filepath.Join(root, "copilot")
	for _, dir := range []string{homeDir, stores.codexHome, stores.cursorProjects, cursorData, stores.zedData, copilotHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CODEX_HOME", stores.codexHome)
	t.Setenv("CODEX_SQLITE_HOME", stores.codexHome)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", stores.cursorProjects)
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", cursorData)
	t.Setenv("CLYDE_ZED_DATA_DIRS", stores.zedData)
	t.Setenv("COPILOT_HOME", copilotHome)
	return stores
}

// newEmbeddedProjectionIndex builds the daemon's index. The index resolves its
// cache path at construction, so callers build it after the environment points
// at the temporary stores.
func newEmbeddedProjectionIndex() *conversation.Index {
	return conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
}

// embeddedProjectionLoad is one conversation record and its loaded messages.
type embeddedProjectionLoad struct {
	record   conversation.Record
	messages []transcript.Message
}

// loadEmbeddedProjectionConversation refreshes the index and loads the only
// record of provider with the load options the semantic feeder derives from
// kinds.
func loadEmbeddedProjectionConversation(
	t *testing.T,
	index *conversation.Index,
	provider conversation.Provider,
	kinds conversation.ContentKindSet,
) embeddedProjectionLoad {
	t.Helper()
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
	stamped, err := index.ListWithStamps(t.Context())
	if err != nil {
		t.Fatalf("list conversation records: %v", err)
	}
	records := make([]conversation.Record, 0, len(stamped))
	for _, stampedRecord := range stamped {
		if stampedRecord.Record.Provider == provider {
			records = append(records, stampedRecord.Record)
		}
	}
	if len(records) != 1 {
		t.Fatalf("%s records = %d, want 1 (all records: %d)", provider.String(), len(records), len(stamped))
	}
	messages, err := index.LoadMessagesWithOptions(records[0], SemanticConversationLoadOptions(kinds))
	if err != nil {
		t.Fatalf("load %s messages: %v", records[0].ID, err)
	}
	return embeddedProjectionLoad{record: records[0], messages: messages}
}

func projectEmbeddedProjectionFields(
	t *testing.T,
	load embeddedProjectionLoad,
	kinds conversation.ContentKindSet,
	artifactSettled bool,
) searchbackend.ProjectedFields {
	t.Helper()
	projected, _, err := projectEmbeddedConversationFields(load.record, load.messages, kinds, artifactSettled)
	if err != nil {
		t.Fatalf("project %s fields: %v", load.record.ID, err)
	}
	return projected
}

// embeddedProjectionFieldShape is the identity of one field: the loaded
// message position, the field kind, and the tool position or -1.
type embeddedProjectionFieldShape struct {
	messageIndex int
	kind         searchbackend.FieldKind
	toolIndex    int
}

// assertEmbeddedProjectionLayout compares field identities in order and checks
// that each key embeds the message index the field reports.
func assertEmbeddedProjectionLayout(t *testing.T, fields []searchbackend.Field, want []embeddedProjectionFieldShape) {
	t.Helper()
	got := make([]embeddedProjectionFieldShape, 0, len(fields))
	for _, field := range fields {
		got = append(got, embeddedProjectionFieldShape{messageIndex: field.MessageIndex, kind: field.Kind, toolIndex: field.ToolIndex})
		keySuffix := fmt.Sprintf("/m%d/%s", field.MessageIndex, field.Kind)
		if field.ToolIndex >= 0 {
			keySuffix += fmt.Sprintf("/t%d", field.ToolIndex)
		}
		if !strings.HasSuffix(field.Key, keySuffix) {
			t.Fatalf("field key %q does not end with %q", field.Key, keySuffix)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fields = %+v, want %+v (keys %q)", got, want, embeddedProjectionKeys(fields))
	}
}

// assertEmbeddedProjectionChatText checks that each chat field text equals the
// text of the loaded message at the field's reported index.
func assertEmbeddedProjectionChatText(t *testing.T, fields []searchbackend.Field, messages []transcript.Message) {
	t.Helper()
	for _, field := range fields {
		if field.Kind != searchbackend.FieldKindChat {
			continue
		}
		if field.MessageIndex < 0 || field.MessageIndex >= len(messages) {
			t.Fatalf("field %q message index %d is outside %d loaded messages", field.Key, field.MessageIndex, len(messages))
		}
		if field.Text != messages[field.MessageIndex].Text {
			t.Fatalf("field %q text = %q, want message %d text %q", field.Key, field.Text, field.MessageIndex, messages[field.MessageIndex].Text)
		}
	}
}

func embeddedProjectionFieldAt(
	t *testing.T,
	fields []searchbackend.Field,
	messageIndex int,
	kind searchbackend.FieldKind,
) searchbackend.Field {
	t.Helper()
	for _, field := range fields {
		if field.MessageIndex == messageIndex && field.Kind == kind {
			return field
		}
	}
	t.Fatalf("no %s field for message %d in %q", kind, messageIndex, embeddedProjectionKeys(fields))
	return searchbackend.Field{}
}

// embeddedProjectionDigests maps each field key to its digest, which is the
// committed state a later ingestion compares against.
func embeddedProjectionDigests(fields []searchbackend.Field) map[string]string {
	digests := make(map[string]string, len(fields))
	for _, field := range fields {
		digests[field.Key] = field.Digest
	}
	return digests
}

func embeddedProjectionKeys(fields []searchbackend.Field) []string {
	keys := make([]string, 0, len(fields))
	for _, field := range fields {
		keys = append(keys, field.Key)
	}
	return keys
}

func describeEmbeddedProjectionMessages(messages []transcript.Message) string {
	parts := make([]string, 0, len(messages))
	for i, message := range messages {
		parts = append(parts, fmt.Sprintf("%d:%s text=%q thinking=%q tools=%d", i, message.Role, message.Text, message.Thinking, len(message.Tools)))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

func writeEmbeddedProjectionCodexRollout(t *testing.T, stores embeddedProjectionStores) string {
	t.Helper()
	dayDir := filepath.Join(stores.codexHome, "sessions", "2026", "05", "02")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("create codex sessions dir: %v", err)
	}
	rolloutPath := filepath.Join(dayDir, "rollout-2026-05-02T10-09-00-"+embeddedProjectionCodexThreadID+".jsonl")
	appendEmbeddedProjectionLines(t, rolloutPath, embeddedProjectionCodexInitialLines)
	return rolloutPath
}

// appendEmbeddedProjectionLines appends newline-terminated JSONL lines and
// creates the file when it does not exist.
func appendEmbeddedProjectionLines(t *testing.T, path string, lines []string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s for append: %v", path, err)
	}
	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		_ = file.Close()
		t.Fatalf("append to %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// openEmbeddedProjectionSQLite opens a SQLite database for fixture writes and
// closes it when the subtest ends.
func openEmbeddedProjectionSQLite(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(dbPath), err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func embeddedProjectionZedThreadsDB(stores embeddedProjectionStores) string {
	return filepath.Join(stores.zedData, "threads", "threads.db")
}

func embeddedProjectionZedSidebarDB(stores embeddedProjectionStores) string {
	return filepath.Join(stores.zedData, "db", "0-stable", "db.sqlite")
}

func writeEmbeddedProjectionZedThread(t *testing.T, stores embeddedProjectionStores, updatedAt time.Time, threadJSON string) {
	t.Helper()
	timestamp := updatedAt.Format(time.RFC3339)
	threadsDB := openEmbeddedProjectionSQLite(t, embeddedProjectionZedThreadsDB(stores))
	if _, err := threadsDB.Exec(`CREATE TABLE IF NOT EXISTS threads(id TEXT PRIMARY KEY,parent_id TEXT,folder_paths TEXT,folder_paths_order TEXT,summary TEXT NOT NULL,updated_at TEXT NOT NULL,data_type TEXT NOT NULL,data BLOB NOT NULL,created_at TEXT NOT NULL) STRICT;`); err != nil {
		t.Fatalf("create zed threads table: %v", err)
	}
	if _, err := threadsDB.Exec(
		`INSERT INTO threads(id,parent_id,folder_paths,folder_paths_order,summary,updated_at,data_type,data,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		embeddedProjectionZedThreadID, "", "/repo", "0", "Summary", timestamp, "json", []byte(threadJSON), timestamp,
	); err != nil {
		t.Fatalf("insert zed thread row: %v", err)
	}
	sidebarDB := openEmbeddedProjectionSQLite(t, embeddedProjectionZedSidebarDB(stores))
	if _, err := sidebarDB.Exec(`CREATE TABLE IF NOT EXISTS sidebar_threads(session_id TEXT PRIMARY KEY,agent_id TEXT,title TEXT NOT NULL,title_override TEXT,updated_at TEXT NOT NULL,created_at TEXT,folder_paths TEXT,folder_paths_order TEXT,archived INTEGER DEFAULT 0,main_worktree_paths TEXT,main_worktree_paths_order TEXT) STRICT;`); err != nil {
		t.Fatalf("create zed sidebar table: %v", err)
	}
	if _, err := sidebarDB.Exec(
		`INSERT INTO sidebar_threads(session_id,agent_id,title,title_override,updated_at,created_at,folder_paths,folder_paths_order,archived,main_worktree_paths,main_worktree_paths_order) VALUES(?,NULL,?,NULL,?,?,?,?,?,?,?)`,
		embeddedProjectionZedThreadID, "Rewrite thread", timestamp, timestamp, "/repo", "0", 0, "/repo", "0",
	); err != nil {
		t.Fatalf("insert zed sidebar row: %v", err)
	}
}

func rewriteEmbeddedProjectionZedThread(t *testing.T, stores embeddedProjectionStores, updatedAt time.Time, threadJSON string) {
	t.Helper()
	timestamp := updatedAt.Format(time.RFC3339)
	threadsDB := openEmbeddedProjectionSQLite(t, embeddedProjectionZedThreadsDB(stores))
	if _, err := threadsDB.Exec(`UPDATE threads SET data = ?, updated_at = ? WHERE id = ?`,
		[]byte(threadJSON), timestamp, embeddedProjectionZedThreadID); err != nil {
		t.Fatalf("rewrite zed thread row: %v", err)
	}
	sidebarDB := openEmbeddedProjectionSQLite(t, embeddedProjectionZedSidebarDB(stores))
	if _, err := sidebarDB.Exec(`UPDATE sidebar_threads SET updated_at = ? WHERE session_id = ?`,
		timestamp, embeddedProjectionZedThreadID); err != nil {
		t.Fatalf("update zed sidebar row: %v", err)
	}
}
