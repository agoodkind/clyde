// Package parser reads standalone GitHub Copilot CLI session event logs.
package parser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

const (
	artifactKind           = "copilot_events"
	concern                = "providers.copilot.parser"
	scanRecordLineLimit    = 128
	supportedSchemaVersion = 1
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
	Version                             int                     `json:"version"`
	SessionID                           string                  `json:"sessionId"`
	StartTime                           string                  `json:"startTime"`
	SelectedModel                       string                  `json:"selectedModel"`
	DetachedFromSpawningParentSessionID string                  `json:"detachedFromSpawningParentSessionId"`
	Context                             workingDirectoryContext `json:"context"`
}

type workingDirectoryContext struct {
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

// Discover finds one candidate for each physical Copilot event log.
func (*Parser) Discover(
	ctx context.Context,
	_ map[string]conversation.Record,
) ([]conversation.ScanCandidate, error) {
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
		candidates = append(candidates, conversation.ScanCandidate{
			Path:     path,
			Selector: "",
			Stamp:    conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()},
		})
	}
	return candidates, nil
}

// ScanRecord reads the root record from one Copilot event log.
func (*Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	candidate := conversation.ScanCandidate{Path: path, Selector: "", Stamp: stamp}
	return scanRootRecord(candidate, func(visit func(event) bool) error {
		_, err := readCompleteEvents(path, 0, scanRecordLineLimit, visit)
		return err
	})
}

func scanRootRecord(
	candidate conversation.ScanCandidate,
	read func(visit func(event) bool) error,
) (conversation.Record, bool) {
	metadata := newScanMetadata(nil)
	err := read(func(item event) bool {
		metadata.add(item)
		if metadata.start.Version != 0 && metadata.start.Version != supportedSchemaVersion {
			return false
		}
		return metadata.start.SessionID == "" || metadata.chat("").firstUser == ""
	})
	if err != nil || metadata.start.Version != supportedSchemaVersion || metadata.start.SessionID == "" {
		return emptyRecord(), false
	}
	return buildRecord(candidate, metadata.start, "", *metadata.chat("")), true
}

// ScanRecords reads every root and subagent record from one physical artifact.
func (*Parser) ScanRecords(
	input conversation.MultiConversationScan,
) (conversation.MultiConversationScanResult, bool) {
	metadata := newScanMetadata(input.PriorRecords)
	completeOffset, err := readCompleteEvents(input.Candidate.Path, input.StartOffset, 0, func(item event) bool {
		metadata.add(item)
		return true
	})
	if err != nil {
		return conversation.MultiConversationScanResult{Records: nil, CompleteOffset: input.StartOffset}, false
	}
	if metadata.start.Version != supportedSchemaVersion || metadata.start.SessionID == "" {
		return conversation.MultiConversationScanResult{Records: nil, CompleteOffset: completeOffset}, false
	}
	records := metadata.records(input.Candidate)
	if len(records) == 0 {
		return conversation.MultiConversationScanResult{Records: nil, CompleteOffset: completeOffset}, false
	}
	return conversation.MultiConversationScanResult{Records: records, CompleteOffset: completeOffset}, true
}

type chatMetadata struct {
	firstUser string
	created   time.Time
	subagent  subagentData
}

type scanMetadata struct {
	start           sessionStart
	chats           map[string]*chatMetadata
	priorBySelector map[string]conversation.Record
}

func newScanMetadata(prior []conversation.Record) *scanMetadata {
	metadata := &scanMetadata{
		start: sessionStart{
			Version: 0, SessionID: "", StartTime: "", SelectedModel: "",
			DetachedFromSpawningParentSessionID: "",
			Context:                             workingDirectoryContext{CWD: "", GitRoot: "", Repository: "", Branch: ""},
		},
		chats:           make(map[string]*chatMetadata),
		priorBySelector: make(map[string]conversation.Record, len(prior)),
	}
	for _, record := range prior {
		firstUser := record.Title
		if record.TitleUncertain {
			firstUser = ""
		}
		metadata.priorBySelector[record.Selector] = record
		metadata.chats[record.Selector] = &chatMetadata{
			firstUser: firstUser,
			created:   record.CreatedAt,
			subagent: subagentData{
				AgentName: "", AgentDisplayName: "", AgentDescription: "",
				Model: record.Model, ToolCallID: parentMessageUUID(record),
			},
		}
		if record.Selector != "" {
			continue
		}
		metadata.start.Version = supportedSchemaVersion
		metadata.start.SessionID = record.NativeID
		metadata.start.StartTime = record.CreatedAt.Format(time.RFC3339Nano)
		metadata.start.SelectedModel = record.Model
		metadata.start.Context.GitRoot = record.WorkspaceRoot
		if record.Lineage != nil {
			metadata.start.DetachedFromSpawningParentSessionID = record.Lineage.ParentNativeID
		}
	}
	return metadata
}

func parentMessageUUID(record conversation.Record) string {
	if record.Lineage == nil {
		return ""
	}
	return record.Lineage.ParentMessageUUID
}

func (metadata *scanMetadata) add(item event) {
	if item.Ephemeral {
		return
	}
	if item.Type == eventSessionStart {
		var start sessionStart
		if json.Unmarshal(item.Data, &start) == nil {
			metadata.start = start
		}
	}
	chat := metadata.chat(item.AgentID)
	if chat.created.IsZero() {
		chat.created = parseTime(item.Timestamp)
	}
	if item.Type == eventUserMessage && chat.firstUser == "" {
		var data userMessageData
		if json.Unmarshal(item.Data, &data) == nil && !isSkillSource(data.Source) {
			chat.firstUser = data.Content
		}
	}
	if item.Type == eventSubagentStarted && item.AgentID != "" {
		_ = json.Unmarshal(item.Data, &chat.subagent)
	}
}

