package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// TurnAttribution names which agent, skill, plugin, or MCP tool produced a
// turn. Claude writes the parts as sibling top-level attribution* fields, so
// the type is embedded in [TranscriptEntry] and decodes from the same object.
// An unattributed turn leaves every part empty.
type TurnAttribution struct {
	Agent     string `json:"attributionAgent"`
	Skill     string `json:"attributionSkill"`
	Plugin    string `json:"attributionPlugin"`
	MCPServer string `json:"attributionMcpServer"`
	MCPTool   string `json:"attributionMcpTool"`
}

// Attributed reports whether any part of the attribution is set.
func (attribution TurnAttribution) Attributed() bool {
	return attribution.Agent != "" ||
		attribution.Skill != "" ||
		attribution.Plugin != "" ||
		attribution.MCPServer != "" ||
		attribution.MCPTool != ""
}

// EntryErrorKind names the JSON shape Claude wrote for a record's error
// field. An assistant turn records a short classification string, while an
// api_error system record carries the whole upstream failure object.
type EntryErrorKind string

const (
	// EntryErrorKindAbsent marks a record with no error.
	EntryErrorKindAbsent EntryErrorKind = ""
	// EntryErrorKindCode marks the classification-string form.
	EntryErrorKindCode EntryErrorKind = "code"
	// EntryErrorKindDetail marks the upstream failure-object form.
	EntryErrorKindDetail EntryErrorKind = "detail"
	// EntryErrorKindUnsupported marks a shape this parser does not model.
	// Raw still holds the value.
	EntryErrorKindUnsupported EntryErrorKind = "unsupported"
)

// EntryError is the failure Claude recorded on a record. Code is set for the
// classification form, and the remaining fields for the failure-object form.
// Connection and rate-limit members of the object form stay in Raw.
type EntryError struct {
	Kind          EntryErrorKind
	Code          string
	Message       string
	Formatted     string
	Status        int
	RequestID     string
	IsNetworkDown bool
	Raw           json.RawMessage
}

// entryErrorDetail is the wire shape of the failure-object form.
type entryErrorDetail struct {
	Message       string `json:"message"`
	Formatted     string `json:"formatted"`
	Status        int    `json:"status"`
	RequestID     string `json:"requestId"`
	IsNetworkDown bool   `json:"isNetworkDown"`
}

// emptyEntryError returns the zero error value, written out so exhaustruct
// sees every field set.
func emptyEntryError() EntryError {
	return EntryError{
		Kind:          EntryErrorKindAbsent,
		Code:          "",
		Message:       "",
		Formatted:     "",
		Status:        0,
		RequestID:     "",
		IsNetworkDown: false,
		Raw:           nil,
	}
}

// UnmarshalJSON decodes the error union by its JSON shape. A shape this parser
// does not model keeps Raw and returns no error, so an unfamiliar failure
// record still yields the rest of the entry.
func (entryError *EntryError) UnmarshalJSON(data []byte) error {
	*entryError = emptyEntryError()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	entryError.Raw = append(json.RawMessage(nil), data...)
	switch trimmed[0] {
	case '"':
		if err := json.Unmarshal(trimmed, &entryError.Code); err != nil {
			slog.Debug("providers.claude.parser.entry_error_code_failed", "concern", concern, "component", "claude", "err", err)
			return fmt.Errorf("decode claude entry error code: %w", err)
		}
		entryError.Kind = EntryErrorKindCode
		return nil
	case '{':
		var fields entryErrorDetail
		entryError.Kind = EntryErrorKindDetail
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			slog.Debug("providers.claude.parser.entry_error_detail_failed", "concern", concern, "component", "claude", "err", err)
			return fmt.Errorf("decode claude entry error detail: %w", err)
		}
		entryError.Message = fields.Message
		entryError.Formatted = fields.Formatted
		entryError.Status = fields.Status
		entryError.RequestID = fields.RequestID
		entryError.IsNetworkDown = fields.IsNetworkDown
		return nil
	default:
		slog.Debug("providers.claude.parser.entry_error_unsupported", "concern", concern, "component", "claude", "shape", string(trimmed[:1]))
		entryError.Kind = EntryErrorKindUnsupported
		return nil
	}
}

// EntryOrigin records who or what produced a user turn.
type EntryOrigin struct {
	Kind OriginKind `json:"kind"`
}

