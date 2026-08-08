// Package parser reads standalone GitHub Copilot CLI session event logs.
package parser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

const (
	artifactKind = "copilot_events"
	concern      = "providers.copilot.parser"
)

// Parser implements conversation parsing for Copilot CLI event logs.
type Parser struct{}

var (
	_ conversation.Parser                  = (*Parser)(nil)
	_ conversation.MultiConversationParser = (*Parser)(nil)
)

// New returns a Copilot CLI conversation parser.
func New() *Parser {
	return &Parser{}
}

// Provider reports the provider handled by this parser.
func (*Parser) Provider() providerid.Provider {
	return providerid.ProviderCopilot
}

type event struct {
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Type      eventType       `json:"type"`
	AgentID   string          `json:"agentId"`
	Ephemeral bool            `json:"ephemeral"`
	Data      json.RawMessage `json:"data"`
}

type eventType string

const (
	eventAssistantMessage      eventType = "assistant.message"
	eventAssistantReasoning    eventType = "assistant.reasoning"
	eventCompactionComplete    eventType = "session.compaction_complete"
	eventSessionStart          eventType = "session.start"
	eventSystemMessage         eventType = "system.message"
	eventToolExecutionComplete eventType = "tool.execution_complete"
	eventSubagentStarted       eventType = "subagent.started"
	eventUserMessage           eventType = "user.message"
)

