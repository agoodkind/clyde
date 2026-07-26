package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// TranscriptEntry is clyde's model of one line of a Claude Code transcript
// JSONL artifact. Claude writes conversation turns and a family of sidecar
// control records into the same file, and [EntryType] says which record a line
// is; most fields belong to one record kind and stay zero on the rest.
//
// Clyde only reads these artifacts, so the model is decode-only and no writer
// ever emits it. Fields with no reader in clyde today are still modeled, so
// what Claude records stays visible instead of disappearing at parse time.
type TranscriptEntry struct {
	// TurnAttribution decodes the sibling attribution* fields that name which
	// agent, skill, plugin, or MCP tool produced this turn.
	TurnAttribution

	// Identity and threading.
	UUID              string       `json:"uuid"`
	ParentUUID        string       `json:"parentUuid"`
	LogicalParentUUID string       `json:"logicalParentUuid"`
	LeafUUID          string       `json:"leafUuid"`
	Type              EntryType    `json:"type"`
	Subtype           EntrySubtype `json:"subtype"`
	Timestamp         time.Time    `json:"timestamp"`

	// Session and environment.
	SessionID string `json:"sessionId"`
	// HookSessionID is the session id under the snake_case key Claude writes
	// alongside the camelCase one on hook-carrying records.
	HookSessionID   string           `json:"session_id"`
	SessionKind     string           `json:"sessionKind"`
	BridgeSessionID string           `json:"bridgeSessionId"`
	LastSequenceNum int              `json:"lastSequenceNum"`
	Slug            string           `json:"slug"`
	Version         string           `json:"version"`
	Entrypoint      Entrypoint       `json:"entrypoint"`
	UserType        string           `json:"userType"`
	CWD             string           `json:"cwd"`
	RelocatedCWD    string           `json:"relocatedCwd"`
	GitBranch       string           `json:"gitBranch"`
	WorktreeSession *WorktreeSession `json:"worktreeSession"`
	ForkedFrom      *ForkedFrom      `json:"forkedFrom"`
	Mode            string           `json:"mode"`
	PermissionMode  PermissionMode   `json:"permissionMode"`
	Effort          string           `json:"effort"`

	// Titles and agent identity.
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	AgentID     string `json:"agentId"`
	AgentName   string `json:"agentName"`
	AgentKey    string `json:"key"`
	// AgentResult is a background agent's return payload. Its keys differ per
	// agent, so it stays undecoded at this edge.
	AgentResult json.RawMessage `json:"result"`

	// Message payload and prompt provenance.
	Message              json.RawMessage `json:"message"`
	Content              string          `json:"content"`
	MessageID            string          `json:"messageId"`
	RequestID            string          `json:"requestId"`
	PromptID             string          `json:"promptId"`
	PromptSource         PromptSource    `json:"promptSource"`
	LastPrompt           string          `json:"lastPrompt"`
	ExplicitPrompt       bool            `json:"explicit"`
	StackedExpansion     bool            `json:"stackedExpansion"`
	StackedOriginalInput string          `json:"stackedOriginalInput"`
	UserFeedback         string          `json:"userFeedback"`
	ClassifierMetaLines  string          `json:"classifierMetaLines"`
	ImagePasteIDs        []int           `json:"imagePasteIds"`
	Origin               *EntryOrigin    `json:"origin"`
	QueuePriority        string          `json:"queuePriority"`
	Operation            QueueOperation  `json:"operation"`

	// Visibility and compaction.
	IsMeta                    bool            `json:"isMeta"`
	IsVisibleInTranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
	IsCompactSummary          bool            `json:"isCompactSummary"`
	CompactMetadata           json.RawMessage `json:"compactMetadata"`
	MicrocompactMetadata      json.RawMessage `json:"microcompactMetadata"`
	SummarizeMetadata         json.RawMessage `json:"summarizeMetadata"`

	// Tool use.
	ToolUseResult           ToolUseResult  `json:"toolUseResult"`
	ToolUseID               string         `json:"toolUseID"`
	ToolDenialKind          ToolDenialKind `json:"toolDenialKind"`
	ToolEndsTurn            bool           `json:"toolEndsTurn"`
	SourceToolUseID         string         `json:"sourceToolUseID"`
	SourceToolAssistantUUID string         `json:"sourceToolAssistantUUID"`
	MCPMeta                 *MCPMeta       `json:"mcpMeta"`

	// Attachments and hook outcomes.
	Attachment            *Attachment      `json:"attachment"`
	HasOutput             bool             `json:"hasOutput"`
	HookCount             int              `json:"hookCount"`
	HookInfos             []HookInvocation `json:"hookInfos"`
	HookErrors            []string         `json:"hookErrors"`
	HookAdditionalContext []string         `json:"hookAdditionalContext"`
	PreventedContinuation bool             `json:"preventedContinuation"`
	StopReason            string           `json:"stopReason"`
	Level                 EntryLevel       `json:"level"`

	// Turn telemetry.
	DurationMs                  int    `json:"durationMs"`
	MessageCount                int    `json:"messageCount"`
	PendingBackgroundAgentCount int    `json:"pendingBackgroundAgentCount"`
	PendingWorkflowCount        int    `json:"pendingWorkflowCount"`
	URL                         string `json:"url"`

	// Upstream errors and interruptions.
	IsAPIErrorMessage    bool       `json:"isApiErrorMessage"`
	APIErrorStatus       int        `json:"apiErrorStatus"`
	APIErrorIsTransient  bool       `json:"apiErrorIsTransient"`
	Error                EntryError `json:"error"`
	ErrorDetails         string     `json:"errorDetails"`
	IsAbortedMidStream   bool       `json:"isAbortedMidStream"`
	InterruptedMessageID string     `json:"interruptedMessageId"`
	RetryAttempt         int        `json:"retryAttempt"`
	MaxRetries           int        `json:"maxRetries"`
	// RetryInMs is fractional in the transcript, so it decodes as a float.
	RetryInMs float64 `json:"retryInMs"`

	// Model refusals and the fallback the harness chose.
	APIRefusalCategory     string   `json:"apiRefusalCategory"`
	APIRefusalExplanation  string   `json:"apiRefusalExplanation"`
	RefusedUserMessageUUID string   `json:"refusedUserMessageUuid"`
	RetractedMessageUUIDs  []string `json:"retractedMessageUuids"`
	OriginalModel          string   `json:"originalModel"`
	FallbackModel          string   `json:"fallbackModel"`
	RefusalDirection       string   `json:"direction"`
	RefusalTrigger         string   `json:"trigger"`

	// Pull request the session opened or touched.
	PRNumber     int    `json:"prNumber"`
	PRRepository string `json:"prRepository"`
	PRURL        string `json:"prUrl"`

	// Tracked-file history.
	Snapshot          *FileHistorySnapshot `json:"snapshot"`
	IsSnapshotUpdate  bool                 `json:"isSnapshotUpdate"`
	SnapshotMessageID string               `json:"snapshotMessageId"`
	Backup            *FileBackup          `json:"backup"`
	TrackingPath      string               `json:"trackingPath"`
}

// DecodeTranscriptEntry decodes one raw transcript JSONL line. A field Claude
// did not write decodes to its zero value, so transcripts from older Claude
// Code releases still decode against the current model.
//
// A field whose JSON type does not match the model is logged and tolerated
// rather than failing the line: encoding/json still fills the rest of the
// entry, and losing a whole turn over one unexpected field would drop
// transcript content clyde can otherwise read. Only a line that is not a JSON
// object at all fails.
func DecodeTranscriptEntry(line []byte) (TranscriptEntry, error) {
	var entry TranscriptEntry
	err := json.Unmarshal(line, &entry)
	if err == nil {
		return entry, nil
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		slog.Debug("providers.claude.parser.entry_field_type_mismatch",
			"concern", concern, "component", "claude",
			"field", typeErr.Field, "value", typeErr.Value, "err", err)
		return entry, nil
	}
	slog.Warn("providers.claude.parser.entry_decode_failed", "concern", concern, "component", "claude", "err", err)
	return entry, fmt.Errorf("decode claude transcript entry: %w", err)
}
