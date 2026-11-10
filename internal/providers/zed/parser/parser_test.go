package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestDiscoverReturnsVirtualCandidateForNativeZedThread(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 12, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{"version":"0.3.0","title":"Thread","messages":[],"updated_at":"2026-06-27T12:00:00Z"}`)

	writeThreadsRow(t, root, "thread-1", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-1", "", "Thread title", "", updatedAt)

	candidates, err := New().Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	if candidates[0].Path != "zed://"+RootHash(root)+"/0-stable/thread-1" {
		t.Fatalf("candidate path = %q, want legacy 3-segment native path", candidates[0].Path)
	}

	parsed, err := ParseVirtualPath(candidates[0].Path)
	if err != nil {
		t.Fatalf("ParseVirtualPath returned error: %v", err)
	}
	if parsed.Channel != "0-stable" || parsed.SessionID != "thread-1" {
		t.Fatalf("parsed = %#v", parsed)
	}
	if candidates[0].Stamp.Size != int64(len(threadJSON)) || !candidates[0].Stamp.Mtime.Equal(updatedAt) {
		t.Fatalf("stamp = %#v", candidates[0].Stamp)
	}
}

func TestDiscoverSkipsExternalAgentMetadataWithoutNativeThreadRow(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 12, 30, 0, 0, time.UTC)

	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "external-1", "claude", "External", "", updatedAt)

	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates len = %d, want 0", len(candidates))
	}
}

func TestScanRecordUsesDiscoveredThreadMetadataAndCurrentThreadJSON(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	rowUpdatedAt := time.Date(2026, time.June, 27, 12, 0, 0, 0, time.UTC)
	metadataUpdatedAt := time.Date(2026, time.June, 27, 12, 5, 0, 0, time.UTC)
	metadataCreatedAt := time.Date(2026, time.June, 26, 8, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title from payload",
		"updated_at":"2026-06-27T12:00:00Z",
		"model":{"provider":"anthropic","model":"claude-sonnet"},
		"subagent_context":{"parent_thread_id":"parent-thread","depth":1},
		"messages":[]
	}`)

	writeThreadsRow(t, root, "thread-1", "", rowUpdatedAt, threadJSON)
	writeSidebarRowWithOptions(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), sidebarRowOptions{
		SessionID:              "thread-1",
		Title:                  "Metadata title",
		TitleOverride:          "User title",
		UpdatedAt:              metadataUpdatedAt,
		CreatedAt:              metadataCreatedAt,
		FolderPaths:            "/repo/a\n/repo/b",
		FolderPathsOrder:       "1,0",
		Archived:               true,
		MainWorktreePaths:      "/repo/a\n/repo/b",
		MainWorktreePathsOrder: "1,0",
	})

	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}

	record, ok := p.ScanRecord(candidates[0].Path, candidates[0].Stamp)
	if !ok {
		t.Fatal("ScanRecord returned ok=false")
	}
	if record.ID != conversation.DerivedID(conversation.ProviderZed, "thread-1", candidates[0].Path) {
		t.Fatalf("record.ID = %q", record.ID)
	}
	if record.Provider != conversation.ProviderZed || record.NativeID != "thread-1" {
		t.Fatalf("record = %#v", record)
	}
	if record.Title != "User title" {
		t.Fatalf("record.Title = %q, want user override", record.Title)
	}
	if record.WorkspaceRoot != "/repo/b" {
		t.Fatalf("record.WorkspaceRoot = %q, want ordered first folder path", record.WorkspaceRoot)
	}
	if record.ArtifactKind != "zed_thread" {
		t.Fatalf("record.ArtifactKind = %q, want zed_thread", record.ArtifactKind)
	}
	if record.Model != "anthropic/claude-sonnet" {
		t.Fatalf("record.Model = %q, want anthropic/claude-sonnet", record.Model)
	}
	if !record.CreatedAt.Equal(metadataCreatedAt) || !record.UpdatedAt.Equal(metadataUpdatedAt) {
		t.Fatalf("created or updated times = %v / %v", record.CreatedAt, record.UpdatedAt)
	}
	if !record.Archived {
		t.Fatal("record.Archived = false, want true")
	}
	if record.Lineage == nil || record.Lineage.Kind != conversation.ConversationLineageKindSpawn || record.Lineage.ParentNativeID != "parent-thread" {
		t.Fatalf("record.Lineage = %#v", record.Lineage)
	}
}

