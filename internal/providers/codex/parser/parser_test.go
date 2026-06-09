package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

func TestStreamShapesCodexConversationMessages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[Image #1]\n\nshow me the exact diff"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.500Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"answer text","phase":"commentary"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect codex messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	// ConversationOnly strips the leading image placeholder line.
	if messages[0].Text != "show me the exact diff" {
		t.Fatalf("messages[0].Text = %q, want %q", messages[0].Text, "show me the exact diff")
	}
	if messages[0].Role != "user" {
		t.Fatalf("messages[0].Role = %q, want user", messages[0].Role)
	}
	if messages[1].Text != "answer text" {
		t.Fatalf("messages[1].Text = %q, want %q", messages[1].Text, "answer text")
	}
}

func TestStreamIncludesCodexCompactionBoundariesWhenRequested(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.500Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"answer text","phase":"commentary"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect codex messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	compactionMessages := transcript.CompactionMessages(messages)
	if len(compactionMessages) != 1 {
		t.Fatalf("compaction messages len = %d, want 1", len(compactionMessages))
	}
	if compactionMessages[0].Compaction == nil || compactionMessages[0].Compaction.Kind != transcript.CompactionKindBoundary {
		t.Fatalf("compaction = %#v", compactionMessages[0].Compaction)
	}
	if compactionMessages[0].Compaction.ReplacementHistoryCount != 2 {
		t.Fatalf("replacement history count = %d, want 2", compactionMessages[0].Compaction.ReplacementHistoryCount)
	}
	if len(compactionMessages[0].Compaction.ContextItems) != 2 {
		t.Fatalf("context items len = %d, want 2", len(compactionMessages[0].Compaction.ContextItems))
	}
	firstItem := compactionMessages[0].Compaction.ContextItems[0]
	secondItem := compactionMessages[0].Compaction.ContextItems[1]
	if firstItem.Kind != transcript.CompactedContextItemKindMessage ||
		firstItem.Message == nil {
		t.Fatalf("first context item = %#v", firstItem)
	}
	if secondItem.Kind != transcript.CompactedContextItemKindMessage ||
		secondItem.Message == nil {
		t.Fatalf("second context item = %#v", secondItem)
	}
	if firstItem.Message.Role != "user" || secondItem.Message.Role != "assistant" {
		t.Fatalf("message roles = %q/%q", firstItem.Message.Role, secondItem.Message.Role)
	}
	if len(firstItem.Message.Raw) == 0 || len(secondItem.Message.Raw) == 0 {
		t.Fatalf("message raw values were empty")
	}
}

// TestDiscoverAndScanResolvesDurableNameAndLatestWorkdir exercises the full
// Discover then ScanRecord flow against a populated store, asserting the durable
// thread name comes from the session index, a pre-header work dir wins over the
// initial cwd, and the archived rollout is flagged.
func TestDiscoverAndScanResolvesDurableNameAndLatestWorkdir(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)

	paths, err := codexstore.ResolveStorePaths(t.Context(), codexHome, "")
	if err != nil {
		t.Fatalf("resolve store paths: %v", err)
	}
	activeDir := filepath.Join(paths.SessionsDir, "2026", "05", "02")
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		t.Fatalf("mkdir active: %v", err)
	}
	if err := os.MkdirAll(paths.ArchivedSessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir archived: %v", err)
	}
	activeID := "019de9aa-3a00-7010-bd9f-a6ee71559357"
	activePath := filepath.Join(activeDir, "rollout-2026-05-02T10-09-00-"+activeID+".jsonl")
	activeBody := `{"timestamp":"2026-05-02T17:09:03.000Z","type":"turn_context","payload":{"cwd":"/repo/subdir"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"` + activeID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(activePath, []byte(activeBody), 0o600); err != nil {
		t.Fatalf("write active rollout: %v", err)
	}
	archivedID := "019de9bb-3a00-7010-bd9f-a6ee71559357"
	archivedPath := filepath.Join(paths.ArchivedSessionsDir, "rollout-2026-05-02T11-00-00-"+archivedID+".jsonl")
	archivedBody := `{"timestamp":"2026-05-02T18:00:00.000Z","type":"session_meta","payload":{"id":"` + archivedID + `","timestamp":"2026-05-02T18:00:00.000Z","cwd":"/old","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(archivedPath, []byte(archivedBody), 0o600); err != nil {
		t.Fatalf("write archived rollout: %v", err)
	}
	indexBody := `{"id":"` + activeID + `","thread_name":"visible name","updated_at":"2026-05-02T18:00:00Z"}` + "\n"
	if err := os.WriteFile(paths.SessionIndexPath, []byte(indexBody), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("Discover returned %d candidates, want 2", len(candidates))
	}

	var active, archived conversation.Record
	for _, candidate := range candidates {
		record, ok := p.ScanRecord(candidate.Path, candidate.Stamp)
		if !ok {
			t.Fatalf("ScanRecord(%q) returned ok=false", candidate.Path)
		}
		switch record.NativeID {
		case activeID:
			active = record
		case archivedID:
			archived = record
		}
	}
	if active.Title != "visible name" {
		t.Fatalf("active title = %q, want durable session-index name", active.Title)
	}
	if active.WorkspaceRoot != "/repo/subdir" {
		t.Fatalf("active workspace root = %q, want latest workdir /repo/subdir", active.WorkspaceRoot)
	}
	if !archived.Archived {
		t.Fatalf("archived record Archived = false, want true")
	}
}

func TestScanRecordReadsCodexHeader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	threadID := "019de9aa-3a00-7010-bd9f-a6ee71559357"
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-"+threadID+".jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"` + threadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat rollout: %v", err)
	}
	stampMtime := time.Date(2026, time.May, 2, 18, 30, 0, 0, time.UTC)

	record, ok := New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: stampMtime})
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	if record.NativeID != threadID {
		t.Fatalf("native id = %q, want %q", record.NativeID, threadID)
	}
	if record.Provider != conversation.ProviderCodex {
		t.Fatalf("provider = %v, want codex", record.Provider)
	}
	if record.Model != "openai" {
		t.Fatalf("model = %q, want openai", record.Model)
	}
	// No durable session-index name is present, so the title falls back to the
	// thread id.
	if record.Title != threadID {
		t.Fatalf("title = %q, want fallback thread id", record.Title)
	}
	if record.ArtifactKind != "rollout" {
		t.Fatalf("artifact kind = %q, want rollout", record.ArtifactKind)
	}
	if !record.UpdatedAt.Equal(stampMtime) {
		t.Fatalf("UpdatedAt = %v, want %v", record.UpdatedAt, stampMtime)
	}
	if record.Lineage != nil {
		t.Fatalf("lineage = %#v, want nil", record.Lineage)
	}
}

