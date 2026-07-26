package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
)

// The fixtures below reproduce record shapes taken from real Claude Code
// transcripts: the key sets, value types, and enum values are the ones Claude
// writes, with the payload text replaced by short stand-ins.

func TestDecodeTranscriptEntryReadsShellToolUseResult(t *testing.T) {
	t.Parallel()
	line := []byte(`{"parentUuid":"parent-1","userType":"external","cwd":"/repo",` +
		`"sessionId":"sess-1","version":"2.1.220","gitBranch":"main","slug":"sleepy-feather",` +
		`"entrypoint":"cli","type":"user","uuid":"entry-1","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"ok"}]},` +
		`"toolUseResult":{"stdout":"ok\n","stderr":"","interrupted":false,"isImage":false,` +
		`"noOutputExpected":false,"backgroundTaskId":"bg-1"}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Kind != ToolUseResultKindDetail {
		t.Fatalf("kind = %q, want %q", entry.ToolUseResult.Kind, ToolUseResultKindDetail)
	}
	if entry.ToolUseResult.Detail == nil {
		t.Fatalf("detail = nil, want the decoded shell result")
	}
	if entry.ToolUseResult.Detail.Stdout != "ok\n" {
		t.Fatalf("stdout = %q, want %q", entry.ToolUseResult.Detail.Stdout, "ok\n")
	}
	if entry.ToolUseResult.Detail.BackgroundTaskID != "bg-1" {
		t.Fatalf("background task id = %q, want bg-1", entry.ToolUseResult.Detail.BackgroundTaskID)
	}
	if entry.Slug != "sleepy-feather" || entry.GitBranch != "main" || entry.Version != "2.1.220" {
		t.Fatalf("session fields = %q/%q/%q, want sleepy-feather/main/2.1.220", entry.Slug, entry.GitBranch, entry.Version)
	}
	if entry.Entrypoint != EntrypointCLI {
		t.Fatalf("entrypoint = %q, want %q", entry.Entrypoint, EntrypointCLI)
	}
}

func TestDecodeTranscriptEntryReadsTextToolUseResult(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-2","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"toolUseResult":"Error: Exit code 2\nbuild failed"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Kind != ToolUseResultKindText {
		t.Fatalf("kind = %q, want %q", entry.ToolUseResult.Kind, ToolUseResultKindText)
	}
	if entry.ToolUseResult.Text != "Error: Exit code 2\nbuild failed" {
		t.Fatalf("text = %q, want the error string", entry.ToolUseResult.Text)
	}
	if entry.ToolUseResult.Detail != nil {
		t.Fatalf("detail = %#v, want nil for the text variant", entry.ToolUseResult.Detail)
	}
}

func TestDecodeTranscriptEntryReadsBlockToolUseResult(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-3","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"toolUseResult":[{"type":"text","text":"Ready to search"},` +
		`{"type":"resource_link","name":"index","uri":"file:///repo","mimeType":"text/plain",` +
		`"description":"repo index","server":"semantic"}]}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Kind != ToolUseResultKindBlocks {
		t.Fatalf("kind = %q, want %q", entry.ToolUseResult.Kind, ToolUseResultKindBlocks)
	}
	if len(entry.ToolUseResult.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(entry.ToolUseResult.Blocks))
	}
	if entry.ToolUseResult.Blocks[0].Text != "Ready to search" {
		t.Fatalf("first block text = %q, want %q", entry.ToolUseResult.Blocks[0].Text, "Ready to search")
	}
	if entry.ToolUseResult.Blocks[1].Server != "semantic" || entry.ToolUseResult.Blocks[1].URI != "file:///repo" {
		t.Fatalf("resource link = %+v, want the semantic file:///repo link", entry.ToolUseResult.Blocks[1])
	}
}

// TestDecodeTranscriptEntryKeepsUnsupportedToolUseResult pins the deliberate
// choice for a result shape the parser does not model: the entry still
// decodes, the kind says the shape is unsupported, and the original value is
// kept so nothing is silently dropped.
func TestDecodeTranscriptEntryKeepsUnsupportedToolUseResult(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-4","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"requestId":"req_1","toolUseResult":42}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Kind != ToolUseResultKindUnsupported {
		t.Fatalf("kind = %q, want %q", entry.ToolUseResult.Kind, ToolUseResultKindUnsupported)
	}
	if string(entry.ToolUseResult.Raw) != "42" {
		t.Fatalf("raw = %q, want the original 42", string(entry.ToolUseResult.Raw))
	}
	if entry.ToolUseResult.Detail != nil || entry.ToolUseResult.Text != "" || entry.ToolUseResult.Blocks != nil {
		t.Fatalf("unsupported result filled a variant: %+v", entry.ToolUseResult)
	}
	if entry.UUID != "entry-4" || entry.RequestID != "req_1" {
		t.Fatalf("uuid/requestId = %q/%q, want entry-4/req_1: the rest of the record must survive", entry.UUID, entry.RequestID)
	}
}

func TestDecodeTranscriptEntryReadsContentBothWays(t *testing.T) {
	t.Parallel()
	written := []byte(`{"type":"user","uuid":"entry-5","toolUseResult":{"type":"create",` +
		`"filePath":"/repo/main.go","content":"package main","structuredPatch":[]}}`)
	blocks := []byte(`{"type":"user","uuid":"entry-6","toolUseResult":{"content":` +
		`[{"type":"text","text":"agent findings"}]}}`)

	writtenEntry, err := DecodeTranscriptEntry(written)
	if err != nil {
		t.Fatalf("decode written-file entry: %v", err)
	}
	if writtenEntry.ToolUseResult.Detail == nil {
		t.Fatalf("detail = nil, want the file-write result")
	}
	if writtenEntry.ToolUseResult.Detail.Type != ToolUseResultDetailTypeCreate {
		t.Fatalf("detail type = %q, want %q", writtenEntry.ToolUseResult.Detail.Type, ToolUseResultDetailTypeCreate)
	}
	if writtenEntry.ToolUseResult.Detail.Content.Text != "package main" {
		t.Fatalf("content text = %q, want %q", writtenEntry.ToolUseResult.Detail.Content.Text, "package main")
	}

	blockEntry, err := DecodeTranscriptEntry(blocks)
	if err != nil {
		t.Fatalf("decode block-content entry: %v", err)
	}
	if blockEntry.ToolUseResult.Detail == nil {
		t.Fatalf("detail = nil, want the block-content result")
	}
	if len(blockEntry.ToolUseResult.Detail.Content.Blocks) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(blockEntry.ToolUseResult.Detail.Content.Blocks))
	}
	if blockEntry.ToolUseResult.Detail.Content.Blocks[0].Text != "agent findings" {
		t.Fatalf("content block text = %q, want %q", blockEntry.ToolUseResult.Detail.Content.Blocks[0].Text, "agent findings")
	}
}

func TestDecodeTranscriptEntryReadsFileReadResult(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-7","toolUseResult":{"type":"text",` +
		`"file":{"filePath":"/repo/README.md","content":"# repo","numLines":1,"startLine":1,` +
		`"totalLines":1,"truncatedByTokenCap":false}}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Detail == nil || entry.ToolUseResult.Detail.File == nil {
		t.Fatalf("file payload = nil, want the read result")
	}
	if entry.ToolUseResult.Detail.File.FilePath != "/repo/README.md" || entry.ToolUseResult.Detail.File.TotalLines != 1 {
		t.Fatalf("file = %+v, want /repo/README.md with 1 total line", entry.ToolUseResult.Detail.File)
	}
}

func TestDecodeTranscriptEntryReadsTurnAttribution(t *testing.T) {
	t.Parallel()
	line := []byte(`{"parentUuid":"parent-8","type":"assistant","uuid":"entry-8",` +
		`"timestamp":"2026-07-25T18:54:58.775Z","requestId":"req_8","agentId":"ad81ed22d5f62048a",` +
		`"attributionAgent":"general-purpose","attributionSkill":"gpt-subagent",` +
		`"attributionPlugin":"superpowers","attributionMcpServer":"semantic",` +
		`"attributionMcpTool":"get_indexing_status","effort":"high",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if !entry.Attributed() {
		t.Fatalf("attributed = false, want true for %+v", entry.TurnAttribution)
	}
	if entry.Agent != "general-purpose" {
		t.Fatalf("agent = %q, want general-purpose", entry.Agent)
	}
	if entry.Skill != "gpt-subagent" {
		t.Fatalf("skill = %q, want gpt-subagent", entry.Skill)
	}
	if entry.Plugin != "superpowers" {
		t.Fatalf("plugin = %q, want superpowers", entry.Plugin)
	}
	if entry.MCPServer != "semantic" || entry.MCPTool != "get_indexing_status" {
		t.Fatalf("mcp server/tool = %q/%q, want semantic/get_indexing_status", entry.MCPServer, entry.MCPTool)
	}
	if entry.AgentID != "ad81ed22d5f62048a" || entry.Effort != "high" {
		t.Fatalf("agent id/effort = %q/%q, want ad81ed22d5f62048a/high", entry.AgentID, entry.Effort)
	}
}

func TestDecodeTranscriptEntryLeavesAttributionEmptyWhenAbsent(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"assistant","uuid":"entry-9","message":{"role":"assistant","content":[]}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.Attributed() {
		t.Fatalf("attributed = true, want false for %+v", entry.TurnAttribution)
	}
}

// TestDecodeTranscriptEntryReadsRecordWithoutNewerFields covers a transcript
// written by an older Claude Code release: every field added since is absent
// and must decode to its zero value rather than failing the line.
func TestDecodeTranscriptEntryReadsRecordWithoutNewerFields(t *testing.T) {
	t.Parallel()
	line := []byte(`{"uuid":"entry-10","parentUuid":"parent-10","type":"user",` +
		`"timestamp":"2024-01-02T03:04:05Z","isMeta":false,` +
		`"message":{"role":"user","content":"hello from an old transcript"}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.UUID != "entry-10" || entry.ParentUUID != "parent-10" || entry.Type != EntryTypeUser {
		t.Fatalf("core fields = %q/%q/%q, want entry-10/parent-10/user", entry.UUID, entry.ParentUUID, entry.Type)
	}
	if !entry.Timestamp.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("timestamp = %s, want 2024-01-02T03:04:05Z", entry.Timestamp)
	}
	if entry.ToolUseResult.Kind != ToolUseResultKindAbsent {
		t.Fatalf("tool use result kind = %q, want absent", entry.ToolUseResult.Kind)
	}
	if entry.Error.Kind != EntryErrorKindAbsent {
		t.Fatalf("error kind = %q, want absent", entry.Error.Kind)
	}
	if entry.Attributed() {
		t.Fatalf("attributed = true, want false")
	}
	if entry.Attachment != nil || entry.Origin != nil || entry.Snapshot != nil ||
		entry.Backup != nil || entry.WorktreeSession != nil || entry.MCPMeta != nil || entry.ForkedFrom != nil {
		t.Fatalf("an absent object field decoded to a non-nil value: %+v", entry)
	}
	if entry.Entrypoint != EntrypointUnspecified || entry.PermissionMode != PermissionModeUnspecified ||
		entry.PromptSource != PromptSourceUnspecified || entry.Level != EntryLevelUnspecified ||
		entry.Operation != QueueOperationUnspecified || entry.ToolDenialKind != ToolDenialKindUnspecified {
		t.Fatalf("an absent enum field decoded to a non-zero value: %+v", entry)
	}
	if entry.RequestID != "" || entry.PromptID != "" || entry.LeafUUID != "" || entry.HookCount != 0 ||
		entry.PRNumber != 0 || entry.RetryInMs != 0 {
		t.Fatalf("an absent scalar field decoded to a non-zero value: %+v", entry)
	}
}

func TestDecodeTranscriptEntryReadsAPIErrorRecord(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"system","subtype":"api_error","uuid":"entry-11","level":"error",` +
		`"content":"API Error: 529 Overloaded","isMeta":false,"retryAttempt":1,"maxRetries":10,` +
		`"retryInMs":1567.5,"error":{"message":"529 Overloaded","status":529,` +
		`"requestId":"req_11","formatted":"529 Overloaded","connection":null,` +
		`"isNetworkDown":false,"rateLimits":null}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.Subtype != EntrySubtypeAPIError || entry.Level != EntryLevelError {
		t.Fatalf("subtype/level = %q/%q, want api_error/error", entry.Subtype, entry.Level)
	}
	if entry.Error.Kind != EntryErrorKindDetail {
		t.Fatalf("error kind = %q, want %q", entry.Error.Kind, EntryErrorKindDetail)
	}
	if entry.Error.Status != 529 || entry.Error.RequestID != "req_11" {
		t.Fatalf("error = %+v, want status 529 from req_11", entry.Error)
	}
	if entry.RetryAttempt != 1 || entry.MaxRetries != 10 || entry.RetryInMs != 1567.5 {
		t.Fatalf("retry plan = %d/%d/%v, want 1/10/1567.5", entry.RetryAttempt, entry.MaxRetries, entry.RetryInMs)
	}
}

func TestDecodeTranscriptEntryReadsErrorCodeString(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"assistant","uuid":"entry-12","isApiErrorMessage":true,` +
		`"apiErrorStatus":429,"apiErrorIsTransient":true,"error":"rate_limit"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.Error.Kind != EntryErrorKindCode || entry.Error.Code != "rate_limit" {
		t.Fatalf("error = %+v, want the rate_limit code form", entry.Error)
	}
	if !entry.IsAPIErrorMessage || entry.APIErrorStatus != 429 || !entry.APIErrorIsTransient {
		t.Fatalf("api error flags = %v/%d/%v, want true/429/true", entry.IsAPIErrorMessage, entry.APIErrorStatus, entry.APIErrorIsTransient)
	}
}

func TestDecodeTranscriptEntryReadsAttachmentRecord(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"attachment","uuid":"entry-13","sessionId":"sess-13",` +
		`"session_id":"sess-13","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"attachment":{"type":"hook_success","hookName":"Stop","toolUseID":"tool-13",` +
		`"hookEvent":"Stop","content":"","stdout":"{}\n","stderr":"","exitCode":0,` +
		`"command":"agent-gate","durationMs":1702,"sentinel":true}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.Type != EntryTypeAttachment {
		t.Fatalf("type = %q, want attachment", entry.Type)
	}
	if entry.Attachment == nil {
		t.Fatalf("attachment = nil, want the hook record")
	}
	if entry.Attachment.Type != AttachmentTypeHookSuccess {
		t.Fatalf("attachment type = %q, want %q", entry.Attachment.Type, AttachmentTypeHookSuccess)
	}
	if entry.Attachment.HookName != "Stop" || entry.Attachment.DurationMs != 1702 || entry.Attachment.Stdout != "{}\n" {
		t.Fatalf("hook fields = %+v, want the Stop hook that ran for 1702ms", entry.Attachment)
	}
	if entry.HookSessionID != "sess-13" {
		t.Fatalf("session_id = %q, want sess-13", entry.HookSessionID)
	}
	// The struct models the shared attachment keys, so a type-specific key it
	// does not model must still be recoverable from the kept original object.
	if !strings.Contains(string(entry.Attachment.Raw), `"sentinel":true`) {
		t.Fatalf("attachment raw = %q, want it to keep the unmodeled sentinel key", string(entry.Attachment.Raw))
	}
}

func TestDecodeTranscriptEntryReadsHookSummaryRecord(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"system","subtype":"stop_hook_summary","uuid":"entry-14",` +
		`"level":"suggestion","hasOutput":true,"hookCount":5,"hookErrors":[],` +
		`"hookAdditionalContext":[],"preventedContinuation":false,"stopReason":"",` +
		`"toolUseID":"tool-14","hookInfos":[{"command":"agent-gate","durationMs":234},` +
		`{"command":"cmux hooks feed"}]}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.HookCount != 5 || !entry.HasOutput {
		t.Fatalf("hook count/output = %d/%v, want 5/true", entry.HookCount, entry.HasOutput)
	}
	if len(entry.HookInfos) != 2 {
		t.Fatalf("hook infos = %d, want 2", len(entry.HookInfos))
	}
	if entry.HookInfos[0].Command != "agent-gate" || entry.HookInfos[0].DurationMs != 234 {
		t.Fatalf("first hook = %+v, want agent-gate at 234ms", entry.HookInfos[0])
	}
	if entry.HookInfos[1].DurationMs != 0 {
		t.Fatalf("second hook duration = %d, want 0 for a hook with no recorded duration", entry.HookInfos[1].DurationMs)
	}
}

func TestDecodeTranscriptEntryReadsSessionControlRecords(t *testing.T) {
	t.Parallel()
	worktree := []byte(`{"type":"worktree-state","sessionId":"sess-15","worktreeSession":{` +
		`"originalCwd":"/repo","preEnterOriginalCwd":"/repo",` +
		`"worktreePath":"/repo/.claude/worktrees/feature","worktreeName":"feature",` +
		`"worktreeBranch":"worktree-feature","originalBranch":"main",` +
		`"originalHeadCommit":"878e0fc","sessionId":"sess-15"}}`)
	snapshot := []byte(`{"type":"file-history-snapshot","messageId":"msg-16",` +
		`"isSnapshotUpdate":false,"snapshot":{"messageId":"msg-16",` +
		`"timestamp":"2026-07-26T14:34:29.478Z","trackedFileBackups":{` +
		`"/repo/main.go":{"backupFileName":null,"version":3,` +
		`"backupTime":"2026-07-24T19:06:44.845Z","realParentDir":"/repo"}}}}`)
	pullRequest := []byte(`{"type":"pr-link","sessionId":"sess-17","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"prNumber":161,"prRepository":"agoodkind/lm-semantic-search",` +
		`"prUrl":"https://github.com/agoodkind/lm-semantic-search/pull/161"}`)
	queued := []byte(`{"type":"queue-operation","sessionId":"sess-18","operation":"enqueue",` +
		`"content":"run the tests","timestamp":"2026-07-25T18:54:58.775Z"}`)

	worktreeEntry, err := DecodeTranscriptEntry(worktree)
	if err != nil {
		t.Fatalf("decode worktree entry: %v", err)
	}
	if worktreeEntry.Type != EntryTypeWorktreeState || worktreeEntry.WorktreeSession == nil {
		t.Fatalf("worktree entry = %+v, want a worktree-state record", worktreeEntry)
	}
	if worktreeEntry.WorktreeSession.WorktreeBranch != "worktree-feature" || worktreeEntry.WorktreeSession.OriginalBranch != "main" {
		t.Fatalf("worktree session = %+v, want worktree-feature off main", worktreeEntry.WorktreeSession)
	}

	snapshotEntry, err := DecodeTranscriptEntry(snapshot)
	if err != nil {
		t.Fatalf("decode snapshot entry: %v", err)
	}
	if snapshotEntry.Snapshot == nil {
		t.Fatalf("snapshot = nil, want the tracked-file backup set")
	}
	backup, ok := snapshotEntry.Snapshot.TrackedFileBackups["/repo/main.go"]
	if !ok {
		t.Fatalf("tracked backups = %+v, want an entry for /repo/main.go", snapshotEntry.Snapshot.TrackedFileBackups)
	}
	if backup.Version != 3 || backup.BackupFileName != "" {
		t.Fatalf("backup = %+v, want version 3 with a null backup file name", backup)
	}
	if backup.BackupTime.IsZero() {
		t.Fatalf("backup time is zero, want the parsed 2026-07-24T19:06:44.845Z")
	}

	pullRequestEntry, err := DecodeTranscriptEntry(pullRequest)
	if err != nil {
		t.Fatalf("decode pr-link entry: %v", err)
	}
	if pullRequestEntry.PRNumber != 161 || pullRequestEntry.PRRepository != "agoodkind/lm-semantic-search" {
		t.Fatalf("pr link = %+v, want pull request 161", pullRequestEntry)
	}

	queuedEntry, err := DecodeTranscriptEntry(queued)
	if err != nil {
		t.Fatalf("decode queue-operation entry: %v", err)
	}
	if queuedEntry.Operation != QueueOperationEnqueue || queuedEntry.Content != "run the tests" {
		t.Fatalf("queue operation = %q with content %q, want enqueue/run the tests", queuedEntry.Operation, queuedEntry.Content)
	}
}

func TestDecodeTranscriptEntryReadsUserTurnProvenance(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-19","promptId":"prompt-19",` +
		`"promptSource":"queued","permissionMode":"bypassPermissions",` +
		`"origin":{"kind":"task-notification"},"toolDenialKind":"permission-rule",` +
		`"sourceToolUseID":"toolu_19","sourceToolAssistantUUID":"assistant-19",` +
		`"imagePasteIds":[1,2],"mcpMeta":{"structuredContent":{"body":"ok"}},` +
		`"message":{"role":"user","content":"go on"}}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.PromptSource != PromptSourceQueued {
		t.Fatalf("prompt source = %q, want queued", entry.PromptSource)
	}
	if entry.PermissionMode != PermissionModeBypassPermissions {
		t.Fatalf("permission mode = %q, want bypassPermissions", entry.PermissionMode)
	}
	if entry.Origin == nil || entry.Origin.Kind != OriginKindTaskNotification {
		t.Fatalf("origin = %+v, want a task-notification origin", entry.Origin)
	}
	if entry.ToolDenialKind != ToolDenialKindPermissionRule {
		t.Fatalf("tool denial = %q, want permission-rule", entry.ToolDenialKind)
	}
	if len(entry.ImagePasteIDs) != 2 || entry.ImagePasteIDs[1] != 2 {
		t.Fatalf("image paste ids = %v, want [1 2]", entry.ImagePasteIDs)
	}
	if entry.MCPMeta == nil || !strings.Contains(string(entry.MCPMeta.StructuredContent), `"body":"ok"`) {
		t.Fatalf("mcp meta = %+v, want the structured content preserved", entry.MCPMeta)
	}
	if entry.SourceToolUseID != "toolu_19" || entry.SourceToolAssistantUUID != "assistant-19" {
		t.Fatalf("source tool ids = %q/%q, want toolu_19/assistant-19", entry.SourceToolUseID, entry.SourceToolAssistantUUID)
	}
}

func TestDecodeTranscriptEntryRejectsNonJSONLine(t *testing.T) {
	t.Parallel()
	if _, err := DecodeTranscriptEntry([]byte("not json at all")); err == nil {
		t.Fatalf("decode error = nil, want a failure for a line that is not JSON")
	}
}

// TestStreamKeepsTurnsAmongControlRecords drives the public stream over a
// transcript that mixes turns with the sidecar record types, so the record
// kinds the parser now names still resolve to the same two conversation turns.
func TestStreamKeepsTurnsAmongControlRecords(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.jsonl")
	body := strings.Join([]string{
		`{"type":"attachment","uuid":"a1","attachment":{"type":"output_style","style":"Proactive","turnReminder":"stay brief"}}`,
		`{"type":"last-prompt","sessionId":"sess-20","lastPrompt":"/compact","leafUuid":"leaf-20"}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-07-25T18:54:58.775Z","promptSource":"typed","message":{"role":"user","content":"ship it"}}`,
		`{"type":"queue-operation","sessionId":"sess-20","operation":"dequeue","content":"ship it"}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-07-25T18:55:00.000Z","requestId":"req_20","attributionAgent":"general-purpose","message":{"role":"assistant","content":[{"type":"text","text":"shipped"}]}}`,
		`{"type":"file-history-snapshot","messageId":"msg-20","snapshot":{"messageId":"msg-20","timestamp":"2026-07-25T18:55:01.000Z","trackedFileBackups":{}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Text != "ship it" {
		t.Fatalf("first message = %q/%q, want user/ship it", messages[0].Role, messages[0].Text)
	}
	if messages[1].Role != "assistant" || messages[1].Text != "shipped" {
		t.Fatalf("second message = %q/%q, want assistant/shipped", messages[1].Role, messages[1].Text)
	}
}
