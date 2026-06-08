package codexstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"strings"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// ThreadSource is the typed subset of Codex SessionSource that Clyde needs for
// local discovery and details rendering.
type ThreadSource struct {
	Kind           ThreadSourceKind
	ParentThreadID string
	AgentNickname  string
	AgentRole      string
}

// ThreadSourceKind is part of Clyde's typed adapter surface.
type ThreadSourceKind string

const (
	// ThreadSourceUnknown is part of Clyde's typed adapter surface.
	ThreadSourceUnknown ThreadSourceKind = "unknown"
	// ThreadSourceCLI is part of Clyde's typed adapter surface.
	ThreadSourceCLI ThreadSourceKind = "cli"
	// ThreadSourceVSCode is part of Clyde's typed adapter surface.
	ThreadSourceVSCode ThreadSourceKind = "vscode"
	// ThreadSourceExec is part of Clyde's typed adapter surface.
	ThreadSourceExec ThreadSourceKind = "exec"
	// ThreadSourceMCP is part of Clyde's typed adapter surface.
	ThreadSourceMCP ThreadSourceKind = "mcp"
	// ThreadSourceCustom is part of Clyde's typed adapter surface.
	ThreadSourceCustom ThreadSourceKind = "custom"
	// ThreadSourceSubagent is part of Clyde's typed adapter surface.
	ThreadSourceSubagent ThreadSourceKind = "subagent"
	// ThreadSourceSubagentOld is part of Clyde's typed adapter surface.
	ThreadSourceSubagentOld ThreadSourceKind = "subAgent"
)

// ThreadSummary is a provider-owned summary of a Codex rollout thread.
type ThreadSummary struct {
	ID            string
	RolloutPath   string
	ForkedFromID  string
	Preview       string
	Name          string
	ModelProvider string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CWD           string
	LatestCWD     string
	CLIVersion    string
	Originator    string
	Source        ThreadSource
	AgentNickname string
	AgentRole     string
	IsSubagent    bool
	IsArchived    bool
	Messages      []HistoryMessage
}

// HistoryMessage is a normalized conversational message extracted from Codex
// rollout entries.
type HistoryMessage struct {
	Role              string
	ParentUUID        string
	LogicalParentUUID string
	Visibility        transcript.MessageVisibility
	Compaction        *transcript.CompactionMetadata
	Text              string
	Timestamp         time.Time
	Phase             string
}

