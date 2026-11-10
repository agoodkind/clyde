package cursorjsonl

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// captureWarnings redirects slog to a buffer for the duration of one test, so a
// test can assert that an unreadable line is reported rather than dropped.
func captureWarnings(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()

	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

// TestStreamMessagesReportsUnreadableLine covers finding 3. A truncated line is
// skipped silently today, which under turn grouping also merges the records on
// either side of it with no trace anywhere.
func TestStreamMessagesReportsUnreadableLine(t *testing.T) {
	buffer := captureWarnings(t, slog.LevelWarn)

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, strings.Join([]string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"before"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"after"}]}}`,
	}, "\n")+"\n")

	err := StreamMessages(path, func(_ TranscriptMessage) error { return nil })
	if err != nil {
		t.Fatalf("StreamMessages returned error: %v", err)
	}

	logged := buffer.String()
	if !strings.Contains(logged, "transcript_line_undecodable") {
		t.Fatalf("logs = %q, want an undecodable-line warning", logged)
	}
	if !strings.Contains(logged, `"line":2`) {
		t.Fatalf("logs = %q, want the failing line number 2", logged)
	}
}

// TestScanHeaderReportsUnreadableLine covers the same silent drop on the header
// scan, which shares the decoder.
func TestScanHeaderReportsUnreadableLine(t *testing.T) {
	buffer := captureWarnings(t, slog.LevelWarn)

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text",`,
		`{"role":"user","message":{"content":[{"type":"text","text":"real title"}]}}`,
	}, "\n")+"\n")

	header, err := ScanHeader(path)
	if err != nil {
		t.Fatalf("ScanHeader returned error: %v", err)
	}
	if header.FirstUserText != "real title" {
		t.Fatalf("FirstUserText = %q, want real title", header.FirstUserText)
	}
	if !strings.Contains(buffer.String(), "transcript_line_undecodable") {
		t.Fatalf("logs = %q, want an undecodable-line warning", buffer.String())
	}
}

// TestStreamMessagesTreatsKnownEventAsBoundaryEvenWithRole covers finding 6. A
// line carrying both a role and a known event type must still close the turn,
// which is what the pre-change type-first guard did.
func TestStreamMessagesTreatsKnownEventAsBoundaryEvenWithRole(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"first a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"first b"}]}}`,
		`{"type":"turn_ended","status":"success","role":"assistant"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second a"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second b"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2; a known event closes the turn even carrying a role: %#v", len(turns), turns)
	}
	if got := turnTexts(turns[0]); len(got) != 2 || got[0] != "first a" {
		t.Fatalf("first turn texts = %#v", got)
	}
	if got := turnTexts(turns[1]); len(got) != 2 || got[0] != "second a" {
		t.Fatalf("second turn texts = %#v", got)
	}
	// The outcome is what separates "clyde read the event" from "clyde could not
	// place the line". Both close the turn and produce the same texts, so without
	// this the test cannot tell them apart.
	if turns[0].Outcome.Uncertain || turns[0].Outcome.Status != TurnStatusSuccess {
		t.Fatalf("first turn outcome = %#v, want the success the event stated", turns[0].Outcome)
	}
}

// TestStreamMessagesReportsTheTurnsOwnLineOnCallbackFailure covers finding 7.
// The reported line named where the turn closed, which for a final turn is a
// line past the end of the file.
func TestStreamMessagesReportsTheTurnsOwnLineOnCallbackFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"ask"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"one"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"two"}]}}`,
	}, "\n")+"\n")

	seen := 0
	err := StreamMessages(path, func(_ TranscriptMessage) error {
		seen++
		if seen == 2 {
			return errors.New("callback refused the final turn")
		}
		return nil
	})
	if err == nil {
		t.Fatal("StreamMessages returned nil error, want the callback failure")
	}
	// The final turn is built from lines 2 and 3; the file has 3 lines.
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error = %q, want it to name line 2, the turn's own first line", err.Error())
	}
	if strings.Contains(err.Error(), "line 4") {
		t.Fatalf("error = %q, names a line past the end of the file", err.Error())
	}
}