// ForkedFrom points at the conversation and message a session forked from.
type ForkedFrom struct {
	SessionID   string `json:"sessionId"`
	MessageUUID string `json:"messageUuid"`
}

// WorktreeSession records the git worktree a session moved into, along with
// the checkout it came from, so a resumed session can find its way back.
type WorktreeSession struct {
	SessionID           string `json:"sessionId"`
	OriginalCWD         string `json:"originalCwd"`
	PreEnterOriginalCWD string `json:"preEnterOriginalCwd"`
	OriginalBranch      string `json:"originalBranch"`
	OriginalHeadCommit  string `json:"originalHeadCommit"`
	WorktreePath        string `json:"worktreePath"`
	WorktreeName        string `json:"worktreeName"`
	WorktreeBranch      string `json:"worktreeBranch"`
}

// FileBackup records one tracked-file copy Claude took before editing it.
// BackupFileName is null in the transcript when the file did not exist yet,
// which decodes to the empty string.
type FileBackup struct {
	BackupFileName string    `json:"backupFileName"`
	Version        int       `json:"version"`
	BackupTime     time.Time `json:"backupTime"`
	RealParentDir  string    `json:"realParentDir"`
}

// FileHistorySnapshot is the tracked-file backup set as of one message.
type FileHistorySnapshot struct {
	MessageID          string                `json:"messageId"`
	Timestamp          time.Time             `json:"timestamp"`
	TrackedFileBackups map[string]FileBackup `json:"trackedFileBackups"`
}

// HookInvocation is one hook command Claude ran for an event. DurationMs is
// absent for a hook that was still running when the summary was written.
type HookInvocation struct {
	Command    string `json:"command"`
	DurationMs int    `json:"durationMs"`
	PromptText string `json:"promptText"`
}

// MCPMeta carries the MCP protocol envelope of a tool result. Both members
// are server-defined payloads from the MCP tool-result contract, so they stay
// opaque here and a consumer decodes a concrete schema per server.
type MCPMeta struct {
	StructuredContent json.RawMessage `json:"structuredContent"`
	Meta              json.RawMessage `json:"_meta"`
}

// Attachment is one injected-context record. Claude writes about fifty
// distinct keys across the attachment types, so this models the fields shared
// by the high-volume types and keeps the whole original object in Raw for the
// type-specific remainder.
type Attachment struct {
	Type AttachmentType `json:"type"`

	// Hook attachment types.
	HookName   string `json:"hookName"`
	HookEvent  string `json:"hookEvent"`
	ToolUseID  string `json:"toolUseID"`
	Command    string `json:"command"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int    `json:"durationMs"`

	// Queued-command attachment types.
	CommandMode string       `json:"commandMode"`
	Timestamp   time.Time    `json:"timestamp"`
	Origin      *EntryOrigin `json:"origin"`

	// Output-style attachment types.
	Style        string `json:"style"`
	TurnReminder string `json:"turnReminder"`

	// File attachment types.
	Filename    string `json:"filename"`
	DisplayPath string `json:"displayPath"`
	Snippet     string `json:"snippet"`

	// Listing and delta attachment types.
	ItemCount  int `json:"itemCount"`
	SkillCount int `json:"skillCount"`

	// Content is the injected text. Claude writes it as a string, an object,
	// a list of strings, or a list of content blocks depending on the
	// attachment type, so it stays undecoded at this edge.
	Content json.RawMessage `json:"content"`
	// Prompt is the queued prompt, written either as a string or as a list of
	// content blocks, so it stays undecoded at this edge.
	Prompt json.RawMessage `json:"prompt"`
	// Raw is the whole attachment object, kept so the type-specific keys this
	// struct does not model are still recoverable.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes one attachment and keeps the original object in Raw.
func (attachment *Attachment) UnmarshalJSON(data []byte) error {
	// attachmentFields drops the method set so decoding does not recurse.
	type attachmentFields Attachment
	var fields attachmentFields
	if err := json.Unmarshal(data, &fields); err != nil {
		slog.Debug("providers.claude.parser.attachment_decode_failed", "concern", concern, "component", "claude", "err", err)
		return fmt.Errorf("decode claude attachment: %w", err)
	}
	*attachment = Attachment(fields)
	attachment.Raw = append(json.RawMessage(nil), data...)
	return nil
}
