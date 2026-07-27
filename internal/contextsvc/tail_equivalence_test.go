package contextsvc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/api/contextpb"
	"goodkind.io/clyde/internal/conversation"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	"goodkind.io/clyde/internal/transcript"
)

// realIndexSource adapts a real [conversation.Index] to [conversationSource].
// ListPage is a canned lookup over a fixed record list (the same shape the
// package's own fakeConversationSource uses), but LoadRecentTurns calls
// straight through to the real index, so a test exercises the real registry,
// the real provider parsers, and the real tail-read dispatch against a real
// fixture file on disk.
type realIndexSource struct {
	idx     *conversation.Index
	records []conversation.Record
}

func (source *realIndexSource) ListPage(
	_ context.Context,
	options conversation.ListOptions,
) (conversation.ListResult, error) {
	return conversation.FilterRecords(source.records, options), nil
}

func (source *realIndexSource) LoadRecentTurns(
	record conversation.Record,
	need int,
	opts conversation.LoadOptions,
) ([]transcript.Message, error) {
	return source.idx.LoadRecentTurns(record, need, opts)
}

func newRealConversationIndex() *conversation.Index {
	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	registry.Register(codexparser.New())
	return conversation.NewIndex(registry)
}

func writeFixtureLines(t *testing.T, filename string, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filename)
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", filename, err)
	}
	return path
}

func claudeFixtureUserLine(uuid string, timestamp string, text string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`,
		uuid, timestamp, text,
	)
}

func claudeFixtureAssistantLine(uuid string, timestamp string, text string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`,
		uuid, timestamp, text,
	)
}

func claudeFixtureSystemCompactBoundaryLine(uuid string, timestamp string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"system","subtype":"compact_boundary","timestamp":%q,"content":"Conversation compacted"}`,
		uuid, timestamp,
	)
}

func claudeFixtureUserToolResultOnlyLine(uuid string, timestamp string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-%s"}]}}`,
		uuid, timestamp, uuid,
	)
}

func codexFixtureUserLine(timestamp string, text string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`,
		timestamp, text,
	)
}

func codexFixtureAssistantLine(timestamp string, text string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`,
		timestamp, text,
	)
}

func codexFixtureCompactedLine(timestamp string, summary string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"compacted","payload":{"message":%q,"replacement_history":[]}}`,
		timestamp, summary,
	)
}

func codexFixtureToolOutputLine(timestamp string, callID string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":"tool result text"}}`,
		timestamp, callID,
	)
}

// fixtureRecord builds a [conversation.Record] for the given provider and
// artifact path. GetRecentTurns only reads Provider, ArtifactPath, and
// WorkspaceRoot off the record it resolves, so the rest are placeholders.
func fixtureRecord(id string, provider conversation.Provider, workspaceRef string, artifactPath string) conversation.Record {
	return conversation.Record{
		ID:            id,
		Provider:      provider,
		NativeID:      id,
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: workspaceRef,
		ArtifactPath:  artifactPath,
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}
}

// turnFields extracts the comparable fields off a reply's turns so the
// assertion does not depend on proto message internals.
func turnFields(turns []*contextpb.Turn) [][3]string {
	out := make([][3]string, 0, len(turns))
	for _, turn := range turns {
		out = append(out, [3]string{turn.GetRole(), turn.GetText(), turn.GetTs()})
	}
	return out
}

// assertGetRecentTurnsUnchanged is the ticket's load-bearing equivalence
// check at the contextsvc public boundary: GetRecentTurns, wired to the new
// tail-read path through a real conversation.Index and real provider parsers,
// must return byte-for-byte the same turns as the reference reply computed
// through the old whole-file path (LoadMessagesWithOptions) plus the
// package's own recentTurns slicing.
func assertGetRecentTurnsUnchanged(
	t *testing.T,
	idx *conversation.Index,
	record conversation.Record,
	turnBudget int,
	maxCharsPerTurn int,
) *contextpb.GetRecentTurnsReply {
	t.Helper()
	referenceMessages, err := idx.LoadMessagesWithOptions(record, conversation.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadMessagesWithOptions: %v", err)
	}
	wantTurns := recentTurns(referenceMessages, turnBudget, maxCharsPerTurn)

	srv := newWithSource(&realIndexSource{idx: idx, records: []conversation.Record{record}})
	got, err := srv.GetRecentTurns(context.Background(), &contextpb.GetRecentTurnsRequest{
		WorkspaceRef:    record.WorkspaceRoot,
		TurnBudget:      int32(turnBudget),
		MaxCharsPerTurn: int32(maxCharsPerTurn),
	})
	if err != nil {
		t.Fatalf("GetRecentTurns: %v", err)
	}

	want := turnFields(wantTurns)
	gotFields := turnFields(got.GetTurns())
	if len(gotFields) != len(want) {
		t.Fatalf("GetRecentTurns turns = %d, want %d\ngot:  %#v\nwant: %#v", len(gotFields), len(want), gotFields, want)
	}
	for i := range want {
		if gotFields[i] != want[i] {
			t.Fatalf("turn[%d] = %#v, want %#v", i, gotFields[i], want[i])
		}
	}
	return got
}

