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
	// LineTypeTurnEnded is the line type Cursor writes for turn-end events.
	LineTypeTurnEnded = "turn_ended"
	// TurnStatusSuccess is the status string Cursor writes for successful turns.
	TurnStatusSuccess = "success"
	// TurnStatusError is the status string Cursor writes for failed turns.
	TurnStatusError = "error"
)

// ErrStopStreaming lets callers stop StreamMessages without treating the
// callback error as a real transcript failure.
var ErrStopStreaming = errors.New("stop cursor streaming")

// TranscriptMessage is one user or assistant message from a Cursor transcript.
type TranscriptMessage struct {
	Role  string
	Parts []ContentPart
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
		ConversationID: conversationIDFromPath(path),
		FirstUserText:  "",
		HasMessages:    false,
	}
	reader := bufio.NewReader(file)
	lineNumber := 0
	for {
		rawLine, readErr := reader.ReadBytes('\n')
		lineNumber++
		message, ok, decodeErr := decodeMessageLine(path, lineNumber, rawLine)
		if decodeErr != nil {
			ok = false
		}
		if ok {
			header.HasMessages = true
			if message.Role == RoleUser {
				firstText := firstTextPart(message.Parts)
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

// StreamMessages reads a Cursor transcript and calls fn for each chat message.
func StreamMessages(path string, fn func(TranscriptMessage) error) error {
	file, err := os.Open(path)
	if err != nil {
		slog.Warn("providers.cursor.jsonl.transcript_open_failed", "concern", concern, "path", path, "err", err)
		return fmt.Errorf("open cursor transcript %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReader(file)
	lineNumber := 0
	for {
		rawLine, readErr := reader.ReadBytes('\n')
		lineNumber++
		message, ok, decodeErr := decodeMessageLine(path, lineNumber, rawLine)
		if decodeErr != nil {
			ok = false
		}
		if ok {
			if err := fn(message); err != nil {
				if errors.Is(err, ErrStopStreaming) {
					return err
				}
				slog.Warn("providers.cursor.jsonl.transcript_stream_failed", "concern", concern, "path", path, "line", lineNumber, "err", err)
				return fmt.Errorf("stream cursor transcript %s line %d: %w", path, lineNumber, err)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			slog.Warn("providers.cursor.jsonl.transcript_read_failed", "concern", concern, "path", path, "line", lineNumber, "err", readErr)
			return fmt.Errorf("read cursor transcript %s line %d: %w", path, lineNumber, readErr)
		}
	}
}

func decodeMessageLine(path string, lineNumber int, rawBytes []byte) (TranscriptMessage, bool, error) {
	line := bytes.TrimSpace(rawBytes)
	if len(line) == 0 {
		return emptyTranscriptMessage(), false, nil
	}

	var envelope rawLine
	if err := json.Unmarshal(line, &envelope); err != nil {
		return emptyTranscriptMessage(), false, DecodeError{Path: path, Line: lineNumber, Err: err}
	}
	if envelope.Type == LineTypeTurnEnded {
		return emptyTranscriptMessage(), false, nil
	}
	if envelope.Role != RoleUser && envelope.Role != RoleAssistant {
		return emptyTranscriptMessage(), false, nil
	}

	var message rawMessage
	if err := json.Unmarshal(envelope.Message, &message); err != nil {
		return emptyTranscriptMessage(), false, DecodeError{Path: path, Line: lineNumber, Err: err}
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
	return TranscriptMessage{
		Role:  envelope.Role,
		Parts: parts,
	}, true, nil
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
		Role:  "",
		Parts: nil,
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
	Type    string          `json:"type"`
	Status  string          `json:"status"`
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