// TestStreamMessagesLogsAbnormalBoundaryEvenWhenCallerStops covers finding 10.
// The abnormal-boundary log is the only place a transcript records that a turn
// was cut short, and it ran only after delivery succeeded.
func TestStreamMessagesLogsAbnormalBoundaryEvenWhenCallerStops(t *testing.T) {
	buffer := captureWarnings(t, slog.LevelDebug)

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, strings.Join([]string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"cut short"}]}}`,
		`{"type":"turn_ended","status":"aborted","error":"stopped by user"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"never reached"}]}}`,
	}, "\n")+"\n")

	err := StreamMessages(path, func(_ TranscriptMessage) error { return ErrStopStreaming })
	if !errors.Is(err, ErrStopStreaming) {
		t.Fatalf("StreamMessages returned %v, want ErrStopStreaming", err)
	}
	logged := buffer.String()
	if !strings.Contains(logged, "turn_ended_abnormally") {
		t.Fatalf("logs = %q, want the abnormal-boundary record even though the caller stopped", logged)
	}
	if !strings.Contains(logged, "aborted") {
		t.Fatalf("logs = %q, want the aborted status", logged)
	}
}

// failingReader replays a prefix and then fails, which is what a transcript
// being rotated or truncated under a live read looks like.
type failingReader struct {
	body   string
	offset int
	err    error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.body) {
		return 0, r.err
	}
	n := copy(p, r.body[r.offset:])
	r.offset += n
	return n, nil
}

// TestStreamTurnsDeliversOpenTurnOnReadFailure covers finding 5. A mid-file read
// error discarded every already-decoded record of the open turn, where the
// pre-change reader had already delivered each of them.
func TestStreamTurnsDeliversOpenTurnOnReadFailure(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"ask"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"kept one"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"kept two"}]}}`,
	}, "\n") + "\n"
	source := &failingReader{body: body, offset: 0, err: errors.New("input/output error")}

	turns := make([]TranscriptMessage, 0)
	err := streamTurns("cursor-transcript.jsonl", source, func(turn TranscriptMessage) error {
		turns = append(turns, turn)
		return nil
	})

	if err == nil {
		t.Fatal("streamTurns returned nil error, want the read failure")
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2; the decoded turns must survive the read failure: %#v", len(turns), turns)
	}
	// The read failed before anything said the turn was over, so the turn is as
	// truncated as one cut short mid-line and must not be stamped certain.
	if !turns[1].Outcome.Uncertain {
		t.Fatalf("flushed turn outcome = %#v, want Uncertain; the read failed mid-file", turns[1].Outcome)
	}
	if got := turnTexts(turns[1]); len(got) != 2 || got[0] != "kept one" || got[1] != "kept two" {
		t.Fatalf("assistant turn texts = %#v, want both already-decoded records", got)
	}
}

