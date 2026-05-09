package daemon

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/transcript"
)

func buildSessionExport(sess *session.Session, req *clydev1.ExportSessionRequest) ([]byte, error) {
	if sess == nil {
		return nil, fmt.Errorf("nil session")
	}
	if strings.TrimSpace(sess.Metadata.ProviderTranscriptPath()) == "" {
		return nil, fmt.Errorf("session has no transcript path")
	}
	messages, err := loadSessionExportMessages(sess, req.GetIncludeSystemPrompts(), req.GetIncludeToolOutputs())
	if err != nil {
		return nil, err
	}
	if history, historyErr := loadExportHistoryMessages(sess, req); historyErr == nil && len(history) > 0 {
		messages = append(history, messages...)
	}
	messages = filterExportMessages(messages, req)
	opts := transcript.ShapeOptions{
		ConversationOnly: true,
		ToolOnly:         transcript.ToolOnlyOmit,
		IncludeThinking:  req.GetIncludeThinking(),
	}
	if req.GetIncludeToolCalls() {
		opts.ConversationOnly = false
		opts.ToolOnly = transcript.ToolOnlyFullDetail
	}
	if req.GetFormat() == clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_JSON && !req.GetIncludeRawJsonMetadata() {
		clearExportMetadata(messages)
	}
	body, err := renderExportMessages(messages, opts, req.GetFormat())
	if err != nil {
		return nil, err
	}
	return compressExportWhitespace(body, req.GetFormat(), req.GetWhitespaceCompression()), nil
}

func renderExportMessages(messages []transcript.Message, opts transcript.ShapeOptions, format clydev1.SessionExportFormat) ([]byte, error) {
	switch format {
	case clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_HTML:
		return []byte(transcript.RenderHTMLWithOptions(messages, opts)), nil
	case clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_JSON:
		return transcript.RenderJSONWithOptions(messages, opts)
	case clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_PLAIN_TEXT:
		return []byte(transcript.RenderPlainTextWithOptions(messages, opts)), nil
	case clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_MARKDOWN,
		clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_UNSPECIFIED:
		fallthrough
	default:
		return []byte(transcript.RenderMarkdownWithOptions(messages, opts)), nil
	}
}

func loadExportMessagesFromPath(path string, includeSystemPrompts bool, includeToolOutputs bool) ([]transcript.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return loadExportMessages(f, includeSystemPrompts, includeToolOutputs)
}

func loadExportMessages(r io.Reader, includeSystemPrompts bool, includeToolOutputs bool) ([]transcript.Message, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	messages, err := transcript.ParseWithOptions(bytes.NewReader(body), transcript.ParseOptions{
		PreserveSystemPrompts: includeSystemPrompts,
	})
	if err != nil {
		return nil, err
	}
	if includeToolOutputs {
		attachExportToolOutputs(body, messages)
	}
	return messages, nil
}

