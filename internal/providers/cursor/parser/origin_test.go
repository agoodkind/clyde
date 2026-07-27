package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

const (
	cursorParentConversationID   = "11111111-1111-4111-8111-111111111111"
	cursorSubagentConversationID = "22222222-2222-4222-8222-222222222222"
	cursorLooseConversationID    = "33333333-3333-4333-8333-333333333333"
)

// cursorTranscriptBody renders a minimal Cursor transcript: one user message.
func cursorTranscriptBody(text string) string {
	return `{"role":"user","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
}

// cursorSpawnTranscriptBody renders a parent transcript whose Task tool call
// resumes the given conversation id, matching the shape Cursor writes at
// message.content[].input.resume.
func cursorSpawnTranscriptBody(resumeID string) string {
	return cursorTranscriptBody("start the work") +
		`{"role":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Task","input":{"description":"dig into the callers","prompt":"find them","subagent_type":"explorer","resume":"` + resumeID + `"}}]}}` + "\n"
}

// writeCursorProject lays down a Cursor project root and points the parser at it.
// Each entry maps a path relative to the project key onto a file body.
func writeCursorProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", root)
	// Point the SQLite side at an empty root too, so discovery sees only these
	// fixtures rather than this machine's real Cursor composers.
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", t.TempDir())
	for relative, body := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create dir for %s: %v", relative, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", relative, err)
		}
	}
	return root
}

// scanCursorRecords discovers and scans every Cursor candidate, returning the
// records keyed by native conversation id.
func scanCursorRecords(t *testing.T) map[string]conversation.Record {
	t.Helper()
	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	records := make(map[string]conversation.Record, len(candidates))
	for _, candidate := range candidates {
		record, ok := parser.ScanRecord(candidate.Path, candidate.Stamp)
		if !ok {
			continue
		}
		records[record.NativeID] = record
	}
	return records
}

// TestSubagentTranscriptUnderSubagentsDirIsClassifiedAndCarriesItsParent covers
// the path rule, which is the classifier for effectively every Cursor subagent
// transcript. The containing directory is literally the parent conversation id.
func TestSubagentTranscriptUnderSubagentsDirIsClassifiedAndCarriesItsParent(t *testing.T) {
	writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl":             cursorTranscriptBody("the user's own work"),
		"proj/agent-transcripts/" + cursorParentConversationID + "/subagents/" + cursorSubagentConversationID + ".jsonl": cursorTranscriptBody("dispatched work"),
	})

	records := scanCursorRecords(t)

	parent, ok := records[cursorParentConversationID]
	if !ok {
		t.Fatalf("parent conversation missing from %v", cursorRecordIDs(records))
	}
	if parent.Origin != conversation.OriginUser {
		t.Fatalf("parent origin = %q, want %q", parent.Origin, conversation.OriginUser)
	}

	subagent, ok := records[cursorSubagentConversationID]
	if !ok {
		t.Fatalf("subagent conversation missing from %v", cursorRecordIDs(records))
	}
	if subagent.Origin != conversation.OriginSubagent {
		t.Fatalf("subagent origin = %q, want %q", subagent.Origin, conversation.OriginSubagent)
	}
	// The subagent file is named with its own uuid, so unlike Claude there is no
	// collision with the parent's derived id.
	if subagent.ID == parent.ID {
		t.Fatalf("subagent id %q collides with the parent conversation", subagent.ID)
	}
	if subagent.Lineage == nil || subagent.Lineage.ParentNativeID != cursorParentConversationID {
		t.Fatalf("subagent lineage = %#v, want the containing directory as the parent", subagent.Lineage)
	}
}

// TestResumeLinkDoesNotReclassifyATopLevelConversation pins the corroboration
// semantics. A resume link corroborates the path shape and supplies the parent
// reference; on its own it never turns a conversation the path calls the user's
// own into a subagent, because that would hide a real conversation.
//
// The live corpus carries exactly this shape: a top-level transcript whose first
// record is a person's typed prompt, resumed later by another conversation.
func TestResumeLinkDoesNotReclassifyATopLevelConversation(t *testing.T) {
	writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl": cursorSpawnTranscriptBody(cursorLooseConversationID),
		"proj/agent-transcripts/" + cursorLooseConversationID + "/" + cursorLooseConversationID + ".jsonl":   cursorTranscriptBody("Implement the cell tunnel half of the plan"),
	})

	records := scanCursorRecords(t)

	resumed, ok := records[cursorLooseConversationID]
	if !ok {
		t.Fatalf("resumed conversation missing from %v", cursorRecordIDs(records))
	}
	if resumed.Origin != conversation.OriginUser {
		t.Fatalf("resumed origin = %q, want %q; a resume link alone must not hide a real conversation", resumed.Origin, conversation.OriginUser)
	}
	if resumed.Lineage != nil {
		t.Fatalf("resumed lineage = %#v, want nil; the path shape says this is the user's own conversation", resumed.Lineage)
	}
	if resumed.IsSubagent() {
		t.Fatalf("IsSubagent = true for a top-level transcript, which would hide it on the default setting")
	}
	if parent := records[cursorParentConversationID]; parent.Origin != conversation.OriginUser {
		t.Fatalf("resuming conversation origin = %q, want %q", parent.Origin, conversation.OriginUser)
	}
}