// ReadThreadByRolloutPath returns a Codex rollout thread summary by JSONL path.
func ReadThreadByRolloutPath(path string, includeHistory bool, archived bool) (ThreadSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("codex.store.history.open_failed", "concern", "providers.codex.store", "path", path, "err", err)
		return ThreadSummary{}, fmt.Errorf("open codex rollout %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	summary := ThreadSummary{
		RolloutPath: path,
		IsArchived:  archived, ID: "", ForkedFromID: "", Preview: "", Name: "", ModelProvider: "", CreatedAt: time.
				Time{},

		UpdatedAt: time.
			Time{},

		CWD: "", LatestCWD: "", CLIVersion: "", Originator: "", Source: ThreadSource{Kind: "", ParentThreadID: "", AgentNickname: "", AgentRole: ""},

		AgentNickname: "", AgentRole: "", IsSubagent: false, Messages: nil,
	}
	// A rollout line is a single JSONL record. Codex compaction checkpoints inline
	// the full pre-compaction history into one `compacted` line, which can reach
	// tens of megabytes, so an uncapped bufio.Reader replaces bufio.Scanner (whose
	// fixed token cap aborts with "token too long" on such lines).
	reader := bufio.NewReader(f)
	for {
		raw, readErr := reader.ReadBytes('\n')
		line := bytes.TrimRight(raw, "\r\n")
		var envelope historyLine
		// An unparseable line decodes to the zero envelope and is skipped, matching
		// the prior scanner behavior; only a successful decode is folded in.
		if len(line) > 0 && json.Unmarshal(line, &envelope) == nil {
			if err := applyEnvelope(&summary, envelope, includeHistory, path); err != nil {
				return ThreadSummary{}, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return ThreadSummary{}, fmt.Errorf("read codex rollout %s: %w", path, readErr)
		}
	}
	if summary.ID == "" {
		return ThreadSummary{}, fmt.Errorf("codex rollout %s missing session_meta id", path)
	}
	if summary.UpdatedAt.IsZero() {
		if stat, err := os.Stat(path); err == nil {
			summary.UpdatedAt = stat.ModTime()
		}
	}
	return summary, nil
}

// StreamMessages yields normalized Codex rollout messages as each JSONL envelope
// is read. When system messages are excluded, compaction payloads are skipped
// before decoding so replacement_history stays untouched.
func StreamMessages(path string, includeSystemMessages bool) iter.Seq2[HistoryMessage, error] {
	return func(yield func(HistoryMessage, error) bool) {
		f, err := os.Open(path)
		if err != nil {
			slog.Warn("codex.store.history.open_failed", "concern", "providers.codex.store", "path", path, "err", err)
			yield(emptyHistoryMessage(), fmt.Errorf("open codex rollout %s: %w", path, err))
			return
		}
		defer func() { _ = f.Close() }()

		reader := bufio.NewReader(f)
		for {
			raw, readErr := reader.ReadBytes('\n')
			line := bytes.TrimRight(raw, "\r\n")
			var envelope historyLine
			if len(line) > 0 && json.Unmarshal(line, &envelope) == nil {
				if msg, ok := streamMessageFromEnvelope(envelope, includeSystemMessages); ok {
					if !yield(msg, nil) {
						return
					}
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				slog.Warn("codex.store.history.read_failed", "concern", "providers.codex.store", "path", path, "err", readErr)
				yield(emptyHistoryMessage(), fmt.Errorf("read codex rollout %s: %w", path, readErr))
				return
			}
		}
	}
}

// applyEnvelope folds one decoded rollout envelope into summary. It returns an
// error only when a session_meta payload cannot be decoded.
func applyEnvelope(summary *ThreadSummary, envelope historyLine, includeHistory bool, path string) error {
	lineTime := parseCodexTime(envelope.Timestamp)
	if !lineTime.IsZero() {
		summary.UpdatedAt = lineTime
	}
	switch historyEnvelopeType(envelope.Type) {
	case historyEnvelopeSessionMeta:
		var payload sessionMetaPayload
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			slog.Warn("codex.store.history.session_meta_unmarshal_failed", "concern", "providers.codex.store", "path", path, "err", err)
			return fmt.Errorf("unmarshal codex session metadata %s: %w", path, err)
		}
		applySessionMeta(summary, payload, lineTime)
	case historyEnvelopeResponseItem:
		if msg, ok := responseItemMessage(envelope.Payload, lineTime); ok {
			applyMessage(summary, msg, includeHistory)
		}
	case historyEnvelopeEventMsg:
		if msg, ok := eventMessage(envelope.Payload, lineTime); ok {
			applyMessage(summary, msg, includeHistory)
		}
	case historyEnvelopeCompacted:
		if msg, ok := compactedMessage(envelope.Payload, lineTime); ok {
			applyMessage(summary, msg, includeHistory)
		}
	case historyEnvelopeTurnContext:
		applyTurnContext(summary, envelope.Payload)
	}
	return nil
}

func streamMessageFromEnvelope(envelope historyLine, includeSystemMessages bool) (HistoryMessage, bool) {
	lineTime := parseCodexTime(envelope.Timestamp)
	switch historyEnvelopeType(envelope.Type) {
	case historyEnvelopeResponseItem:
		return responseItemMessage(envelope.Payload, lineTime)
	case historyEnvelopeEventMsg:
		return eventMessage(envelope.Payload, lineTime)
	case historyEnvelopeCompacted:
		if !includeSystemMessages {
			return emptyHistoryMessage(), false
		}
		return compactedMessage(envelope.Payload, lineTime)
	case historyEnvelopeSessionMeta, historyEnvelopeTurnContext:
		return emptyHistoryMessage(), false
	default:
		return emptyHistoryMessage(), false
	}
}

type historyLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMetaPayload struct {
	ID            string      `json:"id"`
	Timestamp     string      `json:"timestamp"`
	CWD           string      `json:"cwd"`
	Originator    string      `json:"originator"`
	CLIVersion    string      `json:"cli_version"`
	ModelProvider string      `json:"model_provider"`
	Source        sourceUnion `json:"source"`
	AgentNickname string      `json:"agent_nickname"`
	AgentRole     string      `json:"agent_role"`
}

// sourceUnion models the SessionSource union from research/codex and keeps the
// raw union localized at the file-format boundary.
type sourceUnion struct {
	ThreadSource
}

func (s *sourceUnion) UnmarshalJSON(data []byte) error {
	var scalar string
	if err := json.Unmarshal(data, &scalar); err == nil {
		s.Kind = normalizeThreadSourceKind(scalar)
		return nil
	}
	var object sourceObject
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("unmarshal codex source object: %w", err)
	}
	switch {
	case object.Subagent.ThreadSpawn.ParentThreadID != "":
		s.Kind = ThreadSourceSubagent
		s.ParentThreadID = object.Subagent.ThreadSpawn.ParentThreadID
		s.AgentNickname = object.Subagent.ThreadSpawn.AgentNickname
		s.AgentRole = object.Subagent.ThreadSpawn.AgentRole
	case object.Subagent.Review:
		s.Kind = ThreadSourceSubagent
		s.AgentRole = "review"
	case object.Subagent.Compact:
		s.Kind = ThreadSourceSubagent
		s.AgentRole = "compact"
	case object.Subagent.Other != "":
		s.Kind = ThreadSourceSubagent
		s.AgentRole = object.Subagent.Other
	case object.Custom != "":
		s.Kind = ThreadSourceCustom
	default:
		s.Kind = ThreadSourceUnknown
	}
	return nil
}

