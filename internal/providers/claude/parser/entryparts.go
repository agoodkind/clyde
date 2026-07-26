package parser

import (
	"bytes"
	"encoding/json"
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
	Kind EntryErrorKind
	// Decode says whether the whole value matched the kind's model. A partial
	// error keeps the members that decoded before the mismatch.
	Decode        FieldDecode
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
		Decode:        FieldDecodeComplete,
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
// does not model keeps Raw, and a modeled shape whose members do not match
// keeps what decoded and marks Decode partial. Neither returns an error, so an
// unfamiliar failure record still yields the rest of the entry.
func (entryError *EntryError) UnmarshalJSON(data []byte) error {
	*entryError = emptyEntryError()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	entryError.Raw = append(json.RawMessage(nil), data...)
	switch trimmed[0] {
	case '"':
		entryError.Kind = EntryErrorKindCode
		if err := json.Unmarshal(trimmed, &entryError.Code); err != nil {
			slog.Debug("providers.claude.parser.entry_error_code_failed", "concern", concern, "component", "claude", "err", err)
			entryError.Decode = FieldDecodePartial
		}
		return nil
	case '{':
		entryError.Kind = EntryErrorKindDetail
		var fields entryErrorDetail
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			slog.Debug("providers.claude.parser.entry_error_detail_failed", "concern", concern, "component", "claude", "err", err)
			entryError.Decode = FieldDecodePartial
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

// EntryTime is a timestamp on a transcript record. [time.Time] returns an error
// for a value it cannot parse, and an error returned from a nested unmarshaler
// costs every key after it, so this type keeps the text Claude wrote and
// records the outcome on the field instead.
type EntryTime struct {
	Time time.Time
	// Text is the value as Claude wrote it, kept only for a value this parser
	// could not read so that it is still recoverable. A timestamp that parsed
	// leaves it empty, because keeping it would cost an allocation on every
	// record of every transcript for a string Time already carries.
	Text   string
	Decode FieldDecode
}

// emptyEntryTime returns the zero timestamp, written out so exhaustruct sees
// every field set.
func emptyEntryTime() EntryTime {
	return EntryTime{Time: time.Time{}, Text: "", Decode: FieldDecodeComplete}
}

// UnmarshalJSON reads the RFC 3339 timestamp Claude writes. A value in any
// other shape marks the field partial and keeps its text rather than failing
// the record.
func (entryTime *EntryTime) UnmarshalJSON(data []byte) error {
	*entryTime = emptyEntryTime()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if len(trimmed) < 2 || trimmed[0] != '"' || trimmed[len(trimmed)-1] != '"' {
		slog.Debug("providers.claude.parser.entry_time_not_a_string", "concern", concern, "component", "claude", "shape", string(trimmed[:1]))
		entryTime.Text = string(trimmed)
		entryTime.Decode = FieldDecodePartial
		return nil
	}
	// The quoted body is read without unescaping, matching what
	// time.Time.UnmarshalJSON does with the same input. An RFC 3339 timestamp
	// has no character an encoder would escape.
	value := string(trimmed[1 : len(trimmed)-1])
	parsed, parseErr := time.Parse(time.RFC3339, value)
	if parseErr != nil {
		slog.Debug("providers.claude.parser.entry_time_unparsable", "concern", concern, "component", "claude", "err", parseErr)
		entryTime.Text = value
		entryTime.Decode = FieldDecodePartial
		return nil
	}
	entryTime.Time = parsed
	return nil
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
	BackupTime     EntryTime `json:"backupTime"`
	RealParentDir  string    `json:"realParentDir"`
}

// FileHistorySnapshot is the tracked-file backup set as of one message.
type FileHistorySnapshot struct {
	MessageID          string                `json:"messageId"`
	Timestamp          EntryTime             `json:"timestamp"`
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
	Timestamp   EntryTime    `json:"timestamp"`
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
	// Decode says whether every modeled key of the attachment matched. It is
	// filled by the unmarshaler rather than read from the record.
	Decode FieldDecode `json:"-"`
}

// UnmarshalJSON decodes one attachment and keeps the original object in Raw. A
// key whose type does not match keeps the rest of the attachment and marks
// Decode partial, because an error returned here would cost every key the
// record carries after the attachment.
func (attachment *Attachment) UnmarshalJSON(data []byte) error {
	// attachmentFields drops the method set so decoding does not recurse.
	type attachmentFields Attachment
	var fields attachmentFields
	decode := FieldDecodeComplete
	if err := json.Unmarshal(data, &fields); err != nil {
		slog.Debug("providers.claude.parser.attachment_decode_failed", "concern", concern, "component", "claude", "err", err)
		decode = FieldDecodePartial
	}
	*attachment = Attachment(fields)
	attachment.Raw = append(json.RawMessage(nil), data...)
	attachment.Decode = decode
	return nil
}
