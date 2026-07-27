package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// writeClaudeTranscript writes real-shaped Claude transcript JSONL lines to a
// temp file and returns its path.
func writeClaudeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture transcript: %v", err)
	}
	return path
}

func claudeUserLine(uuid string, timestamp string, text string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`,
		uuid, timestamp, text,
	)
}

func claudeAssistantLine(uuid string, timestamp string, text string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`,
		uuid, timestamp, text,
	)
}

// claudeAssistantToolOnlyLine builds an assistant turn that carries only a
// tool call and no text. It still qualifies as a candidate turn (HasTools is
// true), matching parseLine's existing behavior.
func claudeAssistantToolOnlyLine(uuid string, timestamp string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t-%s","name":"Bash","input":{"command":"echo hi"}}]}}`,
		uuid, timestamp, uuid,
	)
}

// claudeUserToolResultOnlyLine builds a user entry that carries only a
// tool_result block. extractUserText skips tool_result blocks, so this line
// never qualifies as a candidate turn.
func claudeUserToolResultOnlyLine(uuid string, timestamp string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-%s"}]}}`,
		uuid, timestamp, uuid,
	)
}

// claudeSystemCompactBoundaryLine builds a compaction-boundary system record.
// With IncludeSystemMessages unset (the contextsvc call shape) it never
// qualifies as a candidate turn.
func claudeSystemCompactBoundaryLine(uuid string, timestamp string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"system","subtype":"compact_boundary","timestamp":%q,"content":"Conversation compacted"}`,
		uuid, timestamp,
	)
}

// claudeCompactSummaryUserLine builds a compaction summary carried on a user
// entry. It qualifies as a normal "user" candidate turn even though it also
// carries compaction metadata, matching recentTurns' existing behavior of not
// special-casing compacted messages.
func claudeCompactSummaryUserLine(uuid string, timestamp string, text string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"user","timestamp":%q,"isCompactSummary":true,"summarizeMetadata":{"messagesSummarized":3},"message":{"role":"user","content":%q}}`,
		uuid, timestamp, text,
	)
}

// claudeAttachmentLine builds a hook/reminder/context attachment record.
// parseLine excludes every EntryType other than user, assistant, and system,
// so this never qualifies as a candidate turn regardless of its content,
// making it a reliable large non-qualifying filler line.
func claudeAttachmentLine(uuid string, timestamp string, content string) string {
	return fmt.Sprintf(
		`{"uuid":%q,"type":"attachment","timestamp":%q,"content":%q}`,
		uuid, timestamp, content,
	)
}

var claudeTailOpts = conversation.LoadOptions{
	IncludeSystemPrompts:  false,
	IncludeSystemMessages: false,
	IncludeToolOutputs:    false,
}

// claudeTestRecord builds a minimal Record over a fixture path so a test can
// drive it through a real conversation.Index.
func claudeTestRecord(path string) conversation.Record {
	return conversation.Record{
		ID:            "claude:fixture",
		Provider:      providerid.ProviderClaude,
		NativeID:      "fixture",
		Lineage:       nil,
		Title:         "fixture",
		WorkspaceRoot: "/repo",
		ArtifactPath:  path,
		ArtifactKind:  "transcript",
		Model:         "model",
	}
}

func newClaudeTestIndex() *conversation.Index {
	registry := conversation.NewRegistry()
	registry.Register(New())
	return conversation.NewIndex(registry)
}

// loadFullAndRecent runs the full forward Stream and the real
// conversation.Index.LoadRecentTurns growth loop against the same fixture
// file and options, so a test can assert they agree. This exercises
// LoadRecentTurns end to end (TailSize, the geometric window growth, and
// StreamFrom), not just a single StreamFrom call.
func loadFullAndRecent(
	t *testing.T,
	path string,
	opts conversation.LoadOptions,
	turnBudget int,
) (full []transcript.Message, recent []transcript.Message) {
	t.Helper()
	p := New()
	full, err := conversation.CollectMessages(p.Stream(path, opts))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	idx := newClaudeTestIndex()
	recent, err = idx.LoadRecentTurns(claudeTestRecord(path), turnBudget, opts)
	if err != nil {
		t.Fatalf("LoadRecentTurns: %v", err)
	}
	return full, recent
}