type sourceObject struct {
	Custom   string `json:"custom"`
	Subagent struct {
		ThreadSpawn struct {
			ParentThreadID string `json:"parent_thread_id"`
			AgentNickname  string `json:"agent_nickname"`
			AgentRole      string `json:"agent_role"`
		} `json:"thread_spawn"`
		Review  bool   `json:"review"`
		Compact bool   `json:"compact"`
		Other   string `json:"other"`
	} `json:"subagent"`
}

type responsePayload struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
	Phase   string        `json:"phase"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type eventPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Phase   string `json:"phase"`
}

type compactedPayload struct {
	Message            string             `json:"message"`
	ReplacementHistory *[]json.RawMessage `json:"replacement_history"`
}

type turnContextPayload struct {
	CWD string `json:"cwd"`
}

func applySessionMeta(summary *ThreadSummary, payload sessionMetaPayload, lineTime time.Time) {
	summary.ID = payload.ID
	summary.CWD = payload.CWD
	summary.Originator = payload.Originator
	summary.CLIVersion = payload.CLIVersion
	summary.ModelProvider = payload.ModelProvider
	summary.Source = payload.Source.ThreadSource
	summary.AgentNickname = payload.AgentNickname
	summary.AgentRole = payload.AgentRole
	if summary.Source.AgentNickname == "" {
		summary.Source.AgentNickname = payload.AgentNickname
	}
	if summary.Source.AgentRole == "" {
		summary.Source.AgentRole = payload.AgentRole
	}
	summary.ForkedFromID = payload.Source.ParentThreadID
	summary.IsSubagent = payload.Source.Kind == ThreadSourceSubagent ||
		payload.Source.Kind == ThreadSourceSubagentOld ||
		payload.Source.ParentThreadID != "" ||
		payload.AgentNickname != "" ||
		payload.AgentRole != ""
	if created := parseCodexTime(payload.Timestamp); !created.IsZero() {
		summary.CreatedAt = created
	} else if !lineTime.IsZero() {
		summary.CreatedAt = lineTime
	}
	if summary.UpdatedAt.IsZero() {
		summary.UpdatedAt = summary.CreatedAt
	}
}

func applyTurnContext(summary *ThreadSummary, raw json.RawMessage) {
	var payload turnContextPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if strings.TrimSpace(payload.CWD) != "" {
		summary.LatestCWD = payload.CWD
	}
}

func applyMessage(summary *ThreadSummary, msg HistoryMessage, includeHistory bool) {
	if summary.Preview == "" && msg.Role == "user" {
		summary.Preview = msg.Text
	}
	if includeHistory {
		summary.Messages = append(summary.Messages, msg)
	}
}

func responseItemMessage(raw json.RawMessage, timestamp time.Time) (HistoryMessage, bool) {
	var payload responsePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emptyHistoryMessage(), false
	}
	if payload.Type != "message" {
		return emptyHistoryMessage(), false
	}
	text := strings.TrimSpace(contentText(payload.Content))
	if text == "" {
		return emptyHistoryMessage(), false
	}
	return HistoryMessage{
		Role:              payload.Role,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Text:              text,
		Timestamp:         timestamp,
		Phase:             payload.Phase,
	}, true
}

func eventMessage(raw json.RawMessage, timestamp time.Time) (HistoryMessage, bool) {
	var payload eventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emptyHistoryMessage(), false
	}
	var role string
	switch eventMessageType(payload.Type) {
	case eventMessageTypeUser:
		role = "user"
	case eventMessageTypeAgent:
		role = "assistant"
	default:
		return emptyHistoryMessage(), false
	}
	text := strings.TrimSpace(payload.Message)
	if text == "" {
		return emptyHistoryMessage(), false
	}
	return HistoryMessage{
		Role:              role,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Text:              text,
		Timestamp:         timestamp,
		Phase:             payload.Phase,
	}, true
}

func compactedMessage(raw json.RawMessage, timestamp time.Time) (HistoryMessage, bool) {
	var payload compactedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emptyHistoryMessage(), false
	}
	contextItems := []transcript.CompactedContextItem(nil)
	replacementHistoryCount := 0
	trimmedMessage := strings.TrimSpace(payload.Message)
	if payload.ReplacementHistory != nil {
		replacementHistoryCount = len(*payload.ReplacementHistory)
		contextItems = normalizeCompactedContextItems(*payload.ReplacementHistory)
	} else if trimmedMessage != "" {
		contextItems = []transcript.CompactedContextItem{
			legacyCompactedSummaryContextItem(trimmedMessage),
		}
	}
	return HistoryMessage{
		Role:              "system",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction: &transcript.CompactionMetadata{
			Kind:                      transcript.CompactionKindBoundary,
			Trigger:                   transcript.CompactionTriggerUnknown,
			PreTokens:                 0,
			PostTokens:                0,
			TokensSaved:               0,
			MessagesSummarized:        0,
			ReplacementHistoryCount:   replacementHistoryCount,
			HeadUUID:                  "",
			AnchorUUID:                "",
			TailUUID:                  "",
			ContextItems:              contextItems,
			UserContext:               "",
			Direction:                 "",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        nil,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		},
		Text:      trimmedMessage,
		Timestamp: timestamp,
		Phase:     "",
	}, true
}

func emptyHistoryMessage() HistoryMessage {
	return HistoryMessage{
		Role:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Visibility:        "",
		Compaction:        nil,
		Text:              "",
		Timestamp:         time.Time{},
		Phase:             "",
	}
}

func contentText(parts []contentPart) string {
	var b strings.Builder
	for _, part := range parts {
		switch contentPartTypeKey(part.Type) {
		case contentPartTypeKeyInputText, contentPartTypeKeyOutputText:
			if part.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func parseCodexTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

func normalizeThreadSourceKind(value string) ThreadSourceKind {
	switch threadSourceAlias(strings.ToLower(strings.TrimSpace(value))) {
	case threadSourceAliasCLI:
		return ThreadSourceCLI
	case threadSourceAliasVSCode, threadSourceAliasVSCodeSnake:
		return ThreadSourceVSCode
	case threadSourceAliasExec:
		return ThreadSourceExec
	case threadSourceAliasMCP, threadSourceAliasAppserver, threadSourceAliasAppServerSnake:
		return ThreadSourceMCP
	case threadSourceAliasCustom:
		return ThreadSourceCustom
	case threadSourceAliasSubagent, threadSourceAliasSubAgentSnake, threadSourceAliasSubagentOld, threadSourceAliasSubagentOldDash:
		return ThreadSourceSubagent
	default:
		if strings.TrimSpace(value) == "" {
			return ThreadSourceUnknown
		}
		return ThreadSourceKind(strings.TrimSpace(value))
	}
}

// historyEnvelopeType enumerates the top-level type strings codex
// writes per rollout-line envelope.
type historyEnvelopeType string

const (
	historyEnvelopeSessionMeta  historyEnvelopeType = "session_meta"
	historyEnvelopeResponseItem historyEnvelopeType = "response_item"
	historyEnvelopeEventMsg     historyEnvelopeType = "event_msg"
	historyEnvelopeCompacted    historyEnvelopeType = "compacted"
	historyEnvelopeTurnContext  historyEnvelopeType = "turn_context"
)

// eventMessageType enumerates the event_msg payload types codex uses
// for user vs agent turns.
type eventMessageType string

const (
	eventMessageTypeUser  eventMessageType = "user_message"
	eventMessageTypeAgent eventMessageType = "agent_message"
)

// contentPartTypeKey enumerates the message content-part type strings
// codex's history-line content arrays use for free-form text.
type contentPartTypeKey string

const (
	contentPartTypeKeyInputText  contentPartTypeKey = "input_text"
	contentPartTypeKeyOutputText contentPartTypeKey = "output_text"
)

// threadSourceAlias enumerates the lowercase aliases the codex history
// loader maps to canonical ThreadSourceKind values.
type threadSourceAlias string

const (
	threadSourceAliasCLI             threadSourceAlias = "cli"
	threadSourceAliasVSCode          threadSourceAlias = "vscode"
	threadSourceAliasVSCodeSnake     threadSourceAlias = "vs_code"
	threadSourceAliasExec            threadSourceAlias = "exec"
	threadSourceAliasMCP             threadSourceAlias = "mcp"
	threadSourceAliasAppserver       threadSourceAlias = "appserver"
	threadSourceAliasAppServerSnake  threadSourceAlias = "app_server"
	threadSourceAliasCustom          threadSourceAlias = "custom"
	threadSourceAliasSubagent        threadSourceAlias = "subagent"
	threadSourceAliasSubAgentSnake   threadSourceAlias = "sub_agent"
	threadSourceAliasSubagentOld     threadSourceAlias = "subagent_old"
	threadSourceAliasSubagentOldDash threadSourceAlias = "subagent-old"
)
