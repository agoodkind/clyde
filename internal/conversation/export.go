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
	messages, err := idx.LoadMessagesWithOptions(record, LoadOptions{
		IncludeSystemPrompts:  options.Content.Has(ContentKindSystemPrompts),
		IncludeSystemMessages: options.Content.Has(ContentKindSystemMessages),
		IncludeToolOutputs:    options.Content.Has(ContentKindToolOutputs),
	})
	if err != nil {
		return nil, err
	}
	messages = filterMessages(messages, options)
	if options.Format == ExportFormatJSON && !options.Content.Has(ContentKindRawJSONMetadata) {
		clearMetadata(messages)
	}
	body, err := renderMessages(record, messages, options)
	if err != nil {
		return nil, err
	}
	return compressWhitespace(body, options.Format, options.Whitespace), nil
}

type rawJSONExport struct {
	ConversationID        string                 `json:"conversation_id"`
	Provider              string                 `json:"provider"`
	ArtifactPath          string                 `json:"artifact_path"`
	Messages              []transcript.Message   `json:"messages"`
	CompactionCheckpoints []CompactionCheckpoint `json:"compaction_checkpoints,omitempty"`
}

func renderMessages(record Record, messages []transcript.Message, options ExportOptions) ([]byte, error) {
	includeToolCalls := options.Content.Has(ContentKindToolCalls)
	shapeOptions := transcript.ShapeOptions{
		IncludeThinking:  options.Content.Has(ContentKindThinking),
		ConversationOnly: !includeToolCalls,
		MaxTextRunes:     0,
		ToolOnly:         transcript.ToolOnlyOmit,
	}
	if includeToolCalls {
		shapeOptions.ToolOnly = transcript.ToolOnlyFullDetail
	}
	switch options.Format {
	case ExportFormatHTML:
		return []byte(transcript.RenderHTMLWithOptions(messages, shapeOptions)), nil
	case ExportFormatJSON:
		if options.Content.Has(ContentKindRawJSONMetadata) {
			return renderRawJSON(record, messages, options)
		}
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

func renderRawJSON(record Record, messages []transcript.Message, options ExportOptions) ([]byte, error) {
	document := rawJSONExport{
		ConversationID:        record.ID,
		Provider:              record.Provider.String(),
		ArtifactPath:          record.ArtifactPath,
		Messages:              messages,
		CompactionCheckpoints: nil,
	}
	if options.Content.Has(ContentKindSystemMessages) {
		document.CompactionCheckpoints = CompactionCheckpoints(messages)
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		slog.Warn("conversation.export.render_raw_json_failed", "concern", "conversation.export", "component", "conversation", "err", err)
		return nil, fmt.Errorf("render raw json transcript: %w", err)
	}
	return append(body, '\n'), nil
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
		if !options.Content.Has(ContentKindChat) && message.Text != "" {
			message.Text = ""
		}
		if !options.Content.Has(ContentKindThinking) {
			message.Thinking = ""
		}
		if !options.Content.Has(ContentKindToolCalls) {
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