func TestFreshParserReloadsDiscoveredVirtualPathForScanAndStream(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	updatedAt := time.Date(2026, time.June, 27, 14, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Reloaded thread",
		"updated_at":"2026-06-27T14:00:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"hello"}]}},
			{"Agent":{"content":[{"Text":"answer"}],"tool_results":{}}}
		]
	}`)

	writeThreadsRowWithDataType(t, root, "thread-reload", "", updatedAt, "json", threadJSON)
	writeSidebarRowWithOptions(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), sidebarRowOptions{
		SessionID:              "thread-reload",
		Title:                  "Reload metadata title",
		UpdatedAt:              updatedAt,
		CreatedAt:              updatedAt,
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		Archived:               false,
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
	})

	discoverParser := New()
	candidates, err := discoverParser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}

	scanParser := New()
	record, ok := scanParser.ScanRecord(candidates[0].Path, candidates[0].Stamp)
	if !ok {
		t.Fatal("ScanRecord returned ok=false")
	}
	if record.NativeID != "thread-reload" {
		t.Fatalf("record.NativeID = %q, want thread-reload", record.NativeID)
	}
	if record.Title != "Reload metadata title" {
		t.Fatalf("record.Title = %q, want reload metadata title", record.Title)
	}

	streamParser := New()
	messages, err := conversation.CollectMessages(streamParser.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Text != "hello" {
		t.Fatalf("first message = %#v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Text != "answer" {
		t.Fatalf("second message = %#v", messages[1])
	}
}

func TestScanRecordReturnsFalseForUnknownVirtualPath(t *testing.T) {
	t.Parallel()

	record, ok := New().ScanRecord("zed://deadbeef/0-stable/thread-1", conversation.FileStamp{})
	if ok {
		t.Fatalf("ScanRecord returned ok=true with record %#v, want false", record)
	}
}

func TestStreamShapesVisibleZedMessagesWithoutCompactionByDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 13, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T13:00:00Z",
		"messages":[
			{"User":{"id":"user-empty","content":[{"Text":"   "}]}},
			{"User":{"id":"user-1","content":[{"Text":"hello"},{"Mention":{"uri":"file:///repo/readme","content":"README excerpt"}}]}},
			{"Agent":{"content":[{"Text":"   "},{"Thinking":{"text":"   ","signature":"sig"}}],"tool_results":{}}},
			{"Agent":{"content":[{"Text":"answer"},{"Thinking":{"text":"reasoning","signature":"sig"}},{"ToolUse":{"id":"call-1","name":"Read","input":{"path":"/repo/readme"}}}],"tool_results":{}}},
			"Resume",
			{"Compaction":{"Summary":"Earlier summary"}}
		]
	}`)

	writeThreadsRow(t, root, "thread-1", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-1", "", "Thread title", "", updatedAt)

	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("Discover returned no candidates")
	}
	messages, err := conversation.CollectMessages(p.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Text != "hello\nREADME excerpt" {
		t.Fatalf("user message = %#v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Text != "answer" || messages[1].Thinking != "reasoning" {
		t.Fatalf("assistant message = %#v", messages[1])
	}
	if !messages[1].HasTools || len(messages[1].Tools) != 1 || messages[1].Tools[0].Name != "Read" {
		t.Fatalf("assistant tools = %#v", messages[1].Tools)
	}
	if messages[1].Tools[0].Output != "" || messages[1].Tools[0].IsError {
		t.Fatalf("assistant tool output = %#v, want empty output without IncludeToolOutputs", messages[1].Tools[0])
	}
	if messages[2].Role != "user" || messages[2].Text != "Continue where you left off" {
		t.Fatalf("resume message = %#v", messages[2])
	}
}

func TestStreamIncludesZedCompactionMessagesWhenRequested(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 13, 30, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T13:30:00Z",
		"messages":[
			{"Compaction":{"Summary":"Earlier summary"}},
			{"Compaction":{"ProviderNative":{"provider":"anthropic","items":[{"type":"thinking"}]}}}
		]
	}`)

	writeThreadsRow(t, root, "thread-2", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-preview", "db.sqlite"), "thread-2", "", "Thread title", "", updatedAt)

	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	messages, err := conversation.CollectMessages(p.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Compaction == nil || messages[0].Compaction.Kind != transcript.CompactionKindSummary || messages[0].Text != "Earlier summary" {
		t.Fatalf("summary compaction = %#v", messages[0])
	}
	if messages[1].Role != "system" || messages[1].Compaction == nil || messages[1].Compaction.Kind != transcript.CompactionKindBoundary || messages[1].Text != "anthropic compaction boundary" {
		t.Fatalf("provider-native compaction = %#v", messages[1])
	}
}

func TestStreamAttachesZedToolResultsWhenRequested(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 14, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T14:00:00Z",
		"messages":[
			{"Agent":{
				"content":[{"ToolUse":{"id":"call-1","name":"Read","input":{"path":"/repo/readme"}}}],
				"tool_results":{
					"call-1":{"tool_use_id":"call-1","is_error":true,"content":["tool output","more output"],"output":{"ok":false}}
				}
			}}
		]
	}`)

	writeThreadsRow(t, root, "thread-3", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-3", "", "Thread title", "", updatedAt)

	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	messages, err := conversation.CollectMessages(p.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 1 || len(messages[0].Tools) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Tools[0].Output != "tool output\nmore output\n{\"ok\":false}" || !messages[0].Tools[0].IsError {
		t.Fatalf("tool output = %#v", messages[0].Tools[0])
	}
}

func TestStreamFallsBackToZedToolResultOutputFieldWhenContentIsEmpty(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 14, 15, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T14:15:00Z",
		"messages":[
			{"Agent":{
				"content":[{"ToolUse":{"id":"call-1","name":"Read","input":{"path":"/repo/readme"}}}],
				"tool_results":{
					"call-1":{"tool_use_id":"call-1","is_error":false,"content":[],"output":"tool output"}
				}
			}}
		]
	}`)

	writeThreadsRow(t, root, "thread-4", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-4", "", "Thread title", "", updatedAt)

	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	messages, err := conversation.CollectMessages(p.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 1 || len(messages[0].Tools) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Tools[0].Output != "tool output" || messages[0].Tools[0].IsError {
		t.Fatalf("tool output fallback = %#v", messages[0].Tools[0])
	}
}

func TestStreamCanReloadDiscoveredVirtualPathWithoutCachedDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 14, 30, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T14:30:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"hello again"}]}}
		]
	}`)

	writeThreadsRow(t, root, "thread-4", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-4", "", "Thread title", "", updatedAt)

	firstParser := New()
	candidates, err := firstParser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	freshParser := New()
	messages, err := conversation.CollectMessages(freshParser.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Text != "hello again" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestStreamDecodesZstdZedThreadPayloads(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 15, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T15:00:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"compressed hello"}]}}
		]
	}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter returned error: %v", err)
	}
	compressed := encoder.EncodeAll(threadJSON, nil)
	if closeErr := encoder.Close(); closeErr != nil {
		t.Fatalf("zstd encoder close: %v", closeErr)
	}

	writeThreadsRowWithDataType(t, root, "thread-zstd", "", updatedAt, "zstd", compressed)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-zstd", "", "Thread title", "", updatedAt)

	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	messages, err := conversation.CollectMessages(p.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Text != "compressed hello" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestDiscoverIncludesTerminalMetadataWithoutTranscriptRow(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 15, 15, 0, 0, time.UTC)
	dbPath := filepath.Join(root, "db", "0-stable", "db.sqlite")
	writeTerminalSidebarRow(t, dbPath, terminalSidebarRowOptions{
		TerminalID:             "terminal-1",
		Title:                  "Terminal thread",
		CustomTitle:            "Build output",
		CreatedAt:              updatedAt,
		WorkingDirectory:       "/repo",
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
		RemoteConnection:       "",
	})

	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}

	record, ok := parser.ScanRecord(candidates[0].Path, candidates[0].Stamp)
	if !ok {
		t.Fatal("ScanRecord returned ok=false")
	}
	if record.Provider != conversation.ProviderZed {
		t.Fatalf("record.Provider = %v, want zed", record.Provider)
	}
	if record.ArtifactKind != "zed_terminal_thread" {
		t.Fatalf("record.ArtifactKind = %q, want zed_terminal_thread", record.ArtifactKind)
	}
	if record.ID != "zed:terminal-terminal-1" {
		t.Fatalf("record.ID = %q, want zed:terminal-terminal-1", record.ID)
	}
	if record.SizeBytes != 0 {
		t.Fatalf("record.SizeBytes = %d, want 0 for metadata-only terminal records", record.SizeBytes)
	}

	messages, err := conversation.CollectMessages(parser.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	if messages[0].Visibility != transcript.MessageVisibilityMetaOnly {
		t.Fatalf("messages[0].Visibility = %q, want meta_only", messages[0].Visibility)
	}
	if messages[0].Text == "" {
		t.Fatalf("messages[0].Text empty, want explicit unavailable reason")
	}
}