// TestResumeLinkSuppliesTheParentReferenceForASubagentTranscript pins the other
// half of corroboration: once the path shape has classified a transcript as a
// subagent, the resume link is preferred for the parent reference because the
// provider stated that link outright.
func TestResumeLinkSuppliesTheParentReferenceForASubagentTranscript(t *testing.T) {
	writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl":             cursorTranscriptBody("the user's own work"),
		"proj/agent-transcripts/" + cursorLooseConversationID + "/" + cursorLooseConversationID + ".jsonl":               cursorSpawnTranscriptBody(cursorSubagentConversationID),
		"proj/agent-transcripts/" + cursorParentConversationID + "/subagents/" + cursorSubagentConversationID + ".jsonl": cursorTranscriptBody("dispatched work"),
	})

	records := scanCursorRecords(t)

	subagent, ok := records[cursorSubagentConversationID]
	if !ok {
		t.Fatalf("subagent conversation missing from %v", cursorRecordIDs(records))
	}
	if subagent.Origin != conversation.OriginSubagent {
		t.Fatalf("subagent origin = %q, want %q", subagent.Origin, conversation.OriginSubagent)
	}
	if subagent.Lineage == nil || subagent.Lineage.ParentNativeID != cursorLooseConversationID {
		t.Fatalf("subagent lineage = %#v, want the resuming conversation as the parent", subagent.Lineage)
	}
}

// TestResumeLinkMatchesExactlyAndNotBySubstring pins the match boundary. A resume
// value that merely contains a conversation id must not classify that
// conversation, because a false positive here drops a real user conversation from
// the index.
func TestResumeLinkMatchesExactlyAndNotBySubstring(t *testing.T) {
	writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl": cursorSpawnTranscriptBody(cursorLooseConversationID + "-extra-suffix"),
		"proj/agent-transcripts/" + cursorLooseConversationID + "/" + cursorLooseConversationID + ".jsonl":   cursorTranscriptBody("a real user conversation"),
	})

	records := scanCursorRecords(t)

	nearMiss, ok := records[cursorLooseConversationID]
	if !ok {
		t.Fatalf("conversation missing from %v", cursorRecordIDs(records))
	}
	if nearMiss.Origin != conversation.OriginUser {
		t.Fatalf("origin = %q, want %q; a substring resume value must not classify a conversation", nearMiss.Origin, conversation.OriginUser)
	}
	if nearMiss.Lineage != nil {
		t.Fatalf("lineage = %#v, want nil for a conversation nothing actually resumed", nearMiss.Lineage)
	}
}

// TestResumeLinkIgnoresNonSpawnToolCalls proves the tool name is load-bearing: a
// parent agent that merely read a transcript file must not be treated as having
// dispatched it.
func TestResumeLinkIgnoresNonSpawnToolCalls(t *testing.T) {
	readToolBody := cursorTranscriptBody("start") +
		`{"role":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"resume":"` + cursorLooseConversationID + `"}}]}}` + "\n"
	writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl": readToolBody,
		"proj/agent-transcripts/" + cursorLooseConversationID + "/" + cursorLooseConversationID + ".jsonl":   cursorTranscriptBody("a real user conversation"),
	})

	records := scanCursorRecords(t)

	if origin := records[cursorLooseConversationID].Origin; origin != conversation.OriginUser {
		t.Fatalf("origin = %q, want %q; only Task and Subagent calls name a spawn", origin, conversation.OriginUser)
	}
}

// TestDiscoverIgnoresOtherFilesUnderAgentTranscripts pins the widened matcher: it
// admits the subagents/ shape and nothing else that happens to sit under
// agent-transcripts/.
func TestDiscoverIgnoresOtherFilesUnderAgentTranscripts(t *testing.T) {
	writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl":             cursorTranscriptBody("the user's own work"),
		"proj/agent-transcripts/" + cursorParentConversationID + "/notes.jsonl":                                          cursorTranscriptBody("a sidecar file"),
		"proj/agent-transcripts/" + cursorParentConversationID + "/attachments/blob.jsonl":                               cursorTranscriptBody("not a conversation"),
		"proj/agent-transcripts/" + cursorParentConversationID + "/subagents/nested/deeper.jsonl":                        cursorTranscriptBody("too deep"),
		"proj/agent-transcripts/" + cursorParentConversationID + "/subagents/" + cursorSubagentConversationID + ".jsonl": cursorTranscriptBody("dispatched work"),
	})

	records := scanCursorRecords(t)

	if len(records) != 2 {
		t.Fatalf("records = %v, want only the conversation and its subagent transcript", cursorRecordIDs(records))
	}
	if _, ok := records[cursorParentConversationID]; !ok {
		t.Fatalf("records = %v, want the conversation transcript", cursorRecordIDs(records))
	}
	if _, ok := records[cursorSubagentConversationID]; !ok {
		t.Fatalf("records = %v, want the subagent transcript", cursorRecordIDs(records))
	}
}

