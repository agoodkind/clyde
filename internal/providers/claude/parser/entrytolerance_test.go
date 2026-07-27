package parser

import (
	"slices"
	"testing"
	"time"
)

// The fixtures below keep the key order Claude Code actually writes, where
// sessionId, cwd, gitBranch, version, entrypoint, userType, and slug all follow
// toolUseResult. Ordering is load-bearing here: a nested decode that gives up
// on the union field costs every key after it, so a fixture that puts the
// session fields first would pass while the record was still truncated.

func TestDecodeTranscriptEntryKeepsFieldsAfterMistypedToolUseResult(t *testing.T) {
	t.Parallel()
	line := []byte(`{"parentUuid":"parent-1","isSidechain":false,"promptId":"prompt-1",` +
		`"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1",` +
		`"type":"tool_result","content":"ok"}]},"uuid":"entry-1",` +
		`"timestamp":"2026-07-25T18:54:58.775Z",` +
		`"toolUseResult":{"stdout":"ok","timedOutAfterMs":1.5},` +
		`"sourceToolAssistantUUID":"assistant-1","userType":"external","entrypoint":"cli",` +
		`"cwd":"/repo","sessionId":"sess-1","version":"2.1.220","gitBranch":"main",` +
		`"slug":"sleepy-feather"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.SessionID != "sess-1" || entry.CWD != "/repo" || entry.GitBranch != "main" {
		t.Fatalf("session/cwd/branch = %q/%q/%q, want sess-1//repo/main: the keys after toolUseResult must survive",
			entry.SessionID, entry.CWD, entry.GitBranch)
	}
	if entry.Version != "2.1.220" || entry.Slug != "sleepy-feather" || entry.Entrypoint != EntrypointCLI {
		t.Fatalf("version/slug/entrypoint = %q/%q/%q, want 2.1.220/sleepy-feather/cli",
			entry.Version, entry.Slug, entry.Entrypoint)
	}
	if entry.UserType != "external" || entry.SourceToolAssistantUUID != "assistant-1" {
		t.Fatalf("userType/sourceToolAssistantUUID = %q/%q, want external/assistant-1",
			entry.UserType, entry.SourceToolAssistantUUID)
	}
	if entry.ToolUseResult.Detail == nil || entry.ToolUseResult.Detail.Stdout != "ok" {
		t.Fatalf("detail = %+v, want the stdout that decoded before the mistyped key", entry.ToolUseResult.Detail)
	}
}

func TestDecodeTranscriptEntryKeepsBlocksDecodedBeforeAMistypedBlock(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-2",` +
		`"toolUseResult":[{"type":"text","text":"first block survives the decoder"},` +
		`{"type":"text","text":5}],"sessionId":"sess-2","cwd":"/repo"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Kind != ToolUseResultKindBlocks {
		t.Fatalf("kind = %q, want %q", entry.ToolUseResult.Kind, ToolUseResultKindBlocks)
	}
	if len(entry.ToolUseResult.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2: a kind of blocks must not come with an empty slice",
			len(entry.ToolUseResult.Blocks))
	}
	if entry.ToolUseResult.Blocks[0].Text != "first block survives the decoder" {
		t.Fatalf("first block text = %q, want the block that decoded cleanly",
			entry.ToolUseResult.Blocks[0].Text)
	}
	if entry.SessionID != "sess-2" || entry.CWD != "/repo" {
		t.Fatalf("session/cwd = %q/%q, want sess-2//repo", entry.SessionID, entry.CWD)
	}
}

func TestDecodeTranscriptEntryKeepsErrorDetailAfterAMistypedMember(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"system","subtype":"api_error","uuid":"entry-3","level":"error",` +
		`"content":"API Error: 529 Overloaded",` +
		`"error":{"message":"529 Overloaded","status":"529","requestId":"req_3"},` +
		`"userType":"external","cwd":"/repo","sessionId":"sess-3","gitBranch":"main"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.Error.Kind != EntryErrorKindDetail {
		t.Fatalf("error kind = %q, want %q", entry.Error.Kind, EntryErrorKindDetail)
	}
	if entry.Error.Message != "529 Overloaded" || entry.Error.RequestID != "req_3" {
		t.Fatalf("error = %+v, want the members that decoded before the mistyped status", entry.Error)
	}
	if entry.SessionID != "sess-3" || entry.CWD != "/repo" || entry.GitBranch != "main" {
		t.Fatalf("session/cwd/branch = %q/%q/%q, want sess-3//repo/main",
			entry.SessionID, entry.CWD, entry.GitBranch)
	}
}

func TestDecodeTranscriptEntryKeepsAttachmentAfterAMistypedMember(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"attachment","uuid":"entry-4",` +
		`"attachment":{"type":"hook_success","hookName":"Stop","exitCode":"0",` +
		`"command":"agent-gate","durationMs":1702},` +
		`"cwd":"/repo","sessionId":"sess-4","gitBranch":"main"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.Attachment == nil {
		t.Fatalf("attachment = nil, want the hook record that decoded around the mistyped exitCode")
	}
	if entry.Attachment.HookName != "Stop" || entry.Attachment.Command != "agent-gate" {
		t.Fatalf("attachment = %+v, want the Stop hook that ran agent-gate", entry.Attachment)
	}
	if entry.SessionID != "sess-4" || entry.CWD != "/repo" || entry.GitBranch != "main" {
		t.Fatalf("session/cwd/branch = %q/%q/%q, want sess-4//repo/main",
			entry.SessionID, entry.CWD, entry.GitBranch)
	}
}

func TestDecodeTranscriptEntryKeepsDetailAfterAMistypedContentBlock(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-5","toolUseResult":{"type":"create",` +
		`"filePath":"/repo/main.go","content":[{"type":"text","text":"agent findings"},5],` +
		`"originalFile":"package main"},"cwd":"/repo","sessionId":"sess-5"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Detail == nil {
		t.Fatalf("detail = nil, want the file-write result")
	}
	if len(entry.ToolUseResult.Detail.Content.Blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2: the block that decoded cleanly must survive",
			len(entry.ToolUseResult.Detail.Content.Blocks))
	}
	if entry.ToolUseResult.Detail.Content.Blocks[0].Text != "agent findings" {
		t.Fatalf("first content block = %q, want agent findings",
			entry.ToolUseResult.Detail.Content.Blocks[0].Text)
	}
	if entry.ToolUseResult.Detail.OriginalFile != "package main" {
		t.Fatalf("originalFile = %q, want the detail key after content",
			entry.ToolUseResult.Detail.OriginalFile)
	}
	if entry.SessionID != "sess-5" {
		t.Fatalf("sessionId = %q, want sess-5", entry.SessionID)
	}
}

func TestDecodeTranscriptEntryRejectsNonObjectLine(t *testing.T) {
	t.Parallel()
	lines := []string{`[1,2,3]`, `"a string line"`, `42`, `true`, `null`}
	for _, line := range lines {
		if _, err := DecodeTranscriptEntry([]byte(line)); err == nil {
			t.Errorf("decode error = nil for %s, want a failure for a line that is not a JSON object", line)
		}
	}
}

// TestDecodeTranscriptEntryReportsWhichFieldsDecodedPartially is the point of
// the tolerance: a record that lost a field must not look like a whole one.
func TestDecodeTranscriptEntryReportsWhichFieldsDecodedPartially(t *testing.T) {
	t.Parallel()
	whole := []byte(`{"type":"user","uuid":"entry-8","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"toolUseResult":{"stdout":"ok"},"sessionId":"sess-8"}`)
	partial := []byte(`{"type":"user","uuid":"entry-9","timestamp":"2026-07-25T18:54:58.775Z",` +
		`"toolUseResult":{"stdout":"ok","timedOutAfterMs":1.5},"error":{"status":"529"},` +
		`"imagePasteIds":["one"],"sessionId":"sess-9"}`)

	wholeEntry, err := DecodeTranscriptEntry(whole)
	if err != nil {
		t.Fatalf("decode whole entry: %v", err)
	}
	if !wholeEntry.Decode.Complete() {
		t.Fatalf("decode = %+v, want a complete report for a record with no mismatch", wholeEntry.Decode)
	}

	partialEntry, err := DecodeTranscriptEntry(partial)
	if err != nil {
		t.Fatalf("decode partial entry: %v", err)
	}
	if partialEntry.Decode.Complete() {
		t.Fatalf("decode = %+v, want a partial report for a record with three mismatched fields", partialEntry.Decode)
	}
	if partialEntry.Decode.Outcome != EntryDecodePartial {
		t.Fatalf("outcome = %q, want %q", partialEntry.Decode.Outcome, EntryDecodePartial)
	}
	for _, want := range []string{"toolUseResult", "error", "imagePasteIds"} {
		if !slices.Contains(partialEntry.Decode.Fields, want) {
			t.Errorf("fields = %v, want it to name %q", partialEntry.Decode.Fields, want)
		}
	}
	if partialEntry.SessionID != "sess-9" {
		t.Fatalf("sessionId = %q, want sess-9: a partial record still carries every other key", partialEntry.SessionID)
	}
}

// TestDecodeTranscriptEntryKeepsRecordAroundAnUnreadableTimestamp covers the
// remaining unmarshaler the entry tree does not own outright. time.Time fails
// a value it cannot parse, which would abort the record the same way a union
// field does, so the timestamp decodes through the parser's own type.
func TestDecodeTranscriptEntryKeepsRecordAroundAnUnreadableTimestamp(t *testing.T) {
	t.Parallel()
	line := []byte(`{"parentUuid":"parent-10","type":"user","uuid":"entry-10",` +
		`"timestamp":"yesterday","cwd":"/repo","sessionId":"sess-10","gitBranch":"main"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.SessionID != "sess-10" || entry.CWD != "/repo" || entry.GitBranch != "main" {
		t.Fatalf("session/cwd/branch = %q/%q/%q, want sess-10//repo/main",
			entry.SessionID, entry.CWD, entry.GitBranch)
	}
	if !entry.Timestamp.Time.IsZero() {
		t.Fatalf("timestamp = %s, want the zero time for a value that does not parse", entry.Timestamp.Time)
	}
	if entry.Timestamp.Text != "yesterday" {
		t.Fatalf("timestamp text = %q, want the original yesterday", entry.Timestamp.Text)
	}
	if !slices.Contains(entry.Decode.Fields, "timestamp") {
		t.Fatalf("fields = %v, want it to name timestamp", entry.Decode.Fields)
	}
}

func TestDecodeTranscriptEntryReadsSidechainMarker(t *testing.T) {
	t.Parallel()
	dispatched := []byte(`{"parentUuid":null,"isSidechain":true,"type":"user","uuid":"entry-11",` +
		`"timestamp":"2026-07-25T18:54:58.775Z","cwd":"/repo","sessionId":"sess-11"}`)
	own := []byte(`{"parentUuid":null,"isSidechain":false,"type":"user","uuid":"entry-12",` +
		`"timestamp":"2026-07-25T18:54:58.775Z","cwd":"/repo","sessionId":"sess-12"}`)

	dispatchedEntry, err := DecodeTranscriptEntry(dispatched)
	if err != nil {
		t.Fatalf("decode dispatched entry: %v", err)
	}
	if !dispatchedEntry.IsSidechain {
		t.Fatalf("isSidechain = false, want true for a turn a dispatched agent produced")
	}
	if !dispatchedEntry.Timestamp.Time.Equal(time.Date(2026, 7, 25, 18, 54, 58, 775000000, time.UTC)) {
		t.Fatalf("timestamp = %s, want 2026-07-25T18:54:58.775Z", dispatchedEntry.Timestamp.Time)
	}

	ownEntry, err := DecodeTranscriptEntry(own)
	if err != nil {
		t.Fatalf("decode own entry: %v", err)
	}
	if ownEntry.IsSidechain {
		t.Fatalf("isSidechain = true, want false for the user's own turn")
	}
}

func TestDecodeTranscriptEntryKeepsUnsupportedErrorShape(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"assistant","uuid":"entry-6","error":42,"cwd":"/repo","sessionId":"sess-6"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.Error.Kind != EntryErrorKindUnsupported {
		t.Fatalf("error kind = %q, want %q", entry.Error.Kind, EntryErrorKindUnsupported)
	}
	if string(entry.Error.Raw) != "42" {
		t.Fatalf("error raw = %q, want the original 42", string(entry.Error.Raw))
	}
	if entry.Error.Code != "" || entry.Error.Message != "" || entry.Error.Status != 0 {
		t.Fatalf("unsupported error filled a modeled member: %+v", entry.Error)
	}
	if entry.SessionID != "sess-6" {
		t.Fatalf("sessionId = %q, want sess-6", entry.SessionID)
	}
}

func TestDecodeTranscriptEntryKeepsUnsupportedContentShape(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"user","uuid":"entry-7","toolUseResult":{"type":"update",` +
		`"content":42,"originalFile":"package main"},"sessionId":"sess-7"}`)

	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if entry.ToolUseResult.Detail == nil {
		t.Fatalf("detail = nil, want the file-update result")
	}
	content := entry.ToolUseResult.Detail.Content
	if content.Text != "" || content.Blocks != nil {
		t.Fatalf("unsupported content filled a modeled member: %+v", content)
	}
	if string(content.Raw) != "42" {
		t.Fatalf("content raw = %q, want the original 42", string(content.Raw))
	}
	if entry.ToolUseResult.Detail.OriginalFile != "package main" || entry.SessionID != "sess-7" {
		t.Fatalf("originalFile/sessionId = %q/%q, want package main/sess-7",
			entry.ToolUseResult.Detail.OriginalFile, entry.SessionID)
	}
}
