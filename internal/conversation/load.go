package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/transcript"
)

// LoadMessages reads the provider artifact and returns generic messages.
func LoadMessages(record Record, includeSystemPrompts bool, includeToolOutputs bool) ([]transcript.Message, error) {
	switch record.Provider {
	case ProviderCodex:
		return loadCodexMessages(record.ArtifactPath)
	case ProviderClaude, ProviderArtifact:
		return loadClaudeMessages(record.ArtifactPath, includeSystemPrompts, includeToolOutputs)
	default:
		return nil, fmt.Errorf("unsupported conversation provider %q", record.Provider)
	}
}

func loadClaudeMessages(path string, includeSystemPrompts bool, includeToolOutputs bool) ([]transcript.Message, error) {
	file, err := os.Open(path)
	if err != nil {
		slog.Warn("conversation.load.open_failed", "concern", "conversation.load", "component", "conversation", "path", path, "err", err)
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()
	messages, err := transcript.ParseWithOptions(file, transcript.ParseOptions{
		PreserveSystemPrompts: includeSystemPrompts,
	})
	if err != nil {
		slog.Warn("conversation.load.parse_failed", "concern", "conversation.load", "component", "conversation", "path", path, "err", err)
		return nil, fmt.Errorf("parse transcript: %w", err)
	}
	if includeToolOutputs {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			slog.Warn("conversation.load.tool_output_read_failed", "concern", "conversation.load", "component", "conversation", "path", path, "err", readErr)
			return nil, fmt.Errorf("read transcript for tool outputs: %w", readErr)
		}
		attachToolOutputs(body, messages)
	}
	return messages, nil
}

func loadCodexMessages(path string) ([]transcript.Message, error) {
	history, err := transcript.ReadCodexHistory(path)
	if err != nil {
		slog.Warn("conversation.load.codex_read_failed", "concern", "conversation.load", "component", "conversation", "path", path, "err", err)
		return nil, fmt.Errorf("read codex rollout: %w", err)
	}
	turns := history.ConversationTurns(transcript.ShapeOptions{
		IncludeThinking:  false,
		ConversationOnly: true,
		MaxTextRunes:     0,
		ToolOnly:         transcript.ToolOnlyOmit,
	})
	out := make([]transcript.Message, 0, len(turns))
	for _, turn := range turns {
		out = append(out, transcript.Message{
			UUID:      "",
			Role:      turn.Role,
			Timestamp: turn.Timestamp,
			Text:      turn.Text,
			Thinking:  "",
			HasTools:  false,
			Tools:     nil,
		})
	}
	return out, nil
}

func attachToolOutputs(body []byte, messages []transcript.Message) {
	toolsByID := make(map[string]*transcript.ToolCall)
	for i := range messages {
		for j := range messages[i].Tools {
			tool := &messages[i].Tools[j]
			if tool.ID != "" {
				toolsByID[tool.ID] = tool
			}
		}
	}
	if len(toolsByID) == 0 {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		attachToolOutputsFromLine(scanner.Bytes(), toolsByID)
	}
}

func attachToolOutputsFromLine(line []byte, toolsByID map[string]*transcript.ToolCall) {
	var entry toolResultEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return
	}
	if entry.Type != "user" || len(entry.Message.Content) == 0 {
		return
	}
	var blocks []toolResultBlock
	if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
		return
	}
	for _, block := range blocks {
		tool := toolsByID[block.ToolUseID]
		if block.Type != "tool_result" || tool == nil {
			continue
		}
		tool.Output = block.Content
		tool.IsError = block.IsError
	}
}

type toolResultEntry struct {
	Type    string            `json:"type"`
	Message toolResultMessage `json:"message"`
}

type toolResultMessage struct {
	Content json.RawMessage `json:"content"`
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}