// TestStreamMessagesKeepsAllOneRoleTranscriptAsOneTurn covers the gap finding 2
// correctly names. A transcript whose records never change role and carries no
// boundary event has stated no boundary, so it is one turn.
func TestStreamMessagesKeepsAllOneRoleTranscriptAsOneTurn(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"step one"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"alpha","input":{}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"step two"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"beta","input":{}}]}}`,
	})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1; the transcript states no boundary: %#v", len(turns), turns)
	}
	if len(turns[0].Parts) != 4 {
		t.Fatalf("parts = %d, want all 4 in file order", len(turns[0].Parts))
	}
}

// TestStreamMessagesCarriesFailedTurnOutcome covers a turn that ended badly.
// Cursor states the failure on the boundary event, and a turn carrying none of
// it reached every consumer as an ordinary finished answer.
func TestStreamMessagesCarriesFailedTurnOutcome(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"partial answer"}]}}`,
		`{"type":"turn_ended","status":"error","error":"context limit"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"finished answer"}]}}`,
		`{"type":"turn_ended","status":"success"}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2: %#v", len(turns), turns)
	}
	if !turns[0].Outcome.Failed() {
		t.Fatalf("first turn outcome = %#v, want a failure", turns[0].Outcome)
	}
	if turns[0].Outcome.Status != TurnStatusError || turns[0].Outcome.Error != "context limit" {
		t.Fatalf("first turn outcome = %#v, want the status and reason Cursor recorded", turns[0].Outcome)
	}
	if turns[1].Outcome.Failed() {
		t.Fatalf("second turn outcome = %#v, want a clean finish", turns[1].Outcome)
	}
}

// TestStreamMessagesCarriesAbortedTurnOutcome covers the third status Cursor
// writes, which pairs a reason with a non-error status.
func TestStreamMessagesCarriesAbortedTurnOutcome(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"cut off here"}]}}`,
		`{"type":"turn_ended","status":"aborted","error":"stopped by user"}`,
	})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if !turns[0].Outcome.Failed() || turns[0].Outcome.Status != TurnStatusAborted {
		t.Fatalf("outcome = %#v, want an aborted turn", turns[0].Outcome)
	}
}

// TestStreamMessagesClosesTurnOnUnreadableLine covers the merge an unreadable
// line caused. The line could have been the turn_ended event separating two
// turns, so carrying the turn across it asserted that no boundary was there.
func TestStreamMessagesClosesTurnOnUnreadableLine(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"first"}]}}`,
		`{"type":"turn_ended","status":"succ`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second"}]}}`,
	})

	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2; an unreadable line must not merge the turns around it: %#v", len(turns), turns)
	}
	if !turns[0].Outcome.Uncertain {
		t.Fatalf("first turn outcome = %#v, want Uncertain", turns[0].Outcome)
	}
	if got := turnTexts(turns[0]); len(got) != 1 || got[0] != "first" {
		t.Fatalf("first turn texts = %#v, want only the record before the unreadable line", got)
	}
}

// TestStreamMessagesKeepsTurnAcrossBlankLine proves the boundary reasoning does
// not overreach: a blank line carries nothing and separates nothing.
func TestStreamMessagesKeepsTurnAcrossBlankLine(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"text","text":"first"}]}}`,
		``,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second"}]}}`,
	})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1; a blank line is not a boundary: %#v", len(turns), turns)
	}
	if turns[0].Outcome.Uncertain {
		t.Fatalf("outcome = %#v, want certain across a blank line", turns[0].Outcome)
	}
}

