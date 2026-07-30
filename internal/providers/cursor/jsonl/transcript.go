package cursorjsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	// RoleUser is the role string Cursor writes for user messages.
	RoleUser = "user"
	// RoleAssistant is the role string Cursor writes for assistant messages.
	RoleAssistant = "assistant"
	// PartTypeText is the content-part type Cursor writes for text.
	PartTypeText = "text"
	// PartTypeToolUse is the content-part type Cursor writes for tool calls.
	PartTypeToolUse = "tool_use"
)

// LineType names the record kind in a Cursor transcript line's "type" field.
// Cursor writes conversation content on lines that carry no type at all, and
// writes a small family of typed events that carry no role and no message. The
// type decides which of a line's fields are populated.
type LineType string

const (
	// LineTypeUnspecified marks a line with no type field, which is a
	// role-bearing conversation record.
	LineTypeUnspecified LineType = ""
	// LineTypeTurnEnded marks the event Cursor writes when a turn finishes.
	LineTypeTurnEnded LineType = "turn_ended"
	// LineTypeError marks a standalone failure event, which carries an error
	// string and no status.
	LineTypeError LineType = "error"
)

// TurnStatus is the outcome Cursor attaches to a turn_ended event. Cursor pairs
// an error string with every status other than success.
type TurnStatus string

const (
	// TurnStatusUnspecified marks an event with no status field.
	TurnStatusUnspecified TurnStatus = ""
	// TurnStatusSuccess marks a turn that finished normally.
	TurnStatusSuccess TurnStatus = "success"
	// TurnStatusError marks a turn that failed.
	TurnStatusError TurnStatus = "error"
	// TurnStatusAborted marks a turn the user or the client stopped.
	TurnStatusAborted TurnStatus = "aborted"
)

// ErrStopStreaming lets callers stop StreamMessages without treating the
// callback error as a real transcript failure.
var ErrStopStreaming = errors.New("stop cursor streaming")

// TranscriptMessage is one conversational turn from a Cursor transcript.
//
// Cursor writes a turn across as many consecutive lines as it takes, one line
// per streamed content block, and records no id, sequence number, or timestamp
// to tie them together. A turn is therefore reconstructed from the two signals
// the artifact does carry: a run of consecutive records sharing one role, ended
// early by a [LineTypeTurnEnded] or [LineTypeError] event when Cursor states the
// turn is over.
//
// Parts holds every content part of the whole turn in the order Cursor wrote
// them, interleaving text and tool calls exactly as they appeared. Consumers
// that flatten a turn into separate text and tool-call fields keep each
// sequence's own order but lose the interleaving between them, so a caller that
// needs to know which sentence preceded which call must read Parts.
type TranscriptMessage struct {
	Role    string
	Parts   []ContentPart
	Outcome TurnOutcome
}

// TurnOutcome is what the transcript says about how a turn ended.
//
// A turn that ended at a role change or at the end of the file carries
// [TurnStatusUnspecified] with Uncertain false, because the artifact states
// nothing about how it ended. A turn closed by a boundary event carries what
// that event said. A turn closed by a line Clyde could not read, or by an event
// kind it does not model, carries Uncertain, because a boundary is there but
// what it says is unknown.
type TurnOutcome struct {
	// Boundary names the event kind that closed the turn, or
	// [LineTypeUnspecified] when the turn ended at a role change or at the end of
	// the file. It is what separates "an event said how this ended" from "nothing
	// said anything", which Status alone cannot express: Cursor writes its
	// standalone error event with a reason and no status at all.
	Boundary  LineType
	Status    TurnStatus
	Error     string
	Uncertain bool
}

// Failed reports whether the transcript recorded this turn as not finishing
// normally.
//
// Success is the allowlist, not failure. Cursor's standalone error event carries
// its reason with no status, so allowlisting the error and aborted statuses read
// that event as a clean finish and dropped its reason, and any status Cursor
// adds later would read as clean for the same reason. An event that did not say
// success did not say the turn finished normally.
func (o TurnOutcome) Failed() bool {
	if o.Boundary == LineTypeUnspecified {
		// Only a role change or the end of the file closed this turn, so the
		// transcript says nothing about how it ended either way.
		return false
	}
	return o.Status != TurnStatusSuccess
}

// Label names how the turn ended for a reader, falling back to the boundary kind
// when the event carried no status, which is how Cursor writes its standalone
// error event.
func (o TurnOutcome) Label() string {
	if o.Status != TurnStatusUnspecified {
		return string(o.Status)
	}
	return string(o.Boundary)
}

