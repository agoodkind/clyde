package zedstore

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// ParseThreadDocument decodes one persisted Zed thread payload and upgrades
// legacy shapes into the current thread document model.
func ParseThreadDocument(dataType DataType, data []byte) (ThreadDocument, error) {
	jsonData, err := DecodeThreadJSON(dataType, data)
	if err != nil {
		return ThreadDocument{}, err
	}
	version, err := detectThreadVersion(jsonData)
	if err != nil {
		return ThreadDocument{}, err
	}
	if version == CurrentThreadVersion {
		return ParseCurrentThreadJSON(jsonData)
	}
	legacyThread, err := parseLegacyThreadJSON(jsonData)
	if err != nil {
		return ThreadDocument{}, err
	}
	return upgradeLegacyThread(legacyThread), nil
}

type legacyThread struct {
	Version  string          `json:"version"`
	Summary  string          `json:"summary"`
	Updated  time.Time       `json:"updated_at"`
	Model    *ThreadModel    `json:"model"`
	Messages []legacyMessage `json:"messages"`
}

type legacyMessage struct {
	ID          int                `json:"id"`
	Role        legacyRole         `json:"role"`
	Text        string             `json:"text"`
	Segments    []legacySegment    `json:"segments"`
	ToolUses    []ToolUse          `json:"tool_uses"`
	ToolResults []legacyToolResult `json:"tool_results"`
	Context     string             `json:"context"`
}

type legacyRole string

const (
	legacyRoleUser      legacyRole = "User"
	legacyRoleAssistant legacyRole = "Assistant"
)

type legacySegmentType string

const (
	legacySegmentTypeThinking         legacySegmentType = "thinking"
	legacySegmentTypeRedactedThinking legacySegmentType = "RedactedThinking"
)

type legacySegment struct {
	Type      legacySegmentType `json:"type"`
	Text      string            `json:"text"`
	Data      string            `json:"data"`
	Signature string            `json:"signature"`
}

type legacyToolResult struct {
	ToolUseID string            `json:"tool_use_id"`
	IsError   bool              `json:"is_error"`
	Content   []json.RawMessage `json:"content"`
	Output    json.RawMessage   `json:"output"`
}

func detectThreadVersion(data []byte) (string, error) {
	var envelope struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		slog.Warn("providers.zed.store.version_decode_failed", "concern", "providers.zed.store", "err", err)
		return "", fmt.Errorf("decode zed thread version: %w", err)
	}
	return envelope.Version, nil
}

func parseLegacyThreadJSON(data []byte) (legacyThread, error) {
	var thread legacyThread
	if err := json.Unmarshal(data, &thread); err != nil {
		slog.Warn("providers.zed.store.legacy_thread_decode_failed", "concern", "providers.zed.store", "err", err)
		return legacyThread{}, fmt.Errorf("decode legacy zed thread json: %w", err)
	}
	return thread, nil
}

func upgradeLegacyThread(thread legacyThread) ThreadDocument {
	messages := make([]ThreadMessage, 0, len(thread.Messages))
	for _, message := range thread.Messages {
		if shouldMergeLegacyToolResults(message, messages) {
			assistant := messages[len(messages)-1].Agent
			for _, toolResult := range message.ToolResults {
				assistant.ToolResults[toolResult.ToolUseID] = convertLegacyToolResult(toolResult)
			}
			continue
		}
		upgraded, ok := upgradeLegacyMessage(message)
		if !ok {
			continue
		}
		messages = append(messages, upgraded)
	}
	return ThreadDocument{
		Version:         CurrentThreadVersion,
		Title:           thread.Summary,
		UpdatedAt:       thread.Updated,
		DetailedSummary: "",
		Model:           thread.Model,
		SubagentContext: nil,
		Messages:        messages,
	}
}

func shouldMergeLegacyToolResults(message legacyMessage, messages []ThreadMessage) bool {
	return message.Role == legacyRoleUser && len(message.ToolResults) > 0 && len(messages) > 0 && messages[len(messages)-1].Agent != nil
}

func upgradeLegacyMessage(message legacyMessage) (ThreadMessage, bool) {
	switch message.Role {
	case legacyRoleUser:
		return ThreadMessage{
			Kind:       ThreadMessageKindUser,
			User:       &UserMessage{ID: strconv.Itoa(message.ID), Content: upgradeLegacyUserContent(message)},
			Agent:      nil,
			Compaction: nil,
		}, true
	case legacyRoleAssistant:
		agent := &AgentMessage{
			Content:          upgradeLegacyAgentContent(message),
			ToolResults:      make(map[string]ToolResult),
			ReasoningDetails: nil,
		}
		return ThreadMessage{Kind: ThreadMessageKindAgent, User: nil, Agent: agent, Compaction: nil}, true
	default:
		return ThreadMessage{Kind: "", User: nil, Agent: nil, Compaction: nil}, false
	}
}

func upgradeLegacyUserContent(message legacyMessage) []UserContentPart {
	if len(message.Segments) == 0 {
		return []UserContentPart{{Kind: UserContentKindText, Text: firstNonEmptyString(message.Text, message.Context), Mention: nil}}
	}
	content := make([]UserContentPart, 0, len(message.Segments))
	for _, segment := range message.Segments {
		content = append(content, UserContentPart{Kind: UserContentKindText, Text: firstNonEmptyString(segment.Text, segment.Data), Mention: nil})
	}
	return content
}

func upgradeLegacyAgentContent(message legacyMessage) []AgentContentPart {
	if len(message.Segments) == 0 {
		return []AgentContentPart{{Kind: AgentContentKindText, Text: firstNonEmptyString(message.Text, message.Context), Signature: "", ToolUse: nil}}
	}
	content := make([]AgentContentPart, 0, len(message.Segments)+len(message.ToolUses))
	for _, segment := range message.Segments {
		switch segment.Type {
		case legacySegmentTypeThinking:
			content = append(content, AgentContentPart{Kind: AgentContentKindThinking, Text: segment.Text, Signature: segment.Signature, ToolUse: nil})
		case legacySegmentTypeRedactedThinking:
			content = append(content, AgentContentPart{Kind: AgentContentKindRedactedThinking, Text: segment.Data, Signature: "", ToolUse: nil})
		default:
			content = append(content, AgentContentPart{Kind: AgentContentKindText, Text: segment.Text, Signature: "", ToolUse: nil})
		}
	}
	for i := range message.ToolUses {
		toolUse := message.ToolUses[i]
		content = append(content, AgentContentPart{Kind: AgentContentKindToolUse, Text: "", Signature: "", ToolUse: &toolUse})
	}
	return content
}

func convertLegacyToolResult(toolResult legacyToolResult) ToolResult {
	return ToolResult(toolResult)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