type sessionStart struct {
	SessionID  string `json:"sessionId"`
	StartTime  string `json:"startTime"`
	Model      string `json:"selectedModel"`
	ModelAlt   string `json:"model"`
	CWD        string `json:"cwd"`
	GitRoot    string `json:"gitRoot"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
}

type subagentData struct {
	AgentName        string `json:"agentName"`
	AgentDisplayName string `json:"agentDisplayName"`
	AgentDescription string `json:"agentDescription"`
	Model            string `json:"model"`
	ToolCallID       string `json:"toolCallId"`
}

type attachmentData struct {
	MIMEType       string `json:"mimeType"`
	MimeType       string `json:"mime_type"`
	SizeBytes      int64  `json:"sizeBytes"`
	Size           int64  `json:"size"`
	Description    string `json:"description"`
	AssetReference string `json:"assetReference"`
	Asset          string `json:"asset"`
	Text           string `json:"text"`
}

type attachmentsData struct {
	Attachments []attachmentData `json:"attachments"`
}

// Discover finds Copilot CLI event logs and their root and subagent chats.
func (p *Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	root, err := copilotRoot()
	if err != nil {
		slog.WarnContext(ctx, "providers.copilot.parser.root_failed", "concern", concern, "err", err)
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "session-state", "*", "events.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("find Copilot event logs: %w", err)
	}
	candidates := make([]conversation.ScanCandidate, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("discover Copilot event logs: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat Copilot event log %s: %w", path, err)
		}
		events, err := readEvents(path)
		if err != nil && len(events) == 0 {
			continue
		}
		_, agents := identities(events)
		candidates = append(candidates, conversation.ScanCandidate{
			Path: path, Selector: "", Stamp: conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()},
		})
		for agentID := range agents {
			candidates = append(candidates, conversation.ScanCandidate{
				Path: path, Selector: agentID, Stamp: conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()},
			})
		}
	}
	return candidates, nil
}

// ScanRecord reads the root chat metadata from one Copilot event log.
func (p *Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	return p.scanHeader(conversation.ScanCandidate{Path: path, Selector: "", Stamp: stamp})
}

// ScanRecords reads metadata for the selected chat in one Copilot event log.
func (p *Parser) ScanRecords(candidate conversation.ScanCandidate) ([]conversation.Record, bool) {
	record, ok := p.scanHeader(candidate)
	if !ok {
		return nil, false
	}
	return []conversation.Record{record}, true
}

func (p *Parser) scanHeader(candidate conversation.ScanCandidate) (conversation.Record, bool) {
	file, err := os.Open(candidate.Path)
	if err != nil {
		return emptyRecord(), false
	}
	defer func() { _ = file.Close() }()

	var metadata headerMetadata
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if item, ok := decodeEventLine(line); ok {
			metadata.add(item, candidate.Selector)
		}
		if metadata.ready(candidate.Selector) {
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return emptyRecord(), false
		}
	}
	return scanRecordMetadata(candidate, metadata.start, metadata.firstUser, metadata.created, metadata.subagent), true
}

type headerMetadata struct {
	start     sessionStart
	firstUser string
	created   time.Time
	subagent  subagentData
}

func decodeEventLine(line []byte) (event, bool) {
	var item event
	if json.Unmarshal(line, &item) != nil || item.Type == "" {
		return item, false
	}
	return item, true
}

func (metadata *headerMetadata) add(item event, selector string) {
	if item.Type == eventSessionStart {
		_ = json.Unmarshal(item.Data, &metadata.start)
	}
	if item.AgentID != selector {
		return
	}
	if item.Type == eventUserMessage && metadata.firstUser == "" {
		metadata.firstUser = stringField(item.Data, stringFieldContent)
	}
	if metadata.created.IsZero() {
		metadata.created = parseTime(item.Timestamp)
	}
	if item.Type == eventSubagentStarted {
		_ = json.Unmarshal(item.Data, &metadata.subagent)
	}
}

func (metadata *headerMetadata) ready(selector string) bool {
	if metadata.start.SessionID == "" || metadata.created.IsZero() {
		return false
	}
	if selector == "" {
		return metadata.firstUser != ""
	}
	return metadata.subagent.AgentDisplayName != "" ||
		metadata.subagent.AgentName != "" ||
		metadata.subagent.AgentDescription != ""
}

func scanRecordMetadata(
	candidate conversation.ScanCandidate,
	start sessionStart,
	firstUser string,
	created time.Time,
	subagent subagentData,
) conversation.Record {
	sessionID := start.SessionID
	if sessionID == "" {
		sessionID = filepath.Base(filepath.Dir(candidate.Path))
	}
	if created.IsZero() {
		created = parseTime(start.StartTime)
	}
	nativeID := sessionID
	title := trimTitle(firstUser)
	origin := conversation.OriginUser
	var lineage *conversation.Lineage
	if candidate.Selector != "" {
		nativeID += ":agent:" + candidate.Selector
		origin = conversation.OriginSubagent
		title = firstNonEmpty(subagent.AgentDisplayName, subagent.AgentName, subagent.AgentDescription, title, nativeID)
		lineage = &conversation.Lineage{
			Kind:              conversation.ConversationLineageKindSpawn,
			ParentProvider:    providerid.ProviderCopilot,
			ParentNativeID:    sessionID,
			ParentMessageUUID: "",
		}
	}
	if title == "" {
		title = nativeID
	}
	model := firstNonEmpty(start.Model, start.ModelAlt, subagent.Model)
	workspace := firstNonEmpty(start.CWD, start.GitRoot)
	return conversation.Record{
		ID:              conversation.DerivedID(providerid.ProviderCopilot, nativeID, candidate.Path),
		Provider:        providerid.ProviderCopilot,
		NativeID:        nativeID,
		Selector:        candidate.Selector,
		Lineage:         lineage,
		Origin:          origin,
		Title:           title,
		TitleUncertain:  false,
		WorkspaceRoot:   workspace,
		ArtifactPath:    candidate.Path,
		ArtifactKind:    artifactKind,
		Model:           model,
		CreatedAt:       created,
		UpdatedAt:       candidate.Stamp.Mtime,
		SizeBytes:       candidate.Stamp.Size,
		Archived:        false,
		LatestRequestID: "",
	}
}

func emptyRecord() conversation.Record {
	return conversation.Record{
		ID: "", Provider: providerid.ProviderUnspecified, NativeID: "", Selector: "",
		Lineage: nil, Origin: conversation.OriginUnspecified, Title: "",
		TitleUncertain: false, WorkspaceRoot: "", ArtifactPath: "", ArtifactKind: "",
		Model: "", CreatedAt: time.Time{}, UpdatedAt: time.Time{}, SizeBytes: 0,
		Archived: false, LatestRequestID: "",
	}
}

func emptyMessage() transcript.Message {
	return transcript.Message{
		UUID: "", ParentUUID: "", LogicalParentUUID: "", Role: "",
		Visibility: "", Compaction: nil, Timestamp: time.Time{}, Text: "",
		Thinking: "", HasTools: false, Tools: nil, Attachments: nil,
	}
}

func parentID(item event) string {
	if item.ParentID == nil {
		return ""
	}
	return *item.ParentID
}

// Stream yields the root chat from a Copilot CLI event log.
func (p *Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return p.StreamSelected(path, "", opts)
}

// StreamSelected yields only the selected root or subagent chat.
func (p *Parser) StreamSelected(path string, selector string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		if !opts.IncludeToolOutputs {
			streamWithoutToolOutputs(path, selector, opts, yield)
			return
		}
		events, err := readEvents(path)
		if err != nil {
			yield(emptyMessage(), err)
			return
		}
		messages := make([]transcript.Message, 0)
		for _, item := range events {
			if item.Ephemeral || item.AgentID != selector {
				continue
			}
			message, ok := mapEvent(item, opts)
			if !ok {
				continue
			}
			messages = append(messages, message)
		}
		if opts.IncludeToolOutputs {
			attachToolOutputs(messages, events, selector)
		}
		for _, message := range messages {
			if !yield(message, nil) {
				return
			}
		}
	}
}

func streamWithoutToolOutputs(
	path string,
	selector string,
	opts conversation.LoadOptions,
	yield func(transcript.Message, error) bool,
) {
	file, err := os.Open(path)
	if err != nil {
		yield(emptyMessage(), fmt.Errorf("open Copilot event log: %w", err))
		return
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var item event
		if json.Unmarshal(scanner.Bytes(), &item) != nil || item.Type == "" {
			continue
		}
		if item.Ephemeral || item.AgentID != selector {
			continue
		}
		message, ok := mapEvent(item, opts)
		if ok && !yield(message, nil) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		yield(emptyMessage(), fmt.Errorf("read Copilot event log: %w", err))
	}
}

func mapEvent(item event, opts conversation.LoadOptions) (transcript.Message, bool) {
	timestamp := parseTime(item.Timestamp)
	switch item.Type {
	case eventUserMessage:
		text := stringField(item.Data, stringFieldContent)
		attachments := mapAttachments(item.Data)
		if text == "" && len(attachments) == 0 {
			return emptyMessage(), false
		}
		return transcript.Message{UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "user", Visibility: transcript.MessageVisibilityVisible, Compaction: nil, Timestamp: timestamp, Text: text, Thinking: "", HasTools: false, Tools: nil, Attachments: attachments}, true
	case eventAssistantMessage:
		text := stringField(item.Data, stringFieldContent)
		thinking := firstNonEmpty(stringField(item.Data, stringFieldReasoningText), stringField(item.Data, stringFieldReasoning))
		var calls []transcript.ToolCall
		var raw struct {
			ToolRequests []struct {
				ToolCallID string          `json:"toolCallId"`
				Name       string          `json:"name"`
				Input      json.RawMessage `json:"arguments"`
			} `json:"toolRequests"`
		}
		_ = json.Unmarshal(item.Data, &raw)
		for _, call := range raw.ToolRequests {
			tool := transcript.ToolCall{ID: call.ToolCallID, Name: call.Name, Input: transcript.ToolInputJSON{Raw: append([]byte(nil), call.Input...)}, Display: "", DisplayLang: "", Output: "", IsError: false, Attachments: nil}
			calls = append(calls, tool)
		}
		attachments := mapAttachments(item.Data)
		return transcript.Message{UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "assistant", Visibility: transcript.MessageVisibilityVisible, Compaction: nil, Timestamp: timestamp, Text: text, Thinking: thinking, HasTools: len(calls) > 0, Tools: calls, Attachments: attachments}, text != "" || thinking != "" || len(calls) > 0 || len(attachments) > 0
	case eventAssistantReasoning:
		text := firstNonEmpty(stringField(item.Data, stringFieldContent), stringField(item.Data, stringFieldText))
		if text == "" {
			return emptyMessage(), false
		}
		return transcript.Message{UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "assistant", Visibility: transcript.MessageVisibilityTranscriptOnly, Compaction: nil, Timestamp: timestamp, Text: "", Thinking: text, HasTools: false, Tools: nil, Attachments: nil}, true
	case eventSystemMessage:
		if !opts.IncludeSystemMessages {
			return emptyMessage(), false
		}
		text := stringField(item.Data, stringFieldContent)
		return transcript.Message{UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "system", Visibility: transcript.MessageVisibilityMetaOnly, Compaction: nil, Timestamp: timestamp, Text: text, Thinking: "", HasTools: false, Tools: nil, Attachments: nil}, text != ""
	case eventToolExecutionComplete:
		return emptyMessage(), false
	case eventCompactionComplete:
		var data struct {
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(item.Data, &data)
		if data.Summary == "" {
			return emptyMessage(), false
		}
		return transcript.Message{UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "assistant", Visibility: transcript.MessageVisibilityTranscriptOnly, Compaction: &transcript.CompactionMetadata{Kind: transcript.CompactionKindSummary, Trigger: transcript.CompactionTriggerUnknown, PreTokens: 0, PostTokens: 0, TokensSaved: 0, MessagesSummarized: 0, ReplacementHistoryCount: 0, HeadUUID: "", AnchorUUID: "", TailUUID: "", ContextItems: nil, UserContext: "", Direction: "", PreCompactDiscoveredTools: nil, CompactedToolIDs: nil, ClearedAttachmentUUIDs: nil, RawCompactMetadata: nil, RawMicrocompactMetadata: nil, RawSummarizeMetadata: nil}, Timestamp: timestamp, Text: data.Summary, Thinking: "", HasTools: false, Tools: nil, Attachments: nil}, true
	case eventSessionStart, eventSubagentStarted:
		return emptyMessage(), false
	default:
		return emptyMessage(), false
	}
}

func attachToolOutputs(messages []transcript.Message, events []event, selector string) {
	for _, item := range events {
		if item.Ephemeral || item.AgentID != selector || item.Type != eventToolExecutionComplete {
			continue
		}
		var data struct {
			ToolCallID string `json:"toolCallId"`
			Success    bool   `json:"success"`
			Result     struct {
				Content     string            `json:"content"`
				Contents    []json.RawMessage `json:"contents"`
				Attachments []attachmentData  `json:"attachments"`
			} `json:"result"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(item.Data, &data) != nil || data.ToolCallID == "" {
			continue
		}
		output := data.Result.Content
		if !data.Success {
			output = data.Error.Message
		}
		for messageIndex := range messages {
			for toolIndex := range messages[messageIndex].Tools {
				tool := &messages[messageIndex].Tools[toolIndex]
				if tool.ID == data.ToolCallID {
					tool.Output = output
					tool.IsError = !data.Success
					tool.Attachments = normalizeAttachments(data.Result.Attachments)
				}
			}
		}
	}
}