func emptyTurnOutcome() TurnOutcome {
	return TurnOutcome{
		Boundary:  LineTypeUnspecified,
		Status:    TurnStatusUnspecified,
		Error:     "",
		Uncertain: false,
	}
}

// ContentPart is one supported Cursor message content part.
type ContentPart struct {
	Type     string
	Text     string
	ToolName string
	// ToolInput is the tool's arbitrary, externally-defined argument object.
	// Cursor stores this object with a tool-specific shape, so Clyde preserves
	// the raw JSON at the smallest file-format boundary.
	ToolInput json.RawMessage
}

// TranscriptHeader is a cheap display summary for one Cursor transcript file.
type TranscriptHeader struct {
	ConversationID string
	FirstUserText  string
	HasMessages    bool
	// FirstUserTextUncertain marks a header whose scan skipped an unreadable
	// record before it settled on FirstUserText. The skipped record may have been
	// the real first user message, so the text is the earliest one Clyde could
	// read rather than the earliest one the conversation has.
	FirstUserTextUncertain bool
}

// DecodeError reports a malformed transcript line with its file location.
type DecodeError struct {
	Path string
	Line int
	Err  error
}

// Error returns the formatted decode failure.
func (e DecodeError) Error() string {
	return fmt.Sprintf("decode cursor transcript %s line %d: %v", e.Path, e.Line, e.Err)
}

// Unwrap returns the underlying decode failure.
func (e DecodeError) Unwrap() error {
	return e.Err
}

// ScanHeader reads enough of a transcript to find its first user text.
func ScanHeader(path string) (TranscriptHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		slog.Warn("providers.cursor.jsonl.transcript_open_failed", "concern", concern, "path", path, "err", err)
		return TranscriptHeader{}, fmt.Errorf("open cursor transcript %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	header := TranscriptHeader{
		ConversationID:         conversationIDFromPath(path),
		FirstUserText:          "",
		HasMessages:            false,
		FirstUserTextUncertain: false,
	}
	reader := bufio.NewReader(file)
	lineNumber := 0
	for {
		rawLine, readErr := reader.ReadBytes('\n')
		lineNumber++
		record, kind, decodeErr := decodeLine(path, lineNumber, rawLine)
		if decodeErr != nil {
			// The skipped record may have been the conversation's real first user
			// message, so anything the scan settles on afterwards is the earliest text
			// Clyde could read rather than the earliest the conversation has.
			logUndecodableLine(path, lineNumber, decodeErr)
			header.FirstUserTextUncertain = true
			kind = lineKindBlank
		}
		if kind == lineKindRecord && record.Type == LineTypeUnspecified {
			header.HasMessages = true
			if record.Message.Role == RoleUser {
				firstText := firstTextPart(record.Message.Parts)
				if firstText != "" {
					header.FirstUserText = firstText
					return header, nil
				}
			}
		}
		if readErr == io.EOF {
			return header, nil
		}
		if readErr != nil {
			slog.Warn("providers.cursor.jsonl.transcript_read_failed", "concern", concern, "path", path, "line", lineNumber, "err", readErr)
			return TranscriptHeader{}, fmt.Errorf("read cursor transcript %s line %d: %w", path, lineNumber, readErr)
		}
	}
}