// TestResumeLinksAreNotRereadWhenAParentIsUnchanged pins the cache key that keeps
// the steady state cheap. Reading parent transcripts end to end is the one Cursor
// scan unbounded in file size, so an unchanged parent must never be re-read.
//
// The subagent transcript sits under one conversation's subagents/ directory
// while a different conversation's Task call resumed it, so the parent reference
// is the observable the resume link decides. The second body swaps Task for Read,
// which is the same byte length, and the modification time is restored, so the
// file looks unchanged to the cache. An implementation that re-read the resuming
// transcript would fall back to the containing directory; one that honors the
// cache keeps the earlier answer.
func TestResumeLinksAreNotRereadWhenAParentIsUnchanged(t *testing.T) {
	resumerRelative := "proj/agent-transcripts/" + cursorLooseConversationID + "/" + cursorLooseConversationID + ".jsonl"
	root := writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl":             cursorTranscriptBody("the user's own work"),
		"proj/agent-transcripts/" + cursorParentConversationID + "/subagents/" + cursorSubagentConversationID + ".jsonl": cursorTranscriptBody("dispatched work"),
		resumerRelative: cursorSpawnTranscriptBody(cursorSubagentConversationID),
	})
	resumerPath := filepath.Join(root, filepath.FromSlash(resumerRelative))

	parser := New()
	if _, err := parser.Discover(t.Context(), nil); err != nil {
		t.Fatalf("first Discover returned error: %v", err)
	}

	before, err := os.Stat(resumerPath)
	if err != nil {
		t.Fatalf("stat resuming transcript: %v", err)
	}
	rewritten := strings.Replace(cursorSpawnTranscriptBody(cursorSubagentConversationID), `"name":"Task"`, `"name":"Read"`, 1)
	if len(rewritten) != int(before.Size()) {
		t.Fatalf("rewritten body is %d bytes, want %d so the file looks unchanged", len(rewritten), before.Size())
	}
	if err := os.WriteFile(resumerPath, []byte(rewritten), 0o600); err != nil {
		t.Fatalf("rewrite resuming transcript: %v", err)
	}
	if err := os.Chtimes(resumerPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("restore resuming transcript mtime: %v", err)
	}

	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("second Discover returned error: %v", err)
	}
	for _, candidate := range candidates {
		record, ok := parser.ScanRecord(candidate.Path, candidate.Stamp)
		if !ok || record.NativeID != cursorSubagentConversationID {
			continue
		}
		if record.Lineage == nil || record.Lineage.ParentNativeID != cursorLooseConversationID {
			t.Fatalf("lineage = %#v, want the resuming conversation; an unchanged parent must be served from the cache rather than re-read", record.Lineage)
		}
		return
	}
	t.Fatalf("subagent conversation was not discovered on the second pass")
}

// TestScanRecordWithoutDiscoverStillClassifiesBySubagentPath covers the direct
// resolve path, which reaches a transcript without the Discover pass that builds
// the resume-link index. The path shape is available there too, so the origin
// and the parent reference must match what Discover would have produced; an
// unspecified origin here would be a hiding bypass for anything that later
// routes an ingestion or visibility decision through this resolve.
func TestScanRecordWithoutDiscoverStillClassifiesBySubagentPath(t *testing.T) {
	relative := "proj/agent-transcripts/" + cursorParentConversationID + "/subagents/" + cursorSubagentConversationID + ".jsonl"
	root := writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl": cursorTranscriptBody("the user's own work"),
		relative: cursorTranscriptBody("dispatched work"),
	})
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat subagent transcript: %v", err)
	}

	record, ok := New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !ok {
		t.Fatalf("ScanRecord returned ok=false for %s", path)
	}

	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q from the path shape alone", record.Origin, conversation.OriginSubagent)
	}
	if record.Lineage == nil || record.Lineage.ParentNativeID != cursorParentConversationID {
		t.Fatalf("lineage = %#v, want the containing directory as the parent", record.Lineage)
	}
}

func cursorRecordIDs(records map[string]conversation.Record) []string {
	out := make([]string, 0, len(records))
	for nativeID := range records {
		out = append(out, nativeID)
	}
	return out
}
