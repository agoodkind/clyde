// Package parser reads raw Codex rollout JSONL artifacts and implements the
// [conversation.Parser] interface. It owns all Codex-specific parsing: rollout
// discovery, metadata-only header scanning, and streaming history shaping. It is
// the only conversation parser that calls into the codex store.
package parser

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	concern             = "providers.codex.parser"
	artifactKindRollout = "rollout"
)

// Parser implements [conversation.Parser] for Codex rollouts. Discover resolves
// the store paths and reads the session index once, caching both so the
// per-file ScanRecord can derive the durable thread name and archived flag
// without re-reading them. The conversation index runs one scan at a time, so
// the cached snapshot is consistent within a scan; a mutex guards it against the
// Discover of an overlapping scan generation.
type Parser struct {
	mu      sync.Mutex
	paths   codexstore.StorePaths
	index   codexstore.SessionIndex
	indexOK bool
}

var _ conversation.Parser = (*Parser)(nil)

// New returns a Codex rollout parser.
func New() *Parser {
	return &Parser{
		mu:      sync.Mutex{},
		paths:   codexstore.StorePaths{CodexHome: "", SQLiteHome: "", SessionsDir: "", ArchivedSessionsDir: "", SessionIndexPath: ""},
		index:   codexstore.SessionIndex{},
		indexOK: false,
	}
}

// Provider reports that this parser handles Codex artifacts.
func (*Parser) Provider() providerid.Provider {
	return providerid.ProviderCodex
}

// emptyRecord returns the fully zeroed record used when a rollout yields no
// usable identity, written out so exhaustruct sees every field set.
func emptyRecord() conversation.Record {
	return conversation.Record{
		ID:              "",
		Provider:        providerid.ProviderUnspecified,
		NativeID:        "",
		Selector:        "",
		Lineage:         nil,
		Origin:          conversation.OriginUnspecified,
		Title:           "",
		TitleUncertain:  false,
		WorkspaceRoot:   "",
		ArtifactPath:    "",
		ArtifactKind:    "",
		Model:           "",
		CreatedAt:       time.Time{},
		UpdatedAt:       time.Time{},
		SizeBytes:       0,
		Archived:        false,
		LatestRequestID: "",
	}
}

// emptyMessage returns the fully zeroed message used for a stream error, written
// out so exhaustruct sees every field set.
func emptyMessage() transcript.Message {
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "",
		Visibility:        "",
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
}

// emptyThreadSummary returns the fully zeroed thread summary used when a rollout
// filename cannot be parsed, written out so exhaustruct sees every field set.
func emptyThreadSummary() codexstore.ThreadSummary {
	return codexstore.ThreadSummary{
		ID:            "",
		RolloutPath:   "",
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
		Source:        codexstore.ThreadSource{Kind: "", ParentThreadID: "", AgentNickname: "", AgentRole: ""},
		AgentNickname: "",
		AgentRole:     "",
		IsSubagent:    false,
		IsArchived:    false,
		Messages:      nil,
	}
}

// Discover resolves the Codex store roots, reads the session index once for
// durable thread names, and walks every rollout file into a scan candidate
// without parsing it. prior is unused because the candidate stamps drive the
// reuse decision in the conversation scan driver.
func (p *Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	paths, err := codexstore.ResolveStorePathsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.codex.parser.paths_failed", "concern", concern, "component", "codex", "err", err)
		return nil, fmt.Errorf("resolve Codex store paths: %w", err)
	}
	index, err := codexstore.ReadSessionIndex(ctx, paths.SessionIndexPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.codex.parser.session_index_failed", "concern", concern, "component", "codex", "path", paths.SessionIndexPath, "err", err)
		return nil, fmt.Errorf("read Codex session index: %w", err)
	}
	candidates, err := codexstore.NewDiscoveryScanner(paths).DiscoverCandidates()
	if err != nil {
		slog.WarnContext(ctx, "providers.codex.parser.discover_failed", "concern", concern, "component", "codex", "err", err)
		return nil, fmt.Errorf("discover Codex rollouts: %w", err)
	}
	p.mu.Lock()
	p.paths = paths
	p.index = index
	p.indexOK = true
	p.mu.Unlock()

	out := make([]conversation.ScanCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, conversation.ScanCandidate{
			Path:     candidate.Path,
			Selector: "",
			Stamp:    conversation.FileStamp{Size: candidate.Stamp.Size, Mtime: candidate.Stamp.Mtime},
		})
	}
	return out, nil
}

