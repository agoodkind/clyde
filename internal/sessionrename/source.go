package sessionrename

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// SourceKind names the heuristic that produced a CandidateSource.
// PR4 only emits SourceFirstUserMessage. The LLM source kind is
// reserved for PR5 and is unused in this package.
type SourceKind string

const (
	// SourceFirstUserMessage marks input synthesized from the first
	// user-authored message on the transcript.
	SourceFirstUserMessage SourceKind = "first_user_message"
)

// CandidateSource is the redacted, normalized text the worker passes
// downstream to the namer. Hash is the stable fingerprint used to
// short-circuit the worker when nothing about the source has changed.
type CandidateSource struct {
	Kind          SourceKind
	Text          string
	Hash          string
	UserMsgCount  int
	HasFirstUser  bool
	FirstUserTime time.Time
}

// errNoUsableSource indicates the transcript carried no non-empty
// first user message.
var errNoUsableSource = errors.New("sessionrename: no usable source")

// firstUserMessageMaxRunes caps how much of the first user message
// the namer ever sees. The cap is conservative because the heuristic
// only needs the leading distinctive phrase.
const firstUserMessageMaxRunes = 240

// transcriptHeaderRecord mirrors the subset of the Claude Code
// transcript JSONL header schema this package consumes.
type transcriptHeaderRecord struct {
	Type      string                          `json:"type"`
	Message   transcriptHeaderInnerMessage    `json:"message"`
	Timestamp string                          `json:"timestamp"`
	Content   transcriptHeaderInnerStringList `json:"content"`
}

type transcriptHeaderInnerMessage struct {
	Role    string                          `json:"role"`
	Content transcriptHeaderInnerStringList `json:"content"`
}

// transcriptHeaderInnerStringList accepts both string and structured
// content forms that appear on Claude transcript records. Anthropic
// emits user content as either a bare string or a list of typed
// blocks; we flatten text-shaped blocks and ignore the rest.
type transcriptHeaderInnerStringList struct {
	Text string
}

func (s *transcriptHeaderInnerStringList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		s.Text = ""
		return nil
	}
	if data[0] == '"' {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("sessionrename: unmarshal transcript inner string: %w", err)
		}
		s.Text = raw
		return nil
	}
	if data[0] == '[' {
		var blocks []transcriptHeaderTextBlock
		if err := json.Unmarshal(data, &blocks); err != nil {
			return fmt.Errorf("sessionrename: unmarshal transcript inner blocks: %w", err)
		}
		var b strings.Builder
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(block.Text)
			}
		}
		s.Text = b.String()
		return nil
	}
	return nil
}

type transcriptHeaderTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ExtractFromTranscript reads the transcript at path and returns the
// first three user messages concatenated, trimmed, and redacted as
// the candidate source. The plan caps PR4 at the first user message
// only as the candidate name input; subsequent user messages count
// toward the trigger gate but do not feed the namer.
func ExtractFromTranscript(path string) (CandidateSource, error) {
	if strings.TrimSpace(path) == "" {
		return CandidateSource{}, errNoUsableSource
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("sessionrename.source.open_failed",
			"path", path,
			"err", err,
		)
		return CandidateSource{}, fmt.Errorf("sessionrename: open transcript %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var firstUser string
	var firstUserTime time.Time
	userCount := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record transcriptHeaderRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if record.Type != "user" {
			continue
		}
		text := firstNonEmptyText(record)
		if strings.TrimSpace(text) == "" {
			continue
		}
		userCount++
		if firstUser == "" {
			firstUser = text
			if record.Timestamp != "" {
				if parsed, parseErr := time.Parse(time.RFC3339, record.Timestamp); parseErr == nil {
					firstUserTime = parsed
				}
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		slog.Warn("sessionrename.source.scan_failed",
			"path", path,
			"err", err,
		)
		return CandidateSource{}, fmt.Errorf("sessionrename: scan transcript %q: %w", path, err)
	}

	if firstUser == "" {
		return CandidateSource{}, errNoUsableSource
	}
	text := redactSourceText(truncateRunes(firstUser, firstUserMessageMaxRunes))
	if text == "" {
		return CandidateSource{}, errNoUsableSource
	}
	return CandidateSource{
		Kind:          SourceFirstUserMessage,
		Text:          text,
		Hash:          fingerprint(SourceFirstUserMessage, text),
		UserMsgCount:  userCount,
		HasFirstUser:  true,
		FirstUserTime: firstUserTime,
	}, nil
}

// firstNonEmptyText returns the first inline text Claude attached to
// a user record. The transcript schema places user text either
// directly on the record or nested under message.content.
func firstNonEmptyText(record transcriptHeaderRecord) string {
	if record.Content.Text != "" {
		return record.Content.Text
	}
	if record.Message.Content.Text != "" {
		return record.Message.Content.Text
	}
	return ""
}

// truncateRunes returns the first n runes of s.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for index := range s {
		if count == n {
			return s[:index]
		}
		count++
	}
	return s
}