func TestTerminalMetadataIsHiddenWhenSystemMessagesAreExcluded(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	createdAt := time.Date(2026, time.June, 27, 15, 15, 0, 0, time.UTC)
	dbPath := filepath.Join(root, "db", "0-stable", "db.sqlite")
	writeTerminalSidebarRow(t, dbPath, terminalSidebarRowOptions{
		TerminalID:             "terminal-1",
		Title:                  "Terminal thread",
		CustomTitle:            "",
		CreatedAt:              createdAt,
		WorkingDirectory:       "/repo",
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
		RemoteConnection:       "",
	})

	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}

	messages, err := conversation.CollectMessages(parser.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages len = %d, want 0 when system messages are excluded", len(messages))
	}
}

func TestTerminalMetadataStampChangesWhenTitleChangesButLengthDoesNot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	createdAt := time.Date(2026, time.June, 27, 15, 15, 0, 0, time.UTC)
	dbPath := filepath.Join(root, "db", "0-stable", "db.sqlite")

	writeTerminalSidebarRow(t, dbPath, terminalSidebarRowOptions{
		TerminalID:             "terminal-1",
		Title:                  "Alpha",
		CustomTitle:            "",
		CreatedAt:              createdAt,
		WorkingDirectory:       "/repo",
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
		RemoteConnection:       "",
	})

	parser := New()
	firstCandidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("first Discover returned error: %v", err)
	}
	if len(firstCandidates) != 1 {
		t.Fatalf("first candidates len = %d, want 1", len(firstCandidates))
	}

	updateTerminalSidebarRowTitle(t, dbPath, "terminal-1", "Bravo")

	secondCandidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("second Discover returned error: %v", err)
	}
	if len(secondCandidates) != 1 {
		t.Fatalf("second candidates len = %d, want 1", len(secondCandidates))
	}
	if firstCandidates[0].Stamp.Equal(secondCandidates[0].Stamp) {
		t.Fatalf("terminal metadata stamp did not change after same-length title update: %#v", secondCandidates[0].Stamp)
	}
}