// ScanRecord reads only the rollout header (no message history) and derives the
// record. It looks up the durable thread name from the session index cached by
// Discover and derives the archived flag from the rollout path, so it never
// reads the session index or the message body. Parentless subagent machinery
// yields no record.
func (p *Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	p.mu.Lock()
	paths := p.paths
	index := p.index
	indexOK := p.indexOK
	p.mu.Unlock()

	archived := p.isArchivedPath(paths, path)
	thread, err := codexstore.ReadHeader(path, archived)
	if err != nil || thread.ID == "" {
		// A rollout too short or malformed to parse a header still gets a record
		// from its filename identity, matching the prior incremental scanner.
		fallback, ok := fallbackThread(path, archived)
		if !ok {
			if err == nil {
				err = fmt.Errorf("codex rollout %s missing session_meta id", path)
			}
			slog.Warn("providers.codex.parser.header_failed", "concern", concern, "component", "codex", "path", path, "err", err)
			return emptyRecord(), false
		}
		thread = fallback
	} else {
		thread.UpdatedAt = stamp.Mtime
	}
	if thread.IsSubagent && thread.Source.ParentThreadID == "" && thread.ForkedFromID == "" {
		// Review and compact subagent machinery has no parent thread, while
		// spawned child threads and forks carry lineage.
		return emptyRecord(), false
	}
	var lineage *conversation.Lineage
	if thread.ForkedFromID != "" {
		lineage = &conversation.Lineage{
			Kind:              conversation.ConversationLineageKindFork,
			ParentProvider:    providerid.ProviderCodex,
			ParentNativeID:    thread.ForkedFromID,
			ParentMessageUUID: "",
		}
	} else if thread.Source.ParentThreadID != "" {
		lineage = &conversation.Lineage{
			Kind:              conversation.ConversationLineageKindSpawn,
			ParentProvider:    providerid.ProviderCodex,
			ParentNativeID:    thread.Source.ParentThreadID,
			ParentMessageUUID: "",
		}
	}
	// session_meta classifies the thread, and the spawn lineage above already
	// carries the parent thread id, so the origin does not repeat it.
	origin := conversation.OriginUser
	if thread.IsSubagent {
		origin = conversation.OriginSubagent
	}
	name := ""
	if indexOK {
		name = index.ThreadName(thread.ID)
	}
	title := name
	if title == "" {
		title = thread.ID
	}
	workspaceRoot := firstNonEmpty(thread.LatestCWD, thread.CWD)
	return conversation.Record{
		ID:             conversation.DerivedID(providerid.ProviderCodex, thread.ID, thread.RolloutPath),
		Provider:       providerid.ProviderCodex,
		NativeID:       thread.ID,
		Selector:       "",
		Lineage:        lineage,
		Origin:         origin,
		Title:          title,
		TitleUncertain: false,
		WorkspaceRoot:  workspaceRoot,
		ArtifactPath:   thread.RolloutPath,
		ArtifactKind:   artifactKindRollout,
		Model:          thread.ModelProvider,
		CreatedAt:      thread.CreatedAt,
		UpdatedAt:      thread.UpdatedAt,
		SizeBytes:      stamp.Size,
		Archived:       thread.IsArchived,
		// Codex rollout headers carry no request id.
		LatestRequestID: "",
	}, true
}

// Stream yields the rollout's conversation messages one at a time. With
// IncludeToolOutputs unset it stays streaming and drops the tool-output marker
// records (they carry no renderable text). With IncludeToolOutputs set it
// buffers so each output can be attached to its tool call by call_id, matching
// the Claude parser's two-pass behavior.
func (p *Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	if opts.IncludeToolOutputs {
		return p.streamWithToolOutputs(path, opts)
	}
	return func(yield func(transcript.Message, error) bool) {
		streamOpts := codexstore.HistoryOptions{
			IncludeSystemMessages: opts.IncludeSystemMessages,
			IncludeSystemPrompts:  opts.IncludeSystemPrompts,
			IncludeInjected:       opts.IncludeInjected,
			Tally:                 opts.HarnessTally,
		}
		for msg, err := range codexstore.StreamMessages(path, streamOpts) {
			if err != nil {
				yield(emptyMessage(), fmt.Errorf("read codex rollout: %w", err))
				return
			}
			message, ok := codexTranscriptMessage(msg, opts.IncludeSystemMessages)
			if !ok {
				continue
			}
			if !yield(message, nil) {
				return
			}
		}
	}
}

