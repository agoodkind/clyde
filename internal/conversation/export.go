package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

// Export renders one raw conversation artifact. It collects every message
// because the rendered document spans the whole conversation.
func (idx *Index) Export(record Record, options ExportOptions) ([]byte, error) {
	messages, err := idx.LoadMessages(record, options.IncludeSystemPrompts, options.IncludeToolOutputs)
	if err != nil {
		return nil, err
	}
	messages = filterMessages(messages, options)
	if options.Format == ExportFormatJSON && !options.IncludeRawJSONMetadata {
		clearMetadata(messages)
	}
	body, err := renderMessages(messages, options)
	if err != nil {
		return nil, err
	}
	return compressWhitespace(body, options.Format, options.Whitespace), nil
}

func renderMessages(messages []transcript.Message, options ExportOptions) ([]byte, error) {
	shapeOptions := transcript.ShapeOptions{
		IncludeThinking:  options.IncludeThinking,
		ConversationOnly: !options.IncludeToolCalls,
		MaxTextRunes:     0,
		ToolOnly:         transcript.ToolOnlyOmit,
	}
	if options.IncludeToolCalls {
		shapeOptions.ToolOnly = transcript.ToolOnlyFullDetail
	}
	switch options.Format {
	case ExportFormatHTML:
		return []byte(transcript.RenderHTMLWithOptions(messages, shapeOptions)), nil
	case ExportFormatJSON:
		body, err := transcript.RenderJSONWithOptions(messages, shapeOptions)
		if err != nil {
			slog.Warn("conversation.export.render_json_failed", "concern", "conversation.export", "component", "conversation", "err", err)
			return nil, fmt.Errorf("render json transcript: %w", err)
		}
		return body, nil
	case ExportFormatPlainText:
		return []byte(transcript.RenderPlainTextWithOptions(messages, shapeOptions)), nil
	case ExportFormatMarkdown, "":
		return []byte(transcript.RenderMarkdownWithOptions(messages, shapeOptions)), nil
	default:
		return nil, fmt.Errorf("unsupported export format %q", options.Format)
	}
}

func filterMessages(messages []transcript.Message, options ExportOptions) []transcript.Message {
	out := make([]transcript.Message, 0, len(messages))
	compactionMessages := transcript.CompactionMessages(messages)
	compactionByMetadata := make(map[*transcript.CompactionMetadata]struct{}, len(compactionMessages))
	for _, message := range compactionMessages {
		if message.Compaction == nil {
			continue
		}
		compactionByMetadata[message.Compaction] = struct{}{}
	}
	for i, message := range messages {
		if options.HistoryStart > 0 && i < options.HistoryStart {
			continue
		}
		if !options.IncludeChat && message.Text != "" {
			message.Text = ""
		}
		if !options.IncludeThinking {
			message.Thinking = ""
		}
		if !options.IncludeToolCalls {
			message.Tools = nil
			message.HasTools = false
		}
		_, keepEmptyCompaction := compactionByMetadata[message.Compaction]
		if message.Text == "" && message.Thinking == "" && len(message.Tools) == 0 && !keepEmptyCompaction {
			continue
		}
		out = append(out, message)
	}
	return out
}

func clearMetadata(messages []transcript.Message) {
	for i := range messages {
		messages[i].UUID = ""
	}
}

func compressWhitespace(body []byte, format ExportFormat, mode WhitespaceMode) []byte {
	if mode == "" || mode == WhitespacePreserve {
		return body
	}
	if format == ExportFormatJSON {
		var buf bytes.Buffer
		if err := json.Compact(&buf, body); err == nil {
			return buf.Bytes()
		}
		return body
	}
	return []byte(compressWhitespaceText(string(body), mode))
}

func compressWhitespaceText(text string, mode WhitespaceMode) string {
	if mode == WhitespacePreserve || mode == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasPrefix(strings.TrimSpace(trimmed), "```") {
			inFence = !inFence
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if strings.TrimSpace(trimmed) == "" {
			if mode == WhitespaceDense {
				continue
			}
			if mode == WhitespaceCompact {
				if blank {
					continue
				}
				blank = true
			}
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, trimmed)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}