func mapAttachments(raw json.RawMessage) []transcript.Attachment {
	var data attachmentsData
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	return normalizeAttachments(data.Attachments)
}

func normalizeAttachments(values []attachmentData) []transcript.Attachment {
	attachments := make([]transcript.Attachment, 0, len(values))
	for _, value := range values {
		mimeType := firstNonEmpty(value.MIMEType, value.MimeType)
		sizeBytes := value.SizeBytes
		if sizeBytes == 0 {
			sizeBytes = value.Size
		}
		assetReference := firstNonEmpty(value.AssetReference, value.Asset)
		if mimeType == "" && sizeBytes == 0 && value.Description == "" && assetReference == "" && value.Text == "" {
			continue
		}
		attachments = append(attachments, transcript.Attachment{
			MIMEType: mimeType, SizeBytes: sizeBytes, Description: value.Description,
			AssetReference: assetReference, Text: value.Text,
		})
	}
	return attachments
}

func readEvents(path string) ([]event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Copilot event log: %w", err)
	}
	defer func() { _ = file.Close() }()
	var events []event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var item event
		if json.Unmarshal(line, &item) != nil || item.Type == "" {
			continue
		}
		events = append(events, item)
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("providers.copilot.parser.read_failed", "concern", concern, "path", path, "err", err)
		return events, fmt.Errorf("read Copilot event log: %w", err)
	}
	return events, nil
}