// streamWithToolOutputs collects the whole rollout, links each tool-output
// record to its tool call by call_id, and then yields the shaped messages.
func (p *Parser) streamWithToolOutputs(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		var history []codexstore.HistoryMessage
		streamOpts := codexstore.HistoryOptions{
			IncludeSystemMessages: opts.IncludeSystemMessages,
			IncludeSystemPrompts:  opts.IncludeSystemPrompts,
			IncludeInjected:       opts.IncludeInjected,
			Tally:                 opts.HarnessTally,
		}
		for msg, err := range codexstore.StreamMessages(path, streamOpts) {
			if err != nil {
				yield(emptyMessage(), fmt.Errorf("read codex rollout: %w", err))
				return
			}
			history = append(history, msg)
		}
		for _, message := range attachCodexToolOutputs(history, opts.IncludeSystemMessages) {
			if !yield(message, nil) {
				return
			}
		}
	}
}

// codexToolRef locates one tool call inside the shaped message slice so a later
// tool-output record can fill its output by call_id.
type codexToolRef struct {
	messageIndex int
	toolIndex    int
}

// attachCodexToolOutputs shapes the buffered history into transcript messages
// and folds each tool-output marker into the matching tool call's Output.
func attachCodexToolOutputs(history []codexstore.HistoryMessage, includeSystemMessages bool) []transcript.Message {
	out := make([]transcript.Message, 0, len(history))
	index := make(map[string]codexToolRef, len(history))
	for _, hm := range history {
		if hm.ToolOutputCallID != "" {
			if ref, ok := index[hm.ToolOutputCallID]; ok {
				out[ref.messageIndex].Tools[ref.toolIndex].Output = hm.ToolOutput
				out[ref.messageIndex].Tools[ref.toolIndex].IsError = hm.ToolOutputIsError
			}
			continue
		}
		message, ok := codexTranscriptMessage(hm, includeSystemMessages)
		if !ok {
			continue
		}
		messageIndex := len(out)
		out = append(out, message)
		for toolIndex := range message.Tools {
			callID := message.Tools[toolIndex].ID
			if callID == "" {
				continue
			}
			index[callID] = codexToolRef{messageIndex: messageIndex, toolIndex: toolIndex}
		}
	}
	return out
}

// fallbackThread builds a minimal thread summary from a rollout filename when
// the file itself cannot be read, preserving the prior scanner's behavior of
// keeping a record for unreadable rollouts.
func fallbackThread(path string, archived bool) (codexstore.ThreadSummary, bool) {
	id, createdAt, ok := codexstore.RolloutIdentityFromFilename(filepath.Base(path))
	if !ok {
		return emptyThreadSummary(), false
	}
	summary := codexstore.ThreadSummary{
		ID:            id,
		RolloutPath:   path,
		ForkedFromID:  "",
		Preview:       "",
		Name:          "",
		ModelProvider: "",
		CreatedAt:     createdAt,
		UpdatedAt:     time.Time{},
		CWD:           "",
		LatestCWD:     "",
		CLIVersion:    "",
		Originator:    "",
		Source:        codexstore.ThreadSource{Kind: "", ParentThreadID: "", AgentNickname: "", AgentRole: ""},
		AgentNickname: "",
		AgentRole:     "",
		IsSubagent:    false,
		IsArchived:    archived,
		Messages:      nil,
	}
	return summary, true
}

// isArchivedPath reports whether a rollout path lives under the archived
// sessions root, which sets the record's archived flag without opening the file.
func (*Parser) isArchivedPath(paths codexstore.StorePaths, path string) bool {
	if paths.ArchivedSessionsDir == "" {
		return false
	}
	return strings.HasPrefix(path, paths.ArchivedSessionsDir)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