func lastMessages(messages []transcript.Message, n int) []transcript.Message {
	start := len(messages) - n
	if start < 0 {
		start = 0
	}
	return messages[start:]
}

// assertTailMatchesFull is the equivalence assertion the ticket calls
// load-bearing: the trailing turnBudget messages the tail read returns must be
// byte-for-byte identical, in the same order, to the trailing turnBudget
// messages a full forward decode returns for the same file and options.
func assertTailMatchesFull(
	t *testing.T,
	full []transcript.Message,
	tail []transcript.Message,
	turnBudget int,
) {
	t.Helper()
	want := lastMessages(full, turnBudget)
	got := lastMessages(tail, turnBudget)
	if len(got) != len(want) {
		t.Fatalf("tail-of-tail length = %d, want %d (full=%d, tail=%d)", len(got), len(want), len(full), len(tail))
	}
	for i := range want {
		if !reflect.DeepEqual(want[i], got[i]) {
			t.Fatalf("message[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
	wantAtLeast := min(turnBudget, len(full))
	if len(tail) < wantAtLeast {
		t.Fatalf("LoadRecentTurns returned %d messages, want at least %d", len(tail), wantAtLeast)
	}
}

func TestStreamFrom_ZeroToSizeMatchesStream(t *testing.T) {
	t.Parallel()
	lines := []string{
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "first user turn"),
		claudeAssistantLine("a1", "2026-04-24T19:00:30Z", "first assistant turn"),
		claudeUserLine("u2", "2026-04-24T19:01:00Z", "second user turn"),
		claudeAssistantLine("a2", "2026-04-24T19:01:30Z", "second assistant turn"),
	}
	path := writeClaudeTranscript(t, lines)
	p := New()

	full, err := conversation.CollectMessages(p.Stream(path, claudeTailOpts))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	size, ok := p.TailSize(path)
	if !ok {
		t.Fatalf("TailSize reported ok=false for a plain transcript file")
	}
	fromZero, err := conversation.CollectMessages(p.StreamFrom(path, claudeTailOpts, 0, size))
	if err != nil {
		t.Fatalf("StreamFrom(0, size): %v", err)
	}
	if !reflect.DeepEqual(full, fromZero) {
		t.Fatalf("StreamFrom(path, opts, 0, size) = %#v, want byte-identical to Stream(path, opts) = %#v", fromZero, full)
	}
}

func TestStreamFrom_SmallBoundedWindowStaysCorrectWithoutReadingFromZero(t *testing.T) {
	t.Parallel()
	// Enough padding near the start of the file that a naive read from
	// offset 0 would be the only way to get the answer if StreamFrom did not
	// genuinely restrict itself to the requested range.
	var lines []string
	filler := strings.Repeat("f", 4096)
	for i := range 40 {
		lines = append(lines, claudeAttachmentLine(fmt.Sprintf("pad-%d", i), fmt.Sprintf("2026-04-24T18:%02d:00Z", i%60), filler))
	}
	lines = append(lines,
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "last user turn"),
		claudeAssistantLine("a1", "2026-04-24T19:00:30Z", "last assistant turn"),
	)
	path := writeClaudeTranscript(t, lines)
	p := New()

	full, err := conversation.CollectMessages(p.Stream(path, claudeTailOpts))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	size, ok := p.TailSize(path)
	if !ok {
		t.Fatalf("TailSize reported ok=false")
	}
	if size < 8*1024 {
		t.Fatalf("fixture size = %d, want at least 8KiB so the bounded window below is a genuine subset", size)
	}

	// A small window near EOF: large enough to contain the last two turns'
	// lines plus part of one padding line, nowhere near offset 0.
	const window = 2048
	start := size - window
	bounded, err := conversation.CollectMessages(p.StreamFrom(path, claudeTailOpts, start, size))
	if err != nil {
		t.Fatalf("StreamFrom(start=%d, size=%d): %v", start, size, err)
	}
	assertTailMatchesFull(t, full, bounded, 2)
}

func TestStreamFrom_DegenerateTailReadsToOffsetZeroButStaysCorrect(t *testing.T) {
	t.Parallel()
	// The qualifying turns sit at the very front, followed by a long run of
	// non-qualifying padding. LoadRecentTurns's growth loop has no choice but
	// to widen all the way to offset 0 here; this test exists to prove that
	// degenerate case still returns the right answer, not a fast one.
	lines := []string{
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "first user turn"),
		claudeAssistantLine("a1", "2026-04-24T19:00:30Z", "first assistant turn"),
	}
	filler := strings.Repeat("f", 4096)
	for i := range 60 {
		lines = append(lines, claudeAttachmentLine(fmt.Sprintf("pad-%d", i), fmt.Sprintf("2026-04-24T19:01:%02dZ", i%60), filler))
	}
	path := writeClaudeTranscript(t, lines)

	full, recent := loadFullAndRecent(t, path, claudeTailOpts, 2)
	if len(full) != 2 {
		t.Fatalf("full qualifying messages = %d, want 2", len(full))
	}
	assertTailMatchesFull(t, full, recent, 2)
}