// TestScanHeaderMarksTitleUncertainAfterUnreadableRecord covers a title drawn
// from incomplete evidence. The skipped record may have been the real first user
// message, so the header must say its text is only the earliest one it could
// read.
func TestScanHeaderMarksTitleUncertainAfterUnreadableRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the real firs`,
		`{"role":"user","message":{"content":[{"type":"text","text":"follow-up"}]}}`,
	}, "\n")+"\n")

	header, err := ScanHeader(path)
	if err != nil {
		t.Fatalf("ScanHeader returned error: %v", err)
	}
	if header.FirstUserText != "follow-up" {
		t.Fatalf("FirstUserText = %q, want the earliest readable text", header.FirstUserText)
	}
	if !header.FirstUserTextUncertain {
		t.Fatal("FirstUserTextUncertain = false, want true; a record before it could not be read")
	}

	// The flag has to mean something, so a transcript that read cleanly must not
	// carry it.
	cleanPath := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, cleanPath, `{"role":"user","message":{"content":[{"type":"text","text":"the real first"}]}}`+"\n")
	cleanHeader, err := ScanHeader(cleanPath)
	if err != nil {
		t.Fatalf("ScanHeader returned error for the clean transcript: %v", err)
	}
	if cleanHeader.FirstUserTextUncertain {
		t.Fatal("FirstUserTextUncertain = true on a transcript whose every line read cleanly")
	}
}

// TestStreamTurnsReportsReadFailureEvenWhenTheCallerStops covers a read failure
// that reached nobody. The open turn is flushed before the error is reported,
// and when that delivery returned the stop sentinel the function returned it
// immediately, so the warning never ran and the parser swallowed the sentinel as
// a clean early stop.
func TestStreamTurnsReportsReadFailureEvenWhenTheCallerStops(t *testing.T) {
	buffer := captureWarnings(t, slog.LevelWarn)

	body := `{"role":"assistant","message":{"content":[{"type":"text","text":"only turn"}]}}` + "\n"
	source := &failingReader{body: body, offset: 0, err: errors.New("input/output error")}

	err := streamTurns("cursor-transcript.jsonl", source, func(_ TranscriptMessage) error {
		return ErrStopStreaming
	})

	if !errors.Is(err, ErrStopStreaming) {
		t.Fatalf("streamTurns returned %v, want the caller's stop", err)
	}
	if !strings.Contains(buffer.String(), "transcript_read_failed") {
		t.Fatalf("logs = %q, want the read failure recorded even though the caller stopped", buffer.String())
	}
}

// TestStreamMessagesPreservesToolInputAcrossAJoinedTurn is a guard that joining
// records never smears one call's arguments onto another.
func TestStreamMessagesPreservesToolInputAcrossAJoinedTurn(t *testing.T) {
	t.Parallel()

	turns := collectTurns(t, []string{
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"read_file","input":{"path":"first.go"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"read_file","input":{"path":"second.go"}}]}}`,
	})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	wantPaths := []string{"first.go", "second.go"}
	// The part count is asserted before the loop, because ranging over the parts
	// and indexing the expected paths stays green against a builder that folded
	// only the first record and dropped the second call.
	if len(turns[0].Parts) != len(wantPaths) {
		t.Fatalf("parts = %d, want %d; every record's parts must be folded in", len(turns[0].Parts), len(wantPaths))
	}
	for i, part := range turns[0].Parts {
		var input struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(part.ToolInput, &input); err != nil {
			t.Fatalf("part %d input: %v", i, err)
		}
		if input.Path != wantPaths[i] {
			t.Fatalf("part %d path = %q, want %q", i, input.Path, wantPaths[i])
		}
	}
}

// lineThenErrorReader returns a complete final line and an error from the same
// Read, which is what a truncated file without a trailing newline looks like.
type lineThenErrorReader struct {
	body string
	done bool
	err  error
}

func (r *lineThenErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.body)
	return n, r.err
}

// TestStreamTurnsReportsReadFailureDeliveredWithItsLastLine covers the case
// where the failing read also carries a usable line. The line was folded in and
// delivered before the failure was examined, so a caller that stopped on that
// turn returned the stop sentinel and both the error and its warning were
// skipped.
func TestStreamTurnsReportsReadFailureDeliveredWithItsLastLine(t *testing.T) {
	buffer := captureWarnings(t, slog.LevelWarn)

	source := &lineThenErrorReader{
		body: `{"type":"turn_ended","status":"success"}`,
		err:  io.ErrUnexpectedEOF,
	}
	// A turn is already open, so the boundary line on the failing read closes it
	// and the callback stops there.
	turns := 0
	err := streamTurns("cursor-transcript.jsonl", io.MultiReader(
		strings.NewReader(`{"role":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}`+"\n"),
		source,
	), func(_ TranscriptMessage) error {
		turns++
		return ErrStopStreaming
	})

	if !errors.Is(err, ErrStopStreaming) {
		t.Fatalf("streamTurns returned %v, want the caller's stop", err)
	}
	if turns != 1 {
		t.Fatalf("delivered %d turns, want 1", turns)
	}
	if !strings.Contains(buffer.String(), "transcript_read_failed") {
		t.Fatalf("logs = %q, want the read failure recorded; it arrived with a usable line", buffer.String())
	}
}