func loadExportHistoryMessages(sess *session.Session, req *clydev1.ExportSessionRequest) ([]transcript.Message, error) {
	if sess == nil || sess.Metadata.ProviderSessionID() == "" || req.GetHistoryStart() < 0 {
		return nil, nil
	}
	entries, err := compactengine.ReadLedger(sess.Metadata.ProviderSessionID())
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	if int(req.GetHistoryStart()) >= len(entries) {
		return nil, nil
	}
	entry := entries[req.GetHistoryStart()]
	if entry.SnapshotPath == "" {
		return nil, nil
	}
	f, err := os.Open(entry.SnapshotPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	return loadExportMessages(gz, req.GetIncludeSystemPrompts(), req.GetIncludeToolOutputs())
}

func loadSessionExportMessages(sess *session.Session, includeSystemPrompts bool, includeToolOutputs bool) ([]transcript.Message, error) {
	if sess == nil {
		return nil, fmt.Errorf("nil session")
	}
	path := strings.TrimSpace(sess.Metadata.ProviderTranscriptPath())
	if path == "" {
		return nil, fmt.Errorf("session has no transcript path")
	}
	if sess.ProviderID() == session.ProviderCodex {
		return loadCodexExportMessages(path)
	}
	return loadExportMessagesFromPath(path, includeSystemPrompts, includeToolOutputs)
}

func loadCodexExportMessages(path string) ([]transcript.Message, error) {
	history, err := transcript.ReadCodexHistory(path)
	if err != nil {
		slog.Warn("daemon.session_export.codex_read_failed",
			"path", path,
			"err", err,
		)
		return nil, fmt.Errorf("read codex export history: %w", err)
	}
	turns := history.ConversationTurns(transcript.ShapeOptions{
		IncludeThinking:  false,
		ConversationOnly: true,
		ToolOnly:         transcript.ToolOnlyOmit,
		MaxTextRunes:     0,
	})
	out := make([]transcript.Message, 0, len(turns))
	for _, turn := range turns {
		text := strings.TrimSpace(turn.Text)
		if text == "" {
			continue
		}
		out = append(out, transcript.Message{
			UUID:      "",
			Role:      turn.Role,
			Timestamp: turn.Timestamp,
			Text:      text,
			Thinking:  "",
			HasTools:  false,
			Tools:     nil,
		})
	}
	return out, nil
}

func clearExportMetadata(messages []transcript.Message) {
	for i := range messages {
		messages[i].UUID = ""
		messages[i].Timestamp = time.Time{}
	}
}

func filterExportMessages(messages []transcript.Message, req *clydev1.ExportSessionRequest) []transcript.Message {
	out := make([]transcript.Message, 0, len(messages))
	for _, msg := range messages {
		hasChatText := strings.TrimSpace(msg.Text) != ""
		if !req.GetIncludeChat() && hasChatText {
			msg.Text = ""
		}
		if !req.GetIncludeThinking() {
			msg.Thinking = ""
		}
		if !req.GetIncludeToolCalls() {
			msg.HasTools = false
			msg.Tools = nil
		}
		if strings.TrimSpace(msg.Text) == "" && strings.TrimSpace(msg.Thinking) == "" && !msg.HasTools {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func compressExportWhitespace(body []byte, format clydev1.SessionExportFormat, mode clydev1.SessionExportWhitespaceCompression) []byte {
	if mode == clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_UNSPECIFIED ||
		mode == clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_PRESERVE {
		return body
	}
	if format == clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_JSON {
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, body); err == nil {
			return compacted.Bytes()
		}
		return body
	}
	text := compressExportWhitespaceText(string(body), mode)
	return []byte(text)
}

func compressExportWhitespaceText(text string, mode clydev1.SessionExportWhitespaceCompression) string {
	if mode == clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_PRESERVE ||
		mode == clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_UNSPECIFIED {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			out = append(out, trimmed)
			blank = false
			continue
		}
		if trimmed == "" {
			if mode == clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_DENSE {
				continue
			}
			if !blank {
				out = append(out, "")
				blank = true
			}
			continue
		}
		blank = false
		if inFence || shouldPreserveExportLineWhitespace(line, trimmed) {
			out = append(out, strings.TrimRight(line, " \t"))
			continue
		}
		out = append(out, strings.Join(strings.Fields(trimmed), " "))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func shouldPreserveExportLineWhitespace(line, trimmed string) bool {
	if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
		return true
	}
	if strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, ">") ||
		strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "* ") ||
		strings.HasPrefix(trimmed, "+ ") ||
		strings.HasPrefix(trimmed, "|") {
		return true
	}
	if len(trimmed) >= 3 && trimmed[0] >= '0' && trimmed[0] <= '9' {
		for i := 1; i < len(trimmed) && i < 4; i++ {
			if trimmed[i] == '.' && i+1 < len(trimmed) && trimmed[i+1] == ' ' {
				return true
			}
			if trimmed[i] < '0' || trimmed[i] > '9' {
				break
			}
		}
	}
	return false
}

type exportRawEntry struct {
	Type    string           `json:"type"`
	Message exportRawMessage `json:"message"`
}

type exportRawMessage struct {
	Content json.RawMessage `json:"content"`
}

type exportToolResultBlock struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func attachExportToolOutputs(body []byte, messages []transcript.Message) {
	toolsByID := make(map[string]*transcript.ToolCall)
	for mi := range messages {
		for ti := range messages[mi].Tools {
			id := strings.TrimSpace(messages[mi].Tools[ti].ID)
			if id != "" {
				toolsByID[id] = &messages[mi].Tools[ti]
			}
		}
	}
	if len(toolsByID) == 0 {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var entry exportRawEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.Type != "user" {
			continue
		}
		var blocks []exportToolResultBlock
		if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type != "tool_result" {
				continue
			}
			tool := toolsByID[strings.TrimSpace(block.ToolUseID)]
			if tool == nil {
				continue
			}
			tool.Output = exportToolResultText(block.Content)
			tool.IsError = block.IsError
		}
	}
}

func exportToolResultText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, strings.TrimSpace(block.Text))
			}
		}
		return strings.Join(parts, "\n")
	}
	return strings.TrimSpace(string(raw))
}