func TestLoadRecentTurns_MatchesStream_NormalTranscript(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 10 {
		lines = append(lines,
			claudeUserLine(fmt.Sprintf("u%d", i), fmt.Sprintf("2026-04-24T19:%02d:00Z", i), fmt.Sprintf("user turn %d", i)),
			claudeAssistantLine(fmt.Sprintf("a%d", i), fmt.Sprintf("2026-04-24T19:%02d:30Z", i), fmt.Sprintf("assistant turn %d", i)),
		)
	}
	path := writeClaudeTranscript(t, lines)

	for _, turnBudget := range []int{1, 2, 4, 20} {
		full, recent := loadFullAndRecent(t, path, claudeTailOpts, turnBudget)
		assertTailMatchesFull(t, full, recent, turnBudget)
	}
}

func TestLoadRecentTurns_MatchesStream_ToolOnlyAssistantTurnQualifies(t *testing.T) {
	t.Parallel()
	lines := []string{
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "first user turn"),
		claudeAssistantToolOnlyLine("a1", "2026-04-24T19:00:30Z"),
	}
	path := writeClaudeTranscript(t, lines)

	full, recent := loadFullAndRecent(t, path, claudeTailOpts, 2)
	if len(full) != 2 {
		t.Fatalf("full qualifying messages = %d, want 2 (a tool_use-only assistant turn must still qualify)", len(full))
	}
	assertTailMatchesFull(t, full, recent, 2)
	if !recent[len(recent)-1].HasTools {
		t.Fatalf("last tail message HasTools = false, want true")
	}
}

func TestLoadRecentTurns_MatchesStream_TrailingToolAndMetadataLines(t *testing.T) {
	t.Parallel()
	lines := []string{
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "first user turn"),
		claudeAssistantLine("a1", "2026-04-24T19:00:30Z", "first assistant turn"),
		claudeUserLine("u2", "2026-04-24T19:01:00Z", "second user turn"),
		claudeAssistantLine("a2", "2026-04-24T19:01:30Z", "second assistant turn"),
		// Everything below this line is a tool-result, metadata, or corrupt
		// record, not a chat turn, and must not count toward the tail.
		claudeUserToolResultOnlyLine("u3", "2026-04-24T19:02:01Z"),
		claudeSystemCompactBoundaryLine("s1", "2026-04-24T19:02:02Z"),
		"",
		"not json at all",
	}
	path := writeClaudeTranscript(t, lines)

	full, recent := loadFullAndRecent(t, path, claudeTailOpts, 2)
	if len(full) != 4 {
		t.Fatalf("full qualifying messages = %d, want 4 (trailing tool/metadata lines must not qualify)", len(full))
	}
	assertTailMatchesFull(t, full, recent, 2)
	if got := recent[len(recent)-1].Text; got != "second assistant turn" {
		t.Fatalf("last tail message text = %q, want %q", got, "second assistant turn")
	}
}