func TestTerminalMetadataStampChangesWhenFolderPathChangesButLengthDoesNot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	createdAt := time.Date(2026, time.June, 27, 15, 15, 0, 0, time.UTC)
	dbPath := filepath.Join(root, "db", "0-stable", "db.sqlite")

	writeTerminalSidebarRow(t, dbPath, terminalSidebarRowOptions{
		TerminalID:             "terminal-1",
		Title:                  "Terminal thread",
		CustomTitle:            "",
		CreatedAt:              createdAt,
		WorkingDirectory:       "/repo",
		FolderPaths:            "/repo/a",
		FolderPathsOrder:       "0",
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
		RemoteConnection:       "",
	})

	parser := New()
	firstCandidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("first Discover returned error: %v", err)
	}
	if len(firstCandidates) != 1 {
		t.Fatalf("first candidates len = %d, want 1", len(firstCandidates))
	}

	updateTerminalSidebarRowFolderPath(t, dbPath, "terminal-1", "/repo/b")

	secondCandidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("second Discover returned error: %v", err)
	}
	if len(secondCandidates) != 1 {
		t.Fatalf("second candidates len = %d, want 1", len(secondCandidates))
	}
	if firstCandidates[0].Stamp.Equal(secondCandidates[0].Stamp) {
		t.Fatalf("terminal metadata stamp did not change after same-length folder path update: %#v", secondCandidates[0].Stamp)
	}
}

func writeThreadsRow(t *testing.T, root, sessionID, parentID string, updatedAt time.Time, data []byte) {
	t.Helper()
	writeThreadsRowWithDataType(t, root, sessionID, parentID, updatedAt, "json", data)
}

