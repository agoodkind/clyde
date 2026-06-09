package codexstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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

const headerLineCap = 64

// ReadHeader returns a Codex rollout thread summary from the header JSONL
// entries without reading message history.
func ReadHeader(path string, archived bool) (ThreadSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("codex.store.history.open_failed", "concern", "providers.codex.store", "path", path, "err", err)
		return ThreadSummary{}, fmt.Errorf("open codex rollout %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	summary := ThreadSummary{
		ID:            "",
		RolloutPath:   path,
		ForkedFromID:  "",
		Preview:       "",
		Name:          "",
		ModelProvider: "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		CWD:           "",
		LatestCWD:     "",
		CLIVersion:    "",
		Originator:    "",
		Source:        ThreadSource{Kind: "", ParentThreadID: "", AgentNickname: "", AgentRole: ""},
		AgentNickname: "",
		AgentRole:     "",
		IsSubagent:    false,
		IsArchived:    archived,
		Messages:      nil,
	}

	reader := bufio.NewReader(f)
	for range headerLineCap {
		raw, readErr := reader.ReadBytes('\n')
		line := bytes.TrimRight(raw, "\r\n")
		var envelope historyLine
		if len(line) > 0 && json.Unmarshal(line, &envelope) == nil {
			switch historyEnvelopeType(envelope.Type) {
			case historyEnvelopeSessionMeta:
				var payload sessionMetaPayload
				if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
					slog.Warn("codex.store.history.session_meta_unmarshal_failed", "concern", "providers.codex.store", "path", path, "err", err)
					return ThreadSummary{}, fmt.Errorf("unmarshal codex session metadata %s: %w", path, err)
				}
				applySessionMeta(&summary, payload, parseCodexTime(envelope.Timestamp))
				summary.UpdatedAt = time.Time{}
				return summary, nil
			case historyEnvelopeTurnContext:
				applyTurnContext(&summary, envelope.Payload)
			case historyEnvelopeResponseItem, historyEnvelopeEventMsg, historyEnvelopeCompacted:
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			slog.Warn("codex.store.history.read_failed", "concern", "providers.codex.store", "path", path, "err", readErr)
			return ThreadSummary{}, fmt.Errorf("read codex rollout %s: %w", path, readErr)
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

		reader := bufio.NewReaderSize(f, streamReaderBufferSize)
		for {
			if !includeSystemMessages {
				skipped, skipErr := skipLargeCompactedLine(reader)
				if skipErr != nil {
					reportStreamReadError(path, skipErr, yield)
					return
				}
				if skipped {
					continue
				}
			}
			raw, readErr := reader.ReadBytes('\n')
			if !emitStreamLine(raw, includeSystemMessages, yield) {
				return
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				reportStreamReadError(path, readErr, yield)
				return
			}
		}
	}
}

// emitStreamLine decodes one raw JSONL line and yields its normalized message
// when the envelope produces one. It returns false when the consumer asked to
// stop iterating, and true otherwise so the caller continues reading.
func emitStreamLine(raw []byte, includeSystemMessages bool, yield func(HistoryMessage, error) bool) bool {
	line := bytes.TrimRight(raw, "\r\n")
	if len(line) == 0 {
		return true
	}
	var envelope historyLine
	if json.Unmarshal(line, &envelope) != nil {
		return true
	}
	msg, ok := streamMessageFromEnvelope(envelope, includeSystemMessages)
	if !ok {
		return true
	}
	return yield(msg, nil)
}

// reportStreamReadError logs a Codex rollout read failure and surfaces it to the
// iteration consumer through the yield function.
func reportStreamReadError(path string, readErr error, yield func(HistoryMessage, error) bool) {
	slog.Warn("codex.store.history.read_failed", "concern", "providers.codex.store", "path", path, "err", readErr)
	yield(emptyHistoryMessage(), fmt.Errorf("read codex rollout %s: %w", path, readErr))
}

const (
	// streamPeekPrefixBytes bounds how much of a JSONL line StreamMessages
	// inspects to classify the envelope type before deciding whether to skip it.
	// The codex envelope writes timestamp then type then payload, so the
	// envelope-level type lands well within this prefix.
	streamPeekPrefixBytes = 4 * 1024
	// streamReaderBufferSize must exceed streamPeekPrefixBytes so Peek can fill
	// the prefix, and it also bounds each drain chunk for skipped lines.
	streamReaderBufferSize = 8 * 1024
)

// skipLargeCompactedLine peeks a bounded prefix of the next JSONL line and, when
// that line is a compacted envelope too large to fit the prefix, drains it to
// the next newline in bounded chunks instead of buffering the whole line into
// memory. It returns true when it consumed such a line. When the line fits the
// prefix, or its envelope type cannot be confirmed as compacted, it consumes
// nothing and returns false so the caller reads the line normally. Callers must
// only invoke this when system messages are excluded, since that is the only
// case where compacted lines are dropped.
func skipLargeCompactedLine(reader *bufio.Reader) (bool, error) {
	prefix, peekErr := reader.Peek(streamPeekPrefixBytes)
	if len(prefix) == 0 {
		return false, nil
	}
	if bytes.IndexByte(prefix, '\n') >= 0 {
		return false, nil
	}
	if peekErr != nil && !errors.Is(peekErr, bufio.ErrBufferFull) {
		return false, nil
	}
	envelopeType, ok := peekedEnvelopeType(prefix)
	if !ok || historyEnvelopeType(envelopeType) != historyEnvelopeCompacted {
		return false, nil
	}
	if err := drainLine(reader); err != nil {
		return false, err
	}
	return true, nil
}

// drainLine advances the reader past the next newline, reading in buffer-sized
// slices that are discarded so an oversized line never lands in a single
// allocation. ReadSlice returns the internal buffer view bounded by the reader
// size and reports ErrBufferFull when the delimiter is not yet reached.
func drainLine(reader *bufio.Reader) error {
	for {
		_, err := reader.ReadSlice('\n')
		if err == nil {
			return nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		slog.Warn("codex.store.history.drain_failed", "concern", "providers.codex.store", "err", err)
		return fmt.Errorf("drain codex rollout line: %w", err)
	}
}

// peekedEnvelopeType extracts the first JSONL envelope "type" value from a
// bounded prefix without decoding the whole line. The codex envelope writes the
// envelope-level type before the payload, so the first "type" key in the prefix
// is the envelope type. It returns ok=false when the type value is not fully
// present within the prefix, so callers fall back to normal decoding.
func peekedEnvelopeType(prefix []byte) (string, bool) {
	_, rest, found := bytes.Cut(prefix, []byte(`"type"`))
	if !found {
		return "", false
	}
	cursor := 0
	for cursor < len(rest) && (rest[cursor] == ' ' || rest[cursor] == '\t' || rest[cursor] == ':') {
		cursor++
	}
	if cursor >= len(rest) || rest[cursor] != '"' {
		return "", false
	}
	cursor++
	valueStart := cursor
	for cursor < len(rest) && rest[cursor] != '"' {
		cursor++
	}
	if cursor >= len(rest) {
		return "", false
	}
	return string(rest[valueStart:cursor]), true
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
	ID            string            `json:"id"`
	ForkedFromID  string            `json:"forked_from_id"`
	Timestamp     string            `json:"timestamp"`
	CWD           string            `json:"cwd"`
	Originator    string            `json:"originator"`
	CLIVersion    string            `json:"cli_version"`
	ModelProvider string            `json:"model_provider"`
	Source        sourceUnion       `json:"source"`
	ThreadSource  threadSourceField `json:"thread_source"`
	AgentNickname string            `json:"agent_nickname"`
	AgentRole     string            `json:"agent_role"`
}

// threadSourceField models the top-level session_meta thread_source classifier
// codex-rs writes alongside source. Only the machinery values matter to Clyde;
// unknown values stay as their raw string and are treated as non-machinery.
type threadSourceField string

const (
	threadSourceFieldUser                threadSourceField = "user"
	threadSourceFieldSubagent            threadSourceField = "subagent"
	threadSourceFieldMemoryConsolidation threadSourceField = "memory_consolidation"
)

// threadSourceFieldIsMachinery reports whether the top-level thread_source marks
// a thread as Codex-internal machinery rather than a user conversation.
func threadSourceFieldIsMachinery(value threadSourceField) bool {
	switch value {
	case threadSourceFieldSubagent, threadSourceFieldMemoryConsolidation:
		return true
	case threadSourceFieldUser:
		return false
	default:
		return false
	}
}

// sourceUnion models the SessionSource union from research/codex and keeps the
// raw union localized at the file-format boundary.
type sourceUnion struct {
	ThreadSource
}

func (s *sourceUnion) UnmarshalJSON(data []byte) error {
	var scalar string
	if err := json.Unmarshal(data, &scalar); err == nil {
		s.applyScalarSource(scalar)
		return nil
	}
	var object sourceObject
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("unmarshal codex source object: %w", err)
	}
	s.applyObjectSource(object)
	return nil
}

// applyScalarSource handles the externally tagged unit variants codex-rs renders
// as bare strings, including the Display machinery forms such as
// "internal_memory_consolidation" and "subagent_review", before falling back to
// the canonical source aliases.
func (s *sourceUnion) applyScalarSource(scalar string) {
	if role, ok := machineryScalarRole(scalar); ok {
		s.Kind = ThreadSourceSubagent
		s.AgentRole = role
		return
	}
	s.Kind = normalizeThreadSourceKind(scalar)
}

// applyObjectSource maps the externally tagged SessionSource object variants onto
// Clyde's typed source. Machinery variants (internal memory_consolidation and the
// parentless subagent review/compact/memory_consolidation) become subagent
// sources with no parent thread id so the ScanRecord machinery exclusion drops
// them. thread_spawn keeps its parent and forks stay untouched.
func (s *sourceUnion) applyObjectSource(object sourceObject) {
	switch {
	case object.Subagent.Kind == subagentSourceThreadSpawn && object.Subagent.ParentThreadID != "":
		s.Kind = ThreadSourceSubagent
		s.ParentThreadID = object.Subagent.ParentThreadID
		s.AgentNickname = object.Subagent.AgentNickname
		s.AgentRole = object.Subagent.AgentRole
	case object.Subagent.Kind == subagentSourceReview:
		s.Kind = ThreadSourceSubagent
		s.AgentRole = string(subagentSourceReview)
	case object.Subagent.Kind == subagentSourceCompact:
		s.Kind = ThreadSourceSubagent
		s.AgentRole = string(subagentSourceCompact)
	case object.Subagent.Kind == subagentSourceMemoryConsolidation:
		s.Kind = ThreadSourceSubagent
		s.AgentRole = string(subagentSourceMemoryConsolidation)
	case object.Subagent.Kind == subagentSourceOther && object.Subagent.AgentRole != "":
		s.Kind = ThreadSourceSubagent
		s.AgentRole = object.Subagent.AgentRole
	case object.Internal.Kind == internalSourceMemoryConsolidation:
		s.Kind = ThreadSourceSubagent
		s.AgentRole = string(internalSourceMemoryConsolidation)
	case object.Custom != "":
		s.Kind = ThreadSourceCustom
	default:
		s.Kind = ThreadSourceUnknown
	}
}

type sourceObject struct {
	Custom   string         `json:"custom"`
	Internal internalSource `json:"internal"`
	Subagent subagentSource `json:"subagent"`
}

// internalSource models SessionSource::Internal(InternalSessionSource). The inner
// enum is unit-only, so codex-rs serializes it as a bare snake_case string.
type internalSource struct {
	Kind internalSourceKind
}

// internalSourceKind enumerates the InternalSessionSource variants Clyde models.
type internalSourceKind string

const (
	internalSourceNone                internalSourceKind = ""
	internalSourceMemoryConsolidation internalSourceKind = "memory_consolidation"
)

func (i *internalSource) UnmarshalJSON(data []byte) error {
	i.Kind = internalSourceNone
	var scalar string
	if json.Unmarshal(data, &scalar) == nil && strings.TrimSpace(scalar) == string(internalSourceMemoryConsolidation) {
		i.Kind = internalSourceMemoryConsolidation
	}
	return nil
}

// subagentSource models SessionSource::SubAgent(SubAgentSource). Unit variants
// (review, compact, memory_consolidation) serialize as bare snake_case strings;
// thread_spawn serializes as {"thread_spawn":{...}} and other as {"other":"..."}.
type subagentSource struct {
	Kind           subagentSourceKind
	ParentThreadID string
	AgentNickname  string
	AgentRole      string
}

// subagentSourceKind enumerates the SubAgentSource variants Clyde models.
type subagentSourceKind string

const (
	subagentSourceNone                subagentSourceKind = ""
	subagentSourceReview              subagentSourceKind = "review"
	subagentSourceCompact             subagentSourceKind = "compact"
	subagentSourceMemoryConsolidation subagentSourceKind = "memory_consolidation"
	subagentSourceThreadSpawn         subagentSourceKind = "thread_spawn"
	subagentSourceOther               subagentSourceKind = "other"
)

func (s *subagentSource) UnmarshalJSON(data []byte) error {
	s.Kind = subagentSourceNone
	var scalar string
	if json.Unmarshal(data, &scalar) == nil {
		switch subagentSourceKind(strings.TrimSpace(scalar)) {
		case subagentSourceReview:
			s.Kind = subagentSourceReview
		case subagentSourceCompact:
			s.Kind = subagentSourceCompact
		case subagentSourceMemoryConsolidation:
			s.Kind = subagentSourceMemoryConsolidation
		case subagentSourceNone, subagentSourceThreadSpawn, subagentSourceOther:
			s.Kind = subagentSourceNone
		default:
			s.Kind = subagentSourceNone
		}
		return nil
	}
	var object subagentObject
	if json.Unmarshal(data, &object) == nil {
		switch {
		case object.ThreadSpawn != nil:
			s.Kind = subagentSourceThreadSpawn
			s.ParentThreadID = object.ThreadSpawn.ParentThreadID
			s.AgentNickname = object.ThreadSpawn.AgentNickname
			s.AgentRole = object.ThreadSpawn.AgentRole
		case object.Other != "":
			s.Kind = subagentSourceOther
			s.AgentRole = object.Other
		}
	}
	return nil
}

type subagentObject struct {
	ThreadSpawn *threadSpawnObject `json:"thread_spawn"`
	Other       string             `json:"other"`
}

type threadSpawnObject struct {
	ParentThreadID string `json:"parent_thread_id"`
	AgentNickname  string `json:"agent_nickname"`
	AgentRole      string `json:"agent_role"`
}

// machineryScalarRole maps the Display-form scalar source strings codex-rs emits
// for internal and parentless subagent machinery onto a normalized role.
func machineryScalarRole(value string) (string, bool) {
	switch machineryScalarSource(strings.ToLower(strings.TrimSpace(value))) {
	case machineryScalarInternalMemoryConsolidation,
		machineryScalarSubagentMemoryConsolidation,
		machineryScalarMemoryConsolidation:
		return string(internalSourceMemoryConsolidation), true
	case machineryScalarSubagentReview:
		return string(subagentSourceReview), true
	case machineryScalarSubagentCompact:
		return string(subagentSourceCompact), true
	default:
		return "", false
	}
}

// machineryScalarSource enumerates the Display-form scalar source strings that
// denote Codex-internal machinery threads.
type machineryScalarSource string

const (
	machineryScalarInternalMemoryConsolidation machineryScalarSource = "internal_memory_consolidation"
	machineryScalarSubagentReview              machineryScalarSource = "subagent_review"
	machineryScalarSubagentCompact             machineryScalarSource = "subagent_compact"
	machineryScalarSubagentMemoryConsolidation machineryScalarSource = "subagent_memory_consolidation"
	machineryScalarMemoryConsolidation         machineryScalarSource = "memory_consolidation"
)

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
	summary.ForkedFromID = payload.ForkedFromID
	summary.IsSubagent = payload.Source.Kind == ThreadSourceSubagent ||
		payload.Source.Kind == ThreadSourceSubagentOld ||
		payload.Source.ParentThreadID != "" ||
		payload.AgentNickname != "" ||
		payload.AgentRole != "" ||
		threadSourceFieldIsMachinery(payload.ThreadSource)
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
