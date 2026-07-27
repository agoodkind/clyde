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

// writeCodexRolloutLines writes real-shaped Codex rollout JSONL lines to a
// temp file and returns its path.
func writeCodexRolloutLines(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tail-rollout.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture rollout: %v", err)
	}
	return path
}

func codexUserLine(timestamp string, text string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`,
		timestamp, text,
	)
}

func codexAssistantLine(timestamp string, text string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`,
		timestamp, text,
	)
}

// codexAssistantToolOnlyLine builds a function_call response item. It
// qualifies as a candidate turn (HasTools is true) even though it carries no
// chat text, matching the plain (non-tool-output) stream's existing
// behavior; see TestLoadRecentTurns_MatchesStream_ToolOnlyAssistantTurnQualifies.
func codexAssistantToolOnlyLine(timestamp string, callID string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call","name":"Bash","arguments":"{\"command\":\"echo hi\"}","call_id":%q}}`,
		timestamp, callID,
	)
}

// codexToolOutputLine builds a function_call_output response item. The plain
// stream drops it (it carries no renderable text), so it never qualifies.
func codexToolOutputLine(timestamp string, callID string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":"tool result text"}}`,
		timestamp, callID,
	)
}

// codexCompactedLine builds a compaction-boundary envelope. With
// IncludeSystemMessages unset (the contextsvc call shape) it never qualifies
// as a candidate turn.
func codexCompactedLine(timestamp string, summary string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"compacted","payload":{"message":%q,"replacement_history":[]}}`,
		timestamp, summary,
	)
}

// codexTurnContextLine builds a turn_context envelope. streamMessageFromEnvelope
// drops it unconditionally, regardless of options, making it a reliable large
// non-qualifying filler line.
func codexTurnContextLine(timestamp string, cwd string) string {
	return fmt.Sprintf(
		`{"timestamp":%q,"type":"turn_context","payload":{"cwd":%q}}`,
		timestamp, cwd,
	)
}

var codexTailOpts = conversation.LoadOptions{
	IncludeSystemPrompts:  false,
	IncludeSystemMessages: false,
	IncludeToolOutputs:    false,
}

// codexTestRecord builds a minimal Record over a fixture path so a test can
// drive it through a real conversation.Index.
func codexTestRecord(path string) conversation.Record {
	return conversation.Record{
		ID:            "codex:fixture",
		Provider:      providerid.ProviderCodex,
		NativeID:      "fixture",
		Lineage:       nil,
		Title:         "fixture",
		WorkspaceRoot: "/repo",
		ArtifactPath:  path,
		ArtifactKind:  "rollout",
		Model:         "model",
	}
}

func newCodexTestIndex() *conversation.Index {
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
	idx := newCodexTestIndex()
	recent, err = idx.LoadRecentTurns(codexTestRecord(path), turnBudget, opts)
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
		codexUserLine("2026-05-02T17:00:00.000Z", "first user turn"),
		codexAssistantLine("2026-05-02T17:00:30.000Z", "first assistant turn"),
		codexUserLine("2026-05-02T17:01:00.000Z", "second user turn"),
		codexAssistantLine("2026-05-02T17:01:30.000Z", "second assistant turn"),
	}
	path := writeCodexRolloutLines(t, lines)
	p := New()

	full, err := conversation.CollectMessages(p.Stream(path, codexTailOpts))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	size, ok := p.TailSize(path)
	if !ok {
		t.Fatalf("TailSize reported ok=false for a plain rollout file")
	}
	fromZero, err := conversation.CollectMessages(p.StreamFrom(path, codexTailOpts, 0, size))
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
	largeCWD := strings.Repeat("f", 4096)
	for i := range 40 {
		lines = append(lines, codexTurnContextLine(fmt.Sprintf("2026-05-02T16:%02d:00.000Z", i%60), largeCWD))
	}
	lines = append(lines,
		codexUserLine("2026-05-02T17:00:00.000Z", "last user turn"),
		codexAssistantLine("2026-05-02T17:00:30.000Z", "last assistant turn"),
	)
	path := writeCodexRolloutLines(t, lines)
	p := New()

	full, err := conversation.CollectMessages(p.Stream(path, codexTailOpts))
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
	bounded, err := conversation.CollectMessages(p.StreamFrom(path, codexTailOpts, start, size))
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
		codexUserLine("2026-05-02T17:00:00.000Z", "first user turn"),
		codexAssistantLine("2026-05-02T17:00:30.000Z", "first assistant turn"),
	}
	largeCWD := strings.Repeat("f", 4096)
	for i := range 60 {
		lines = append(lines, codexTurnContextLine(fmt.Sprintf("2026-05-02T17:01:%02d.000Z", i%60), largeCWD))
	}
	path := writeCodexRolloutLines(t, lines)

	full, recent := loadFullAndRecent(t, path, codexTailOpts, 2)
	if len(full) != 2 {
		t.Fatalf("full qualifying messages = %d, want 2", len(full))
	}
	assertTailMatchesFull(t, full, recent, 2)
}

func TestLoadRecentTurns_MatchesStream_NormalRollout(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 10 {
		lines = append(lines,
			codexUserLine(fmt.Sprintf("2026-05-02T17:%02d:00.000Z", i), fmt.Sprintf("user turn %d", i)),
			codexAssistantLine(fmt.Sprintf("2026-05-02T17:%02d:30.000Z", i), fmt.Sprintf("assistant turn %d", i)),
		)
	}
	path := writeCodexRolloutLines(t, lines)

	for _, turnBudget := range []int{1, 2, 4, 20} {
		full, recent := loadFullAndRecent(t, path, codexTailOpts, turnBudget)
		assertTailMatchesFull(t, full, recent, turnBudget)
	}
}

func TestLoadRecentTurns_MatchesStream_ToolOnlyAssistantTurnQualifies(t *testing.T) {
	t.Parallel()
	lines := []string{
		codexUserLine("2026-05-02T17:00:00.000Z", "first user turn"),
		codexAssistantToolOnlyLine("2026-05-02T17:00:30.000Z", "call-1"),
	}
	path := writeCodexRolloutLines(t, lines)

	full, recent := loadFullAndRecent(t, path, codexTailOpts, 2)
	if len(full) != 2 {
		t.Fatalf("full qualifying messages = %d, want 2 (a function_call-only assistant turn must still qualify)", len(full))
	}
	assertTailMatchesFull(t, full, recent, 2)
	if !recent[len(recent)-1].HasTools {
		t.Fatalf("last tail message HasTools = false, want true")
	}
}

func TestLoadRecentTurns_MatchesStream_TrailingToolAndMetadataLines(t *testing.T) {
	t.Parallel()
	lines := []string{
		codexUserLine("2026-05-02T17:00:00.000Z", "first user turn"),
		codexAssistantLine("2026-05-02T17:00:30.000Z", "first assistant turn"),
		codexUserLine("2026-05-02T17:01:00.000Z", "second user turn"),
		codexAssistantLine("2026-05-02T17:01:30.000Z", "second assistant turn"),
		// Everything below this line is a tool-output, metadata, or corrupt
		// record, not a chat turn, and must not count toward the tail.
		codexToolOutputLine("2026-05-02T17:02:01.000Z", "call-1"),
		codexCompactedLine("2026-05-02T17:02:02.000Z", "boundary summary"),
		codexTurnContextLine("2026-05-02T17:02:03.000Z", "/repo/tail"),
		"",
		"not json at all",
	}
	path := writeCodexRolloutLines(t, lines)

	full, recent := loadFullAndRecent(t, path, codexTailOpts, 2)
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
		codexUserLine("2026-05-02T17:00:00.000Z", "only user turn"),
		codexAssistantLine("2026-05-02T17:00:30.000Z", "only assistant turn"),
	}
	path := writeCodexRolloutLines(t, lines)

	full, recent := loadFullAndRecent(t, path, codexTailOpts, 4)
	if len(full) != 2 {
		t.Fatalf("full qualifying messages = %d, want 2", len(full))
	}
	assertTailMatchesFull(t, full, recent, 4)
	if len(recent) != 2 {
		t.Fatalf("tail length = %d, want 2 (rollout is shorter than the turn budget)", len(recent))
	}
}

func TestLoadRecentTurns_MatchesStream_NoQualifyingMessages(t *testing.T) {
	t.Parallel()
	lines := []string{
		codexToolOutputLine("2026-05-02T17:00:00.000Z", "call-1"),
		codexCompactedLine("2026-05-02T17:00:01.000Z", "boundary summary"),
		codexTurnContextLine("2026-05-02T17:00:02.000Z", "/repo/none"),
	}
	path := writeCodexRolloutLines(t, lines)

	full, recent := loadFullAndRecent(t, path, codexTailOpts, 4)
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
		codexUserLine("2026-05-02T17:00:00.000Z", "turn before compaction 1"),
		codexAssistantLine("2026-05-02T17:00:30.000Z", "turn before compaction 2"),
		codexCompactedLine("2026-05-02T17:01:00.000Z", "compacted summary of prior turns"),
		codexAssistantLine("2026-05-02T17:01:30.000Z", "turn after compaction 1"),
		codexUserLine("2026-05-02T17:02:00.000Z", "turn after compaction 2"),
	}
	path := writeCodexRolloutLines(t, lines)

	for _, turnBudget := range []int{1, 3, 4} {
		full, recent := loadFullAndRecent(t, path, codexTailOpts, turnBudget)
		assertTailMatchesFull(t, full, recent, turnBudget)
	}
}

func TestStreamFrom_LargeCompactedEnvelopeNearOffsetDoesNotBufferWhole(t *testing.T) {
	t.Parallel()
	// A large compacted envelope sits right where the bounded window's start
	// offset lands. skipLargeCompactedLine only special-cases the forward
	// StreamMessages loop's own reads; StreamMessagesFrom's discard step uses
	// drainLine for the same reason, so this must not error or hang.
	largeSummary := strings.Repeat("s", 6*4096)
	lines := []string{
		codexUserLine("2026-05-02T17:00:00.000Z", "before the large compacted line"),
		codexCompactedLine("2026-05-02T17:00:30.000Z", largeSummary),
		codexUserLine("2026-05-02T17:01:00.000Z", "after the large compacted line 1"),
		codexAssistantLine("2026-05-02T17:01:30.000Z", "after the large compacted line 2"),
	}
	path := writeCodexRolloutLines(t, lines)

	full, recent := loadFullAndRecent(t, path, codexTailOpts, 2)
	if len(full) != 3 {
		t.Fatalf("full qualifying messages = %d, want 3 (the compacted line itself never qualifies)", len(full))
	}
	assertTailMatchesFull(t, full, recent, 2)
}

func TestStreamFrom_UnsupportedWithIncludeToolOutputs(t *testing.T) {
	t.Parallel()
	path := writeCodexRolloutLines(t, []string{
		codexUserLine("2026-05-02T17:00:00.000Z", "turn"),
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
