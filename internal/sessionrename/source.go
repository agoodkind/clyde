package sessionrename

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/slogger"
	itranscript "goodkind.io/clyde/internal/transcript"
)

type sourceKind string

const (
	sourceCustomTitle      sourceKind = "custom_title"
	sourceFirstUserMessage sourceKind = "first_user_message"
)

type candidateSource struct {
	Kind          sourceKind
	Text          string
	Hash          string
	UserMsgCount  int
	HasFirstUser  bool
	FirstUserTime time.Time
}

var errNoUsableSource = errors.New("sessionrename: no usable source")

const firstUserMessageMaxRunes = 240

type transcriptRecordType string

const (
	transcriptRecordCustomTitle transcriptRecordType = "custom-title"
	transcriptRecordUser        transcriptRecordType = "user"
)

type transcriptHeaderRecord struct {
	Type        transcriptRecordType            `json:"type"`
	CustomTitle string                          `json:"customTitle"`
	Message     transcriptHeaderInnerMessage    `json:"message"`
	Timestamp   string                          `json:"timestamp"`
	Content     transcriptHeaderInnerStringList `json:"content"`
}

type transcriptHeaderInnerMessage struct {
	Role    string                          `json:"role"`
	Content transcriptHeaderInnerStringList `json:"content"`
}

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
			return fmt.Errorf("unmarshal string content: %w", err)
		}
		s.Text = raw
		return nil
	}
	if data[0] == '[' {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &blocks); err != nil {
			return fmt.Errorf("unmarshal block content: %w", err)
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
	}
	return nil
}

func extractFromSession(sess *session.Session) (candidateSource, error) {
	if sess == nil {
		return candidateSource{}, errNoUsableSource
	}
	path := strings.TrimSpace(sess.Metadata.ProviderTranscriptPath())
	if path == "" {
		return candidateSource{}, errNoUsableSource
	}
	if sess.ProviderID() == session.ProviderCodex {
		return extractFromCodexTranscript(path)
	}
	return extractFromClaudeTranscript(path)
}

func extractFromClaudeTranscript(path string) (candidateSource, error) {
	if strings.TrimSpace(path) == "" {
		return candidateSource{}, errNoUsableSource
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("sessionrename.claude_source_open_failed",
			"component", "daemon",
			"subcomponent", "autoname",
			"concern", slogger.ConcernDaemonWorkersPrune,
			"path", path,
			"err", err,
		)
		return candidateSource{}, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var customTitle string
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
		switch record.Type {
		case transcriptRecordCustomTitle:
			if customTitle == "" && strings.TrimSpace(record.CustomTitle) != "" {
				customTitle = strings.TrimSpace(record.CustomTitle)
			}
		case transcriptRecordUser:
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
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		slog.Warn("sessionrename.claude_source_scan_failed",
			"component", "daemon",
			"subcomponent", "autoname",
			"concern", slogger.ConcernDaemonWorkersPrune,
			"path", path,
			"err", err,
		)
		return candidateSource{}, fmt.Errorf("scan transcript: %w", err)
	}

	return candidateFromParts(customTitle, firstUser, userCount, firstUserTime)
}

func extractFromCodexTranscript(path string) (candidateSource, error) {
	history, err := itranscript.ReadCodexHistory(path)
	if err != nil {
		slog.Warn("sessionrename.codex_source_read_failed",
			"component", "daemon",
			"subcomponent", "autoname",
			"concern", slogger.ConcernDaemonWorkersPrune,
			"path", path,
			"err", err,
		)
		return candidateSource{}, fmt.Errorf("read codex transcript: %w", err)
	}
	turns := history.ConversationTurns(itranscript.ShapeOptions{
		IncludeThinking:  false,
		ConversationOnly: true,
		ToolOnly:         "",
		MaxTextRunes:     firstUserMessageMaxRunes,
	})

	var firstUser string
	var firstUserTime time.Time
	userCount := 0
	for _, turn := range turns {
		if turn.Role != "user" {
			continue
		}
		if strings.TrimSpace(turn.Text) == "" {
			continue
		}
		userCount++
		if firstUser == "" {
			firstUser = turn.Text
			firstUserTime = turn.Timestamp
		}
	}

	return candidateFromParts("", firstUser, userCount, firstUserTime)
}

func candidateFromParts(customTitle, firstUser string, userCount int, firstUserTime time.Time) (candidateSource, error) {
	if customTitle != "" {
		text := redactSourceText(customTitle)
		if text == "" {
			return candidateSource{}, errNoUsableSource
		}
		return candidateSource{
			Kind:          sourceCustomTitle,
			Text:          text,
			Hash:          fingerprint(sourceCustomTitle, text),
			UserMsgCount:  userCount,
			HasFirstUser:  firstUser != "",
			FirstUserTime: firstUserTime,
		}, nil
	}
	if firstUser == "" {
		return candidateSource{}, errNoUsableSource
	}
	text := redactSourceText(truncateRunes(firstUser, firstUserMessageMaxRunes))
	if text == "" {
		return candidateSource{}, errNoUsableSource
	}
	return candidateSource{
		Kind:          sourceFirstUserMessage,
		Text:          text,
		Hash:          fingerprint(sourceFirstUserMessage, text),
		UserMsgCount:  userCount,
		HasFirstUser:  true,
		FirstUserTime: firstUserTime,
	}, nil
}

func firstNonEmptyText(record transcriptHeaderRecord) string {
	if record.Content.Text != "" {
		return record.Content.Text
	}
	if record.Message.Content.Text != "" {
		return record.Message.Content.Text
	}
	return ""
}

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

func fingerprint(kind sourceKind, text string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + text))
	return hex.EncodeToString(sum[:8])
}
