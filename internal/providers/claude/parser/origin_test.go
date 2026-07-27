package parser

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

// scanRecordAtPath writes a transcript at the given filename inside a fresh
// temporary directory and returns the record the parser derives for it, so each
// origin case exercises the real file-reading path rather than a header shim.
func scanRecordAtPath(t *testing.T, filename string, body string) conversation.Record {
	t.Helper()
	path := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	record, ok := New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !ok {
		t.Fatalf("ScanRecord returned ok=false for %s", filename)
	}
	return record
}

func TestScanRecordClassifiesSidechainTranscriptAsSubagentOrigin(t *testing.T) {
	t.Parallel()
	body := `{"sessionId":"agent-session","cwd":"/repo","isSidechain":true,"agentId":"explorer","type":"user","timestamp":"2026-07-01T10:00:00Z","content":"find the callers"}` + "\n" +
		`{"sessionId":"agent-session","isSidechain":true,"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"here they are"}]}}` + "\n"

	record := scanRecordAtPath(t, "agent-01234567.jsonl", body)

	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginSubagent)
	}
	if !record.IsSubagent() {
		t.Fatalf("IsSubagent = false, want true for a sidechain transcript")
	}
	// Claude names no parent conversation for a sidechain, and agentId names the
	// agent rather than a conversation, so no lineage is invented.
	if record.Lineage != nil {
		t.Fatalf("lineage = %#v, want nil; Claude supplies no parent conversation", record.Lineage)
	}
}

func TestScanRecordClassifiesOrdinaryTranscriptAsUserOrigin(t *testing.T) {
	t.Parallel()
	body := `{"sessionId":"user-session","cwd":"/repo","isSidechain":false,"type":"user","timestamp":"2026-07-01T10:00:00Z","content":"ship the feature"}` + "\n"

	record := scanRecordAtPath(t, "user-session.jsonl", body)

	if record.Origin != conversation.OriginUser {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginUser)
	}
	if record.IsSubagent() {
		t.Fatalf("IsSubagent = true, want false for an ordinary transcript")
	}
}

// TestScanRecordPrefersSidechainMarkerOverAgentFilename pins the marker as the
// authority. The agent-<id>.jsonl name is a convention rather than a documented
// contract, so a file that carries the name but not the marker must stay a user
// conversation.
func TestScanRecordPrefersSidechainMarkerOverAgentFilename(t *testing.T) {
	t.Parallel()
	body := `{"sessionId":"named-like-an-agent","cwd":"/repo","type":"user","timestamp":"2026-07-01T10:00:00Z","content":"not actually a sidechain"}` + "\n"

	record := scanRecordAtPath(t, "agent-89abcdef.jsonl", body)

	if record.Origin != conversation.OriginUser {
		t.Fatalf("origin = %q, want %q; the in-file marker must beat the filename", record.Origin, conversation.OriginUser)
	}
}

// TestScanRecordReadsSidechainMarkerFromALaterHeaderLine covers a transcript
// whose first line is a summary record with no marker, proving the origin comes
// from the conversation records rather than from line one alone.
func TestScanRecordReadsSidechainMarkerFromALaterHeaderLine(t *testing.T) {
	t.Parallel()
	body := `{"type":"summary","summary":"earlier work"}` + "\n" +
		`{"sessionId":"late-marker","cwd":"/repo","isSidechain":true,"type":"user","timestamp":"2026-07-01T10:00:00Z","content":"dispatched work"}` + "\n"

	record := scanRecordAtPath(t, "agent-late.jsonl", body)

	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginSubagent)
	}
}

// TestScanRecordReadsTheSidechainMarkerFromTheSessionRecord pins the origin to
// the record that supplied the session id. A sidechain entry appearing later
// inside a user's own transcript, which is how Claude Code wrote these before the
// subagents/ directory existed, must not reclassify the whole transcript: that
// would blank its native id and hide the user's own conversation.
func TestScanRecordReadsTheSidechainMarkerFromTheSessionRecord(t *testing.T) {
	t.Parallel()
	body := `{"sessionId":"user-session","cwd":"/repo","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":"ship the feature"}]}}` + "\n" +
		`{"sessionId":"user-session","isSidechain":true,"type":"user","message":{"role":"user","content":[{"type":"text","text":"dispatched work"}]}}` + "\n"

	record := scanRecordAtPath(t, "user-session.jsonl", body)

	if record.Origin != conversation.OriginUser {
		t.Fatalf("origin = %q, want %q; a later sidechain entry must not reclassify the transcript", record.Origin, conversation.OriginUser)
	}
	if record.NativeID != "user-session" {
		t.Fatalf("native id = %q, want user-session so the session id still resolves", record.NativeID)
	}
}

// TestScanRecordDoesNotTitleASidechainWithTheParentSessionID closes the other
// half of the identity split. A sidechain transcript records the dispatching
// conversation's session id, and Resolve matches on Title as well as id, so a
// title fallback to that id lets a lookup for the parent's session id return the
// agent's transcript instead.
//
// Real Claude transcripts carry user text at message.content rather than at a
// top-level content field, so the title fallback is the common case rather than
// an edge one.
func TestScanRecordDoesNotTitleASidechainWithTheParentSessionID(t *testing.T) {
	t.Parallel()
	body := `{"sessionId":"parent-session","cwd":"/repo","isSidechain":true,"agentId":"explorer","type":"user","timestamp":"2026-07-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"find the callers"}]}}` + "\n"

	record := scanRecordAtPath(t, "agent-01234567.jsonl", body)

	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginSubagent)
	}
	if record.Title == "parent-session" {
		t.Fatal("title is the dispatching conversation's session id, so Resolve can answer a parent lookup with the agent's transcript")
	}
	if record.Title == "" {
		t.Fatal("title is empty, so the row lists with no name at all")
	}
}