func TestLoadRecentTurns_MatchesStream_ShorterThanBudget(t *testing.T) {
	t.Parallel()
	lines := []string{
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "only user turn"),
		claudeAssistantLine("a1", "2026-04-24T19:00:30Z", "only assistant turn"),
	}
	path := writeClaudeTranscript(t, lines)

	full, recent := loadFullAndRecent(t, path, claudeTailOpts, 4)
	if len(full) != 2 {
		t.Fatalf("full qualifying messages = %d, want 2", len(full))
	}
	assertTailMatchesFull(t, full, recent, 4)
	if len(recent) != 2 {
		t.Fatalf("tail length = %d, want 2 (transcript is shorter than the turn budget)", len(recent))
	}
}

func TestLoadRecentTurns_MatchesStream_NoQualifyingMessages(t *testing.T) {
	t.Parallel()
	lines := []string{
		claudeUserToolResultOnlyLine("u1", "2026-04-24T19:00:00Z"),
		claudeSystemCompactBoundaryLine("s1", "2026-04-24T19:00:01Z"),
		claudeAttachmentLine("att1", "2026-04-24T19:00:02Z", "irrelevant attachment payload"),
	}
	path := writeClaudeTranscript(t, lines)

	full, recent := loadFullAndRecent(t, path, claudeTailOpts, 4)
	if len(full) != 0 {
		t.Fatalf("full qualifying messages = %d, want 0", len(full))
	}
	if len(recent) != 0 {
		t.Fatalf("tail messages = %d, want 0", len(recent))
	}
}

func TestLoadRecentTurns_MatchesStream_CompactionBoundary(t *testing.T) {
	t.Parallel()
	lines := []string{
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "turn before compaction 1"),
		claudeAssistantLine("a1", "2026-04-24T19:00:30Z", "turn before compaction 2"),
		claudeSystemCompactBoundaryLine("s1", "2026-04-24T19:01:00Z"),
		claudeCompactSummaryUserLine("u2", "2026-04-24T19:01:01Z", "compacted summary of prior turns"),
		claudeAssistantLine("a2", "2026-04-24T19:01:30Z", "turn after compaction 1"),
		claudeUserLine("u3", "2026-04-24T19:02:00Z", "turn after compaction 2"),
	}
	path := writeClaudeTranscript(t, lines)

	for _, turnBudget := range []int{1, 3, 4} {
		full, recent := loadFullAndRecent(t, path, claudeTailOpts, turnBudget)
		assertTailMatchesFull(t, full, recent, turnBudget)
	}
	// The five qualifying turns in file order are: u1, a1, u2(summary), a2,
	// u3. turnBudget=3 must reach back across the compaction boundary and
	// include the compaction summary message itself, since recentTurns does
	// not special-case Compaction != nil.
	full, recent := loadFullAndRecent(t, path, claudeTailOpts, 3)
	want := lastMessages(full, 3)
	if want[0].Text != "compacted summary of prior turns" {
		t.Fatalf("want[0].Text = %q, want the compaction summary text", want[0].Text)
	}
	assertTailMatchesFull(t, full, recent, 3)
}

func TestStreamFrom_UnsupportedWithIncludeToolOutputs(t *testing.T) {
	t.Parallel()
	path := writeClaudeTranscript(t, []string{
		claudeUserLine("u1", "2026-04-24T19:00:00Z", "turn"),
	})
	p := New()
	size, ok := p.TailSize(path)
	if !ok {
		t.Fatalf("TailSize reported ok=false")
	}
	_, err := conversation.CollectMessages(p.StreamFrom(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	}, 0, size))
	if err == nil {
		t.Fatalf("StreamFrom returned nil error for IncludeToolOutputs, want an error since a bounded range cannot attach a tool result from outside it")
	}
}