// StreamMessages reads a Cursor transcript and calls fn once per reconstructed
// conversational turn, in file order.
//
// Cursor writes one turn across many consecutive lines, so a turn closes only
// when the next record proves it ended: a record with a different role, an
// explicit turn-boundary event, or the end of the file. The reader holds one
// turn in flight rather than the whole artifact, so a caller reading a window
// can stop early with [ErrStopStreaming] without the rest of the file being
// read. A turn is not itself bounded: it is as large as the run of records
// Cursor wrote for it. In the observed corpus the turn with the most parts holds
// 466 of them across about 100 KB, and the largest by bytes is a single part of
// about 650 KB, so neither the part count nor the byte size bounds the other.
func StreamMessages(path string, fn func(TranscriptMessage) error) error {
	file, err := os.Open(path)
	if err != nil {
		slog.Warn("providers.cursor.jsonl.transcript_open_failed", "concern", concern, "path", path, "err", err)
		return fmt.Errorf("open cursor transcript %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	return streamTurns(path, file, fn)
}

// streamTurns is the reader-facing half of [StreamMessages], separated so a test
// can drive it with a source that fails mid-file.
func streamTurns(path string, source io.Reader, fn func(TranscriptMessage) error) error {
	reader := bufio.NewReader(source)
	lineNumber := 0
	var turn turnBuilder

	// deliver reports the turn's own first line rather than the line that closed
	// it, so an operator following the number lands on the turn's content instead
	// of on the boundary event or on a line past the end of the file.
	deliver := func(completed completedTurn) error {
		if err := fn(completed.Message); err != nil {
			if errors.Is(err, ErrStopStreaming) {
				return err
			}
			slog.Warn("providers.cursor.jsonl.transcript_stream_failed",
				"concern", concern,
				"path", path,
				"line", completed.FirstLine,
				"last_line", completed.LastLine,
				"err", err,
			)
			return fmt.Errorf("stream cursor transcript %s line %d: %w", path, completed.FirstLine, err)
		}
		return nil
	}

	for {
		rawLine, readErr := reader.ReadBytes('\n')
		lineNumber++
		// The failure is reported before any delivery, because the same read can
		// return a complete final line and an error together. Reporting it after
		// folding that line in let a caller that stopped on it carry the stop
		// sentinel out of the function with the failure recorded nowhere.
		// The failure is reported before any delivery, because the same read can
		// return a complete final line and an error together. Reporting it after
		// folding that line in let a caller that stopped on it carry the stop
		// sentinel out of the function with the failure recorded nowhere.
		if readErr != nil && readErr != io.EOF {
			slog.Warn("providers.cursor.jsonl.transcript_read_failed", "concern", concern, "path", path, "line", lineNumber, "err", readErr)
		}
		if err := foldLine(path, lineNumber, rawLine, &turn, deliver); err != nil {
			return err
		}
		if readErr == io.EOF {
			// The last turn of the file has no following record to close it.
			return flushOpenTurn(&turn, deliver)
		}
		if readErr != nil {
			// Deliver what was already decoded, so a mid-file read error costs the
			// unread remainder rather than also discarding the records of the turn
			// that was already in hand. The turn is marked uncertain: nothing said it
			// was over, and the same failure landing one byte earlier would have hit
			// mid-line and been marked uncertain, so stamping it certain would report
			// a truncated answer as a finished one.
			if err := closeUncertainTurn(&turn, deliver); err != nil {
				// The caller stopped on that turn. Its stop is what happened, and
				// reporting the read error instead would call the consumer's yield
				// again after it asked to stop, so the warning already emitted above
				// is where this failure surfaces.
				return err
			}
			return fmt.Errorf("read cursor transcript %s line %d: %w", path, lineNumber, readErr)
		}
	}
}

// turnDelivery hands one completed turn to the caller's callback.
type turnDelivery func(completedTurn) error

// foldLine decodes one transcript line and folds it into the open turn,
// delivering whichever turn the line closed.
func foldLine(
	path string,
	lineNumber int,
	rawLine []byte,
	turn *turnBuilder,
	deliver turnDelivery,
) error {
	record, kind, decodeErr := decodeLine(path, lineNumber, rawLine)
	if decodeErr != nil {
		// An unreadable line could have been anything, including the turn_ended
		// event that separates two turns. Carrying the open turn across it would
		// assert that no boundary was there, so the turn closes and is marked
		// uncertain: splitting one turn shows two real fragments, where merging two
		// turns invents a message that never existed.
		logUndecodableLine(path, lineNumber, decodeErr)
		return closeUncertainTurn(turn, deliver)
	}
	switch kind {
	case lineKindBlank:
		// A blank line carries nothing and separates nothing.
		return nil
	case lineKindUnmodeled:
		// Neither conversation nor an event Clyde models. Every typed role-less line
		// in the observed corpus is a turn terminator, so the same reasoning applies
		// as for an unreadable line: treat it as a boundary whose meaning is unknown
		// rather than as proof that no boundary occurred.
		logUnmodeledLine(path, lineNumber)
		return closeUncertainTurn(turn, deliver)
	case lineKindRecord:
		// Fall through to fold the record into the open turn.
	}
	// The boundary is logged before delivery, because a caller that stops on this
	// turn would otherwise lose the only record that it ended badly.
	logAbnormalBoundary(path, lineNumber, record)
	completed, hasCompleted := turn.accept(record, lineNumber)
	if !hasCompleted {
		return nil
	}
	return deliver(completed)
}

// closeUncertainTurn ends the open turn at a line whose meaning Clyde could not
// establish, marking the turn so consumers can see the boundary was not read.
func closeUncertainTurn(turn *turnBuilder, deliver turnDelivery) error {
	completed, hasCompleted := turn.takeWithOutcome(TurnOutcome{
		Boundary:  LineTypeUnspecified,
		Status:    TurnStatusUnspecified,
		Error:     "",
		Uncertain: true,
	})
	if !hasCompleted {
		return nil
	}
	return deliver(completed)
}

// flushOpenTurn delivers the turn still open, if any.
func flushOpenTurn(turn *turnBuilder, deliver turnDelivery) error {
	completed, hasCompleted := turn.take()
	if !hasCompleted {
		return nil
	}
	return deliver(completed)
}

// completedTurn is one reconstructed turn together with the line span it was
// built from, so a failure names the turn's own lines.
type completedTurn struct {
	Message   TranscriptMessage
	FirstLine int
	LastLine  int
}

// turnBuilder accumulates the consecutive records of one turn. It holds only the
// turn currently open, which is what keeps [StreamMessages] lazy.
type turnBuilder struct {
	role      string
	parts     []ContentPart
	firstLine int
	lastLine  int
}

// accept folds one decoded record into the open turn and returns the turn the
// record closed, if any. A conversation record whose role differs from the open
// turn's starts a new turn, and a boundary event closes the open turn outright
// so two runs of one role that Cursor separated stay separate turns.
func (b *turnBuilder) accept(record transcriptRecord, lineNumber int) (completedTurn, bool) {
	if record.Type != LineTypeUnspecified {
		// The event states how the turn ended, so the turn carries that outcome
		// rather than reaching consumers as an ordinary finished answer.
		return b.takeWithOutcome(TurnOutcome{
			Boundary:  record.Type,
			Status:    record.Status,
			Error:     record.Error,
			Uncertain: false,
		})
	}
	completed, hasCompleted := emptyCompletedTurn(), false
	if b.role != "" && b.role != record.Message.Role {
		completed, hasCompleted = b.take()
	}
	if b.role == "" {
		b.firstLine = lineNumber
	}
	b.role = record.Message.Role
	b.parts = append(b.parts, record.Message.Parts...)
	b.lastLine = lineNumber
	return completed, hasCompleted
}

// take returns the open turn with no recorded outcome, which is what a turn
// ending at a role change or at the end of the file has.
func (b *turnBuilder) take() (completedTurn, bool) {
	return b.takeWithOutcome(emptyTurnOutcome())
}

// takeWithOutcome returns the open turn stamped with how it ended, and resets
// the builder. It reports false when no turn is open, so a boundary event with
// nothing before it emits nothing.
func (b *turnBuilder) takeWithOutcome(outcome TurnOutcome) (completedTurn, bool) {
	if b.role == "" {
		return emptyCompletedTurn(), false
	}
	completed := completedTurn{
		Message: TranscriptMessage{
			Role:    b.role,
			Parts:   b.parts,
			Outcome: outcome,
		},
		FirstLine: b.firstLine,
		LastLine:  b.lastLine,
	}
	b.role = ""
	b.parts = nil
	b.firstLine = 0
	b.lastLine = 0
	return completed, true
}

func emptyCompletedTurn() completedTurn {
	return completedTurn{
		Message:   emptyTranscriptMessage(),
		FirstLine: 0,
		LastLine:  0,
	}
}

// logUndecodableLine records a line the decoder could not read. The line's
// content is lost either way; this is the only trace that it existed.
func logUndecodableLine(path string, lineNumber int, err error) {
	slog.Warn("providers.cursor.jsonl.transcript_line_undecodable",
		"concern", concern,
		"path", path,
		"line", lineNumber,
		"err", err,
	)
}

// logUnmodeledLine records a line that decoded but names neither conversation
// nor an event Clyde models, which is how a boundary kind Cursor adds later
// becomes visible instead of quietly changing how turns are grouped.
func logUnmodeledLine(path string, lineNumber int) {
	slog.Warn("providers.cursor.jsonl.transcript_line_unmodeled",
		"concern", concern,
		"path", path,
		"line", lineNumber,
	)
}

// logAbnormalBoundary records a turn that did not end cleanly. Cursor states the
// failure on the boundary event itself, and it is the only place a transcript
// says a turn was cut short rather than completed.
func logAbnormalBoundary(path string, lineNumber int, record transcriptRecord) {
	if record.Type == LineTypeUnspecified || record.Status == TurnStatusSuccess {
		return
	}
	slog.Debug("providers.cursor.jsonl.turn_ended_abnormally",
		"concern", concern,
		"path", path,
		"line", lineNumber,
		"line_type", string(record.Type),
		"status", string(record.Status),
		"err", record.Error,
	)
}

// transcriptRecord is one decoded Cursor transcript line. A line is either a
// role-bearing conversation record, whose Type is [LineTypeUnspecified], or a
// typed turn-boundary event, which carries no role and no message.
type transcriptRecord struct {
	Type    LineType
	Status  TurnStatus
	Error   string
	Message TranscriptMessage
}

// lineKind says what the decoder made of one raw line, so a caller can tell a
// line that carries nothing from one whose meaning it could not establish.
type lineKind uint8

const (
	// lineKindBlank marks an empty line. It carries nothing and bounds nothing.
	lineKindBlank lineKind = iota
	// lineKindRecord marks a line the decoder read into a record.
	lineKindRecord
	// lineKindUnmodeled marks a line that is valid JSON but is neither
	// conversation nor an event Clyde models.
	lineKindUnmodeled
)

func decodeLine(path string, lineNumber int, rawBytes []byte) (transcriptRecord, lineKind, error) {
	line := bytes.TrimSpace(rawBytes)
	if len(line) == 0 {
		return emptyTranscriptRecord(), lineKindBlank, nil
	}

	var envelope rawLine
	if err := json.Unmarshal(line, &envelope); err != nil {
		return emptyTranscriptRecord(), lineKindBlank, DecodeError{Path: path, Line: lineNumber, Err: err}
	}
	// A type Clyde models names a boundary event outright, so it is read as one
	// whatever else the line carries. Cursor writes these with no role today, but
	// keeping the type first means a boundary Cursor states is never lost to the
	// role check.
	if envelope.Type == LineTypeTurnEnded || envelope.Type == LineTypeError {
		return transcriptRecord{
			Type:    envelope.Type,
			Status:  envelope.Status,
			Error:   envelope.Error,
			Message: emptyTranscriptMessage(),
		}, lineKindRecord, nil
	}
	// Otherwise the role decides. Cursor writes conversation content with a role
	// and no type, so a line that carries a role is conversation even under a type
	// Clyde does not model, which keeps a type added to conversation records later
	// from silently dropping every record.
	if envelope.Role != RoleUser && envelope.Role != RoleAssistant {
		return emptyTranscriptRecord(), lineKindUnmodeled, nil
	}

	var message rawMessage
	if err := json.Unmarshal(envelope.Message, &message); err != nil {
		return emptyTranscriptRecord(), lineKindBlank, DecodeError{Path: path, Line: lineNumber, Err: err}
	}

	parts := make([]ContentPart, 0, len(message.Content))
	for _, part := range message.Content {
		switch part.Type {
		case PartTypeText:
			parts = append(parts, ContentPart{
				Type:      PartTypeText,
				Text:      part.Text,
				ToolName:  "",
				ToolInput: nil,
			})
		case PartTypeToolUse:
			parts = append(parts, ContentPart{
				Type:      PartTypeToolUse,
				Text:      "",
				ToolName:  part.Name,
				ToolInput: cloneRawMessage(part.Input),
			})
		default:
			continue
		}
	}
	return transcriptRecord{
		Type:   LineTypeUnspecified,
		Status: TurnStatusUnspecified,
		Error:  "",
		Message: TranscriptMessage{
			Role:    envelope.Role,
			Parts:   parts,
			Outcome: emptyTurnOutcome(),
		},
	}, lineKindRecord, nil
}

func conversationIDFromPath(path string) string {
	parentName := filepath.Base(filepath.Dir(path))
	filenameStem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if parentName == filenameStem {
		return parentName
	}
	return filenameStem
}

func firstTextPart(parts []ContentPart) string {
	for _, part := range parts {
		if part.Type == PartTypeText && strings.TrimSpace(part.Text) != "" {
			return strings.TrimSpace(part.Text)
		}
	}
	return ""
}

func emptyTranscriptMessage() TranscriptMessage {
	return TranscriptMessage{
		Role:    "",
		Parts:   nil,
		Outcome: emptyTurnOutcome(),
	}
}

func emptyTranscriptRecord() transcriptRecord {
	return transcriptRecord{
		Type:    LineTypeUnspecified,
		Status:  TurnStatusUnspecified,
		Error:   "",
		Message: emptyTranscriptMessage(),
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

type rawLine struct {
	Role    string          `json:"role"`
	Type    LineType        `json:"type"`
	Status  TurnStatus      `json:"status"`
	Error   string          `json:"error"`
	Message json.RawMessage `json:"message"`
}

type rawMessage struct {
	Content []rawContentPart `json:"content"`
}

type rawContentPart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}