func TestScanRecordPopulatesCodexForkLineageFromSessionMeta(t *testing.T) {
	t.Parallel()
	threadID := "019de9aa-3a00-7010-bd9f-a6ee71559357"
	parentID := "019de9aa-parent-thread"
	payload := `{"id":"` + threadID + `","forked_from_id":"` + parentID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}`

	record, ok := scanRecordFromCodexSessionMetaPayload(t, threadID, payload)
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	if record.Lineage == nil {
		t.Fatalf("lineage = nil, want fork lineage")
	}
	if record.Lineage.Kind != conversation.ConversationLineageKindFork {
		t.Fatalf("lineage kind = %q, want fork", record.Lineage.Kind)
	}
	if record.Lineage.ParentProvider != conversation.ProviderCodex {
		t.Fatalf("parent provider = %v, want Codex", record.Lineage.ParentProvider)
	}
	if record.Lineage.ParentNativeID != parentID {
		t.Fatalf("parent native id = %q, want %q", record.Lineage.ParentNativeID, parentID)
	}
	if record.Lineage.ParentMessageUUID != "" {
		t.Fatalf("parent message uuid = %q, want empty", record.Lineage.ParentMessageUUID)
	}
}

func TestScanRecordIndexesCodexSpawnedSubagentWithLineage(t *testing.T) {
	t.Parallel()
	threadID := "019de9bb-3a00-7010-bd9f-a6ee71559357"
	parentID := "019de9aa-spawn-parent"
	payload := `{"id":"` + threadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `","agent_nickname":"helper","agent_role":"analysis"}}},"model_provider":"openai"}`

	record, ok := scanRecordFromCodexSessionMetaPayload(t, threadID, payload)
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	if record.Lineage == nil {
		t.Fatalf("lineage = nil, want spawn lineage")
	}
	if record.Lineage.Kind != conversation.ConversationLineageKindSpawn {
		t.Fatalf("lineage kind = %q, want spawn", record.Lineage.Kind)
	}
	if record.Lineage.ParentProvider != conversation.ProviderCodex {
		t.Fatalf("parent provider = %v, want Codex", record.Lineage.ParentProvider)
	}
	if record.Lineage.ParentNativeID != parentID {
		t.Fatalf("parent native id = %q, want %q", record.Lineage.ParentNativeID, parentID)
	}
	if record.Lineage.ParentMessageUUID != "" {
		t.Fatalf("parent message uuid = %q, want empty", record.Lineage.ParentMessageUUID)
	}
}

func TestScanRecordSkipsCodexParentlessSubagentMachinery(t *testing.T) {
	t.Parallel()
	threadID := "019de9cc-3a00-7010-bd9f-a6ee71559357"
	payload := `{"id":"` + threadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":{"subagent":{"review":true}},"agent_role":"review","model_provider":"openai"}`

	_, ok := scanRecordFromCodexSessionMetaPayload(t, threadID, payload)
	if ok {
		t.Fatalf("ScanRecord returned ok=true, want false")
	}
}

func scanRecordFromCodexSessionMetaPayload(
	t *testing.T,
	threadID string,
	sessionMetaPayload string,
) (conversation.Record, bool) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-"+threadID+".jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":` + sessionMetaPayload + `}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat rollout: %v", err)
	}
	return New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
}

func TestStreamLiveCodexCompactionSmoke(t *testing.T) {
	t.Parallel()
	const path = "/Users/agoodkind/.codex/sessions/2026/04/27/rollout-2026-04-27T15-49-07-019dd121-d1ff-7ad3-862d-79b33d5fa300.jsonl"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("live Codex rollout unavailable: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect live Codex rollout: %v", err)
	}

	var boundaryCount int
	for _, message := range transcript.CompactionMessages(messages) {
		if message.Compaction == nil {
			continue
		}
		if message.Compaction.Kind == transcript.CompactionKindBoundary {
			boundaryCount++
		}
	}
	if boundaryCount != 1 {
		t.Fatalf("boundary count = %d, want 1", boundaryCount)
	}
}