func (metadata *scanMetadata) chat(selector string) *chatMetadata {
	chat, ok := metadata.chats[selector]
	if ok {
		return chat
	}
	chat = &chatMetadata{
		firstUser: "",
		created:   time.Time{},
		subagent: subagentData{
			AgentName: "", AgentDisplayName: "", AgentDescription: "",
			Model: "", ToolCallID: "",
		},
	}
	metadata.chats[selector] = chat
	return chat
}

func (metadata *scanMetadata) records(candidate conversation.ScanCandidate) []conversation.Record {
	selectors := make([]string, 0, len(metadata.chats))
	for selector := range metadata.chats {
		selectors = append(selectors, selector)
	}
	sort.Strings(selectors)
	if len(selectors) == 0 || selectors[0] != "" {
		selectors = append([]string{""}, selectors...)
	}
	records := make([]conversation.Record, 0, len(selectors))
	for _, selector := range selectors {
		chat := metadata.chat(selector)
		if prior, ok := metadata.priorBySelector[selector]; ok {
			prior.UpdatedAt = candidate.Stamp.Mtime
			prior.SizeBytes = candidate.Stamp.Size
			if prior.TitleUncertain && chat.firstUser != "" {
				prior.Title = trimTitle(chat.firstUser)
				prior.TitleUncertain = false
			}
			if selector != "" && chat.subagent.Model != "" {
				prior.Model = chat.subagent.Model
			}
			records = append(records, prior)
			continue
		}
		records = append(records, buildRecord(candidate, metadata.start, selector, *chat))
	}
	return records
}

func buildRecord(
	candidate conversation.ScanCandidate,
	start sessionStart,
	selector string,
	chat chatMetadata,
) conversation.Record {
	nativeID := start.SessionID
	origin := conversation.OriginUser
	var lineage *conversation.Lineage
	model := start.SelectedModel
	title := trimTitle(chat.firstUser)
	if selector == "" && start.DetachedFromSpawningParentSessionID != "" {
		origin = conversation.OriginSubagent
		lineage = &conversation.Lineage{
			Kind:              conversation.ConversationLineageKindSpawn,
			ParentProvider:    providerid.ProviderCopilot,
			ParentNativeID:    start.DetachedFromSpawningParentSessionID,
			ParentMessageUUID: "",
		}
	}
	if selector != "" {
		nativeID += ":agent:" + selector
		origin = conversation.OriginSubagent
		model = firstNonEmpty(chat.subagent.Model, start.SelectedModel)
		title = firstNonEmpty(
			chat.subagent.AgentDisplayName,
			chat.subagent.AgentName,
			chat.subagent.AgentDescription,
			title,
		)
		lineage = &conversation.Lineage{
			Kind:              conversation.ConversationLineageKindSpawn,
			ParentProvider:    providerid.ProviderCopilot,
			ParentNativeID:    start.SessionID,
			ParentMessageUUID: chat.subagent.ToolCallID,
		}
	}
	titleUncertain := title == ""
	if titleUncertain {
		title = nativeID
	}
	created := chat.created
	if selector == "" {
		startTime := parseTime(start.StartTime)
		if !startTime.IsZero() {
			created = startTime
		}
	}
	if created.IsZero() {
		created = parseTime(start.StartTime)
	}
	return conversation.Record{
		ID:              conversation.DerivedID(providerid.ProviderCopilot, nativeID, candidate.Path),
		Provider:        providerid.ProviderCopilot,
		NativeID:        nativeID,
		Selector:        selector,
		Lineage:         lineage,
		Origin:          origin,
		Title:           title,
		TitleUncertain:  titleUncertain,
		WorkspaceRoot:   firstNonEmpty(start.Context.GitRoot, start.Context.CWD),
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

func readCompleteEvents(
	path string,
	startOffset int64,
	maxCompleteLines int,
	visit func(event) bool,
) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		wrapped := fmt.Errorf("open Copilot event log: %w", err)
		slog.Warn("providers.copilot.parser.read_failed", "concern", concern, "path", path, "err", wrapped)
		return startOffset, wrapped
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		wrapped := fmt.Errorf("seek Copilot event log: %w", err)
		slog.Warn("providers.copilot.parser.read_failed", "concern", concern, "path", path, "err", wrapped)
		return startOffset, wrapped
	}
	reader := bufio.NewReader(file)
	completeOffset := startOffset
	completeLines := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if bytes.HasSuffix(line, []byte{'\n'}) {
			completeOffset += int64(len(line))
			completeLines++
			if item, ok := decodeEventLine(line); ok {
				if !visit(item) {
					return completeOffset, nil
				}
			}
			if maxCompleteLines > 0 && completeLines >= maxCompleteLines {
				return completeOffset, nil
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return completeOffset, nil
		}
		wrapped := fmt.Errorf("read Copilot event log: %w", readErr)
		slog.Warn("providers.copilot.parser.read_failed", "concern", concern, "path", path, "err", wrapped)
		return completeOffset, wrapped
	}
}

func decodeEventLine(line []byte) (event, bool) {
	var item event
	if json.Unmarshal(line, &item) != nil || item.Type == "" {
		return item, false
	}
	return item, true
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

// Stream yields the root chat from a Copilot CLI event log.
func (p *Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return p.StreamSelected(path, "", opts)
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