func identities(events []event) (string, map[string]bool) {
	var sessionID string
	agents := make(map[string]bool)
	for _, item := range events {
		if item.Type == eventSessionStart {
			var start sessionStart
			_ = json.Unmarshal(item.Data, &start)
			sessionID = start.SessionID
		}
		if item.AgentID != "" {
			agents[item.AgentID] = true
		}
	}
	return sessionID, agents
}

func copilotRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv("COPILOT_HOME")); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("providers.copilot.parser.home_failed", "concern", concern, "err", err)
		return "", fmt.Errorf("resolve Copilot home: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

func parseTime(raw string) time.Time {
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return value
}

type stringFieldKey string

const (
	stringFieldContent       stringFieldKey = "content"
	stringFieldReasoningText stringFieldKey = "reasoningText"
	stringFieldReasoning     stringFieldKey = "reasoning"
	stringFieldText          stringFieldKey = "text"
)

func stringField(data json.RawMessage, key stringFieldKey) string {
	var fields struct {
		Content       string `json:"content"`
		ReasoningText string `json:"reasoningText"`
		Reasoning     string `json:"reasoning"`
		Text          string `json:"text"`
	}
	if json.Unmarshal(data, &fields) != nil {
		return ""
	}
	switch key {
	case stringFieldContent:
		return fields.Content
	case stringFieldReasoningText:
		return fields.ReasoningText
	case stringFieldReasoning:
		return fields.Reasoning
	case stringFieldText:
		return fields.Text
	}
	return ""
}

func trimTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 120 {
		return text[:120]
	}
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