func writeThreadsRowWithDataType(t *testing.T, root, sessionID, parentID string, updatedAt time.Time, dataType string, data []byte) {
	t.Helper()
	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open threads db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS threads(id TEXT PRIMARY KEY,parent_id TEXT,folder_paths TEXT,folder_paths_order TEXT,summary TEXT NOT NULL,updated_at TEXT NOT NULL,data_type TEXT NOT NULL,data BLOB NOT NULL,created_at TEXT NOT NULL) STRICT;`); err != nil {
		t.Fatalf("create threads table: %v", err)
	}
	ts := updatedAt.Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO threads(id,parent_id,folder_paths,folder_paths_order,summary,updated_at,data_type,data,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, sessionID, parentID, "/repo", "0", "Summary", ts, dataType, data, ts); err != nil {
		t.Fatalf("insert threads row: %v", err)
	}
}

func writeSidebarRow(t *testing.T, dbPath, sessionID, agentID, title, titleOverride string, updatedAt time.Time) {
	t.Helper()
	writeSidebarRowWithOptions(t, dbPath, sidebarRowOptions{
		SessionID:              sessionID,
		AgentID:                agentID,
		Title:                  title,
		TitleOverride:          titleOverride,
		UpdatedAt:              updatedAt,
		CreatedAt:              updatedAt,
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		Archived:               false,
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
	})
}

type sidebarRowOptions struct {
	SessionID              string
	AgentID                string
	Title                  string
	TitleOverride          string
	UpdatedAt              time.Time
	CreatedAt              time.Time
	FolderPaths            string
	FolderPathsOrder       string
	Archived               bool
	MainWorktreePaths      string
	MainWorktreePathsOrder string
}

func writeSidebarRowWithOptions(t *testing.T, dbPath string, options sidebarRowOptions) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sidebar_threads(session_id TEXT PRIMARY KEY,agent_id TEXT,title TEXT NOT NULL,title_override TEXT,updated_at TEXT NOT NULL,created_at TEXT,folder_paths TEXT,folder_paths_order TEXT,archived INTEGER DEFAULT 0,main_worktree_paths TEXT,main_worktree_paths_order TEXT) STRICT;`); err != nil {
		t.Fatalf("create sidebar table: %v", err)
	}
	updatedAt := options.UpdatedAt.Format(time.RFC3339)
	createdAt := nullableTime(options.CreatedAt)
	archivedValue := 0
	if options.Archived {
		archivedValue = 1
	}
	if _, err := db.Exec(`INSERT INTO sidebar_threads(session_id,agent_id,title,title_override,updated_at,created_at,folder_paths,folder_paths_order,archived,main_worktree_paths,main_worktree_paths_order) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, options.SessionID, nullable(options.AgentID), options.Title, nullable(options.TitleOverride), updatedAt, createdAt, options.FolderPaths, options.FolderPathsOrder, archivedValue, options.MainWorktreePaths, options.MainWorktreePathsOrder); err != nil {
		t.Fatalf("insert sidebar row: %v", err)
	}
}

type terminalSidebarRowOptions struct {
	TerminalID             string
	Title                  string
	CustomTitle            string
	CreatedAt              time.Time
	WorkingDirectory       string
	FolderPaths            string
	FolderPathsOrder       string
	MainWorktreePaths      string
	MainWorktreePathsOrder string
	RemoteConnection       string
}

func writeTerminalSidebarRow(t *testing.T, dbPath string, options terminalSidebarRowOptions) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sidebar_terminal_threads(terminal_id TEXT PRIMARY KEY,title TEXT NOT NULL,custom_title TEXT,created_at TEXT NOT NULL,working_directory TEXT,folder_paths TEXT,folder_paths_order TEXT,main_worktree_paths TEXT,main_worktree_paths_order TEXT,remote_connection TEXT) STRICT;`); err != nil {
		t.Fatalf("create terminal sidebar table: %v", err)
	}
	createdAt := options.CreatedAt.Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO sidebar_terminal_threads(terminal_id,title,custom_title,created_at,working_directory,folder_paths,folder_paths_order,main_worktree_paths,main_worktree_paths_order,remote_connection) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		options.TerminalID,
		options.Title,
		nullable(options.CustomTitle),
		createdAt,
		nullable(options.WorkingDirectory),
		nullable(options.FolderPaths),
		nullable(options.FolderPathsOrder),
		nullable(options.MainWorktreePaths),
		nullable(options.MainWorktreePathsOrder),
		nullable(options.RemoteConnection),
	); err != nil {
		t.Fatalf("insert terminal sidebar row: %v", err)
	}
}

func updateTerminalSidebarRowTitle(t *testing.T, dbPath string, terminalID string, title string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE sidebar_terminal_threads SET title = ? WHERE terminal_id = ?`, title, terminalID); err != nil {
		t.Fatalf("update terminal sidebar row: %v", err)
	}
}

func updateTerminalSidebarRowFolderPath(t *testing.T, dbPath string, terminalID string, folderPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE sidebar_terminal_threads SET folder_paths = ? WHERE terminal_id = ?`, folderPath, terminalID); err != nil {
		t.Fatalf("update terminal sidebar folder path: %v", err)
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.Format(time.RFC3339)
}