func TestGetRecentTurns_MatchesFullLoad_ClaudeNormalTranscript(t *testing.T) {
	t.Parallel()
	idx := newRealConversationIndex()
	var lines []string
	for i := range 8 {
		lines = append(lines,
			claudeFixtureUserLine(fmt.Sprintf("u%d", i), fmt.Sprintf("2026-04-24T19:%02d:00Z", i), fmt.Sprintf("user turn %d", i)),
			claudeFixtureAssistantLine(fmt.Sprintf("a%d", i), fmt.Sprintf("2026-04-24T19:%02d:30Z", i), fmt.Sprintf("assistant turn %d", i)),
		)
	}
	path := writeFixtureLines(t, "transcript.jsonl", lines)
	record := fixtureRecord("claude:normal", conversation.ProviderClaude, "/repo/claude-normal", path)

	for _, turnBudget := range []int{1, 2, 4, 100} {
		assertGetRecentTurnsUnchanged(t, idx, record, turnBudget, 280)
	}
}

func TestGetRecentTurns_MatchesFullLoad_ClaudeTrailingToolAndMetadataLines(t *testing.T) {
	t.Parallel()
	idx := newRealConversationIndex()
	lines := []string{
		claudeFixtureUserLine("u1", "2026-04-24T19:00:00Z", "first user turn"),
		claudeFixtureAssistantLine("a1", "2026-04-24T19:00:30Z", "first assistant turn"),
		claudeFixtureUserLine("u2", "2026-04-24T19:01:00Z", "second user turn"),
		claudeFixtureAssistantLine("a2", "2026-04-24T19:01:30Z", "second assistant turn"),
		claudeFixtureUserToolResultOnlyLine("u3", "2026-04-24T19:02:00Z"),
		claudeFixtureSystemCompactBoundaryLine("s1", "2026-04-24T19:02:01Z"),
		"",
	}
	path := writeFixtureLines(t, "transcript.jsonl", lines)
	record := fixtureRecord("claude:trailing", conversation.ProviderClaude, "/repo/claude-trailing", path)

	got := assertGetRecentTurnsUnchanged(t, idx, record, 2, 280)
	if len(got.GetTurns()) != 2 {
		t.Fatalf("turns = %d, want 2", len(got.GetTurns()))
	}
	if last := got.GetTurns()[1].GetText(); last != "second assistant turn" {
		t.Fatalf("last turn text = %q, want %q", last, "second assistant turn")
	}
}

func TestGetRecentTurns_MatchesFullLoad_CodexNormalRollout(t *testing.T) {
	t.Parallel()
	idx := newRealConversationIndex()
	var lines []string
	for i := range 8 {
		lines = append(lines,
			codexFixtureUserLine(fmt.Sprintf("2026-05-02T17:%02d:00.000Z", i), fmt.Sprintf("user turn %d", i)),
			codexFixtureAssistantLine(fmt.Sprintf("2026-05-02T17:%02d:30.000Z", i), fmt.Sprintf("assistant turn %d", i)),
		)
	}
	path := writeFixtureLines(t, "rollout.jsonl", lines)
	record := fixtureRecord("codex:normal", conversation.ProviderCodex, "/repo/codex-normal", path)

	for _, turnBudget := range []int{1, 2, 4, 100} {
		assertGetRecentTurnsUnchanged(t, idx, record, turnBudget, 280)
	}
}

func TestGetRecentTurns_MatchesFullLoad_CodexCompactionBoundary(t *testing.T) {
	t.Parallel()
	idx := newRealConversationIndex()
	lines := []string{
		codexFixtureUserLine("2026-05-02T17:00:00.000Z", "turn before compaction 1"),
		codexFixtureAssistantLine("2026-05-02T17:00:30.000Z", "turn before compaction 2"),
		codexFixtureCompactedLine("2026-05-02T17:01:00.000Z", "compacted summary of prior turns"),
		codexFixtureToolOutputLine("2026-05-02T17:01:01.000Z", "call-1"),
		codexFixtureAssistantLine("2026-05-02T17:01:30.000Z", "turn after compaction 1"),
		codexFixtureUserLine("2026-05-02T17:02:00.000Z", "turn after compaction 2"),
	}
	path := writeFixtureLines(t, "rollout.jsonl", lines)
	record := fixtureRecord("codex:compaction", conversation.ProviderCodex, "/repo/codex-compaction", path)

	for _, turnBudget := range []int{1, 3, 4, 100} {
		assertGetRecentTurnsUnchanged(t, idx, record, turnBudget, 280)
	}
}

func TestGetRecentTurns_MatchesFullLoad_ShorterThanBudget(t *testing.T) {
	t.Parallel()
	idx := newRealConversationIndex()
	lines := []string{
		claudeFixtureUserLine("u1", "2026-04-24T19:00:00Z", "only user turn"),
	}
	path := writeFixtureLines(t, "transcript.jsonl", lines)
	record := fixtureRecord("claude:short", conversation.ProviderClaude, "/repo/claude-short", path)

	got := assertGetRecentTurnsUnchanged(t, idx, record, 4, 280)
	if len(got.GetTurns()) != 1 {
		t.Fatalf("turns = %d, want 1", len(got.GetTurns()))
	}
}

func TestGetRecentTurns_MatchesFullLoad_NoQualifyingMessages(t *testing.T) {
	t.Parallel()
	idx := newRealConversationIndex()
	lines := []string{
		claudeFixtureUserToolResultOnlyLine("u1", "2026-04-24T19:00:00Z"),
		claudeFixtureSystemCompactBoundaryLine("s1", "2026-04-24T19:00:01Z"),
	}
	path := writeFixtureLines(t, "transcript.jsonl", lines)
	record := fixtureRecord("claude:empty", conversation.ProviderClaude, "/repo/claude-empty", path)

	got := assertGetRecentTurnsUnchanged(t, idx, record, 4, 280)
	if len(got.GetTurns()) != 0 {
		t.Fatalf("turns = %d, want 0", len(got.GetTurns()))
	}
}
