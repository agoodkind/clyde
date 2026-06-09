// Package parser reads raw Claude Code transcript JSONL artifacts and implements
// the [conversation.Parser] interface. It owns all Claude-specific parsing:
// header discovery, streaming message parse, and tool-output attachment.
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
	concern                = "providers.claude.parser"
	artifactKindTranscript = "transcript"
	// headerLineCap bounds how many lines ScanRecord reads before giving up on a
	// late custom title. The session header and first user message sit in the
	// first lines, so the early stop almost always fires well before this cap.
	headerLineCap = 256
	// scanBufferMax bounds a single transcript line during scanning.
	scanBufferMax = 4 * 1024 * 1024
)

// Parser implements [conversation.Parser] for Claude transcripts.
type Parser struct{}

var _ conversation.Parser = Parser{}

// New returns a Claude transcript parser.
func New() Parser {
	return Parser{}
}

// Provider reports that this parser handles Claude artifacts.
func (Parser) Provider() providerid.Provider {
	return providerid.ProviderClaude
}

// emptyRecord returns the fully zeroed record used when an artifact yields no
// usable identity, written out so exhaustruct sees every field set.
func emptyRecord() conversation.Record {
	return conversation.Record{
		ID:            "",
		Provider:      providerid.ProviderUnspecified,
		NativeID:      "",
		Lineage:       nil,
		Title:         "",
		WorkspaceRoot: "",
		ArtifactPath:  "",
		ArtifactKind:  "",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}
}

type claudeHeader struct {
	SessionID   string            `json:"sessionId"`
	CWD         string            `json:"cwd"`
	Timestamp   string            `json:"timestamp"`
	Type        string            `json:"type"`
	Content     string            `json:"content"`
	CustomTitle string            `json:"customTitle"`
	ForkedFrom  *claudeForkedFrom `json:"forkedFrom"`
}

type claudeForkedFrom struct {
	SessionID   string `json:"sessionId"`
	MessageUUID string `json:"messageUuid"`
}

// Discover walks ~/.claude/projects and returns every transcript file as a scan
// candidate. The scan driver consults the stamps to skip unchanged files. prior
// is unused here because the walk restats every file.
func (Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.WarnContext(ctx, "providers.claude.parser.home_failed", "concern", concern, "component", "claude", "err", err)
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	root := filepath.Join(home, ".claude", "projects")
	var out []conversation.ScanCandidate
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "providers.claude.parser.canceled", "concern", concern, "component", "claude", "err", ctx.Err())
			return fmt.Errorf("discover Claude conversations canceled: %w", ctx.Err())
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrPermission) || errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			slog.WarnContext(ctx, "providers.claude.parser.walk_entry_failed", "concern", concern, "component", "claude", "path", path, "err", walkErr)
			return fmt.Errorf("walk Claude transcript entry: %w", walkErr)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		if strings.Contains(path, string(os.PathSeparator)+"subagents"+string(os.PathSeparator)) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil
			}
			slog.WarnContext(ctx, "providers.claude.parser.stat_failed", "concern", concern, "component", "claude", "path", path, "err", infoErr)
			return nil
		}
		out = append(out, conversation.ScanCandidate{
			Path:  path,
			Stamp: conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()},
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		slog.WarnContext(ctx, "providers.claude.parser.walk_failed", "concern", concern, "component", "claude", "root", root, "err", err)
		return nil, fmt.Errorf("walk Claude transcript root: %w", err)
	}
	return out, nil
}

// ScanRecord reads only the top of a transcript and stops the moment it has the
// session id, a title, and a created time. It never reads to EOF and is bounded
// by headerLineCap so a late custom title cannot force a full read.
func (Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	file, err := os.Open(path)
	if err != nil {
		return emptyRecord(), false
	}
	defer func() { _ = file.Close() }()
	return scanHeader(file, path, stamp)
}

// headerFields holds the record-shaping fields gathered from a transcript
// header. providerID is required; the rest are best-effort.
type headerFields struct {
	providerID            string
	title                 string
	workspaceRoot         string
	createdAt             time.Time
	forkedFromSessionID   string
	forkedFromMessageUUID string
}

// scanHeader reads transcript lines from r and stops the moment it has the
// session id, a title, and a created time, or after headerLineCap lines. It is
// the reader-based core of [Parser.ScanRecord] so a test can wrap r to assert
// the read stays bounded and never reaches EOF.
func scanHeader(r io.Reader, path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	fields := headerFields{
		providerID:            "",
		title:                 "",
		workspaceRoot:         "",
		createdAt:             time.Time{},
		forkedFromSessionID:   "",
		forkedFromMessageUUID: "",
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), scanBufferMax)
	lines := 0
	for scanner.Scan() {
		lines++
		if lines > headerLineCap {
			break
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var header claudeHeader
		if err := json.Unmarshal(line, &header); err != nil {
			continue
		}
		applyHeaderLine(&fields, header)
		// Stop early once the id, a title, and a created time are filled. The
		// first user message supplies a good title, so reading further is
		// wasteful for the common case.
		if fields.providerID != "" && fields.title != "" && !fields.createdAt.IsZero() {
			break
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		// A scan error (such as an over-long line) leaves the fields collected so
		// far intact, so keep the partial record rather than dropping it.
		slog.Warn("providers.claude.parser.scan_failed", "concern", concern, "component", "claude", "path", path, "err", scanErr)
	}
	if fields.providerID == "" {
		return emptyRecord(), false
	}
	title := fields.title
	if title == "" {
		title = fields.providerID
	}
	var lineage *conversation.Lineage
	if fields.forkedFromSessionID != "" {
		lineage = &conversation.Lineage{
			Kind:              conversation.ConversationLineageKindFork,
			ParentProvider:    providerid.ProviderClaude,
			ParentNativeID:    fields.forkedFromSessionID,
			ParentMessageUUID: fields.forkedFromMessageUUID,
		}
	}
	return conversation.Record{
		ID:            conversation.DerivedID(providerid.ProviderClaude, fields.providerID, path),
		Provider:      providerid.ProviderClaude,
		NativeID:      fields.providerID,
		Lineage:       lineage,
		Title:         title,
		WorkspaceRoot: fields.workspaceRoot,
		ArtifactPath:  path,
		ArtifactKind:  artifactKindTranscript,
		Model:         "",
		CreatedAt:     fields.createdAt,
		UpdatedAt:     stamp.Mtime,
		SizeBytes:     stamp.Size,
		Archived:      false,
	}, true
}

// applyHeaderLine folds one decoded header line into the accumulating fields,
// keeping the first session id, cwd, and created time and preferring a custom
// title over the first user message.
func applyHeaderLine(fields *headerFields, header claudeHeader) {
	if header.SessionID != "" && fields.providerID == "" {
		fields.providerID = header.SessionID
	}
	if header.ForkedFrom != nil && header.ForkedFrom.SessionID != "" && fields.forkedFromSessionID == "" {
		fields.forkedFromSessionID = header.ForkedFrom.SessionID
		fields.forkedFromMessageUUID = header.ForkedFrom.MessageUUID
	}
	if header.CWD != "" && fields.workspaceRoot == "" {
		fields.workspaceRoot = header.CWD
	}
	if header.CustomTitle != "" {
		fields.title = header.CustomTitle
	}
	if header.Type == "user" && fields.title == "" && strings.TrimSpace(header.Content) != "" {
		fields.title = trimTitle(header.Content)
	}
	if header.Timestamp != "" && fields.createdAt.IsZero() {
		if parsed, parseErr := time.Parse(time.RFC3339, header.Timestamp); parseErr == nil {
			fields.createdAt = parsed
		}
	}
}

// Stream yields one parsed message at a time. With IncludeToolOutputs unset it
// holds at most one message in flight, so a caller reading a window can stop
// early. With IncludeToolOutputs set it must buffer to attach tool results that
// appear on later lines, matching the prior two-pass behavior.
func (p Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	if opts.IncludeToolOutputs {
		return p.streamWithToolOutputs(path, opts)
	}
	return func(yield func(transcript.Message, error) bool) {
		file, err := os.Open(path)
		if err != nil {
			slog.Warn("providers.claude.parser.open_failed", "concern", concern, "component", "claude", "path", path, "err", err)
			yield(emptyMessage(), fmt.Errorf("open transcript: %w", err))
			return
		}
		defer func() { _ = file.Close() }()
		parseOpts := parseOptions{
			PreserveSystemPrompts: opts.IncludeSystemPrompts,
			IncludeSystemMessages: opts.IncludeSystemMessages,
		}
		reader := bufio.NewReader(file)
		for {
			line, readErr := reader.ReadBytes('\n')
			if stop := emitLine(line, parseOpts, yield); stop {
				return
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				slog.Warn("providers.claude.parser.read_line_failed", "concern", concern, "component", "claude", "path", path, "err", readErr)
				yield(emptyMessage(), fmt.Errorf("read transcript line: %w", readErr))
				return
			}
		}
	}
}

// emitLine parses one raw transcript line and, when it is a usable turn, yields
// it. It returns true when the caller stopped the range so the stream loop can
// return. A blank or non-turn line yields nothing and does not stop the loop.
func emitLine(line []byte, opts parseOptions, yield func(transcript.Message, error) bool) bool {
	if len(line) == 0 {
		return false
	}
	trimmed := trimLine(line)
	if len(trimmed) == 0 {
		return false
	}
	msg, ok := parseLine(trimmed, opts)
	if !ok {
		return false
	}
	return !yield(msg, nil)
}

func (p Parser) streamWithToolOutputs(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		messages, err := p.collectMessages(path, opts)
		if err != nil {
			yield(emptyMessage(), err)
			return
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			slog.Warn("providers.claude.parser.tool_output_read_failed", "concern", concern, "component", "claude", "path", path, "err", readErr)
			yield(emptyMessage(), fmt.Errorf("read transcript for tool outputs: %w", readErr))
			return
		}
		attachToolOutputs(body, messages)
		for _, msg := range messages {
			if !yield(msg, nil) {
				return
			}
		}
	}
}

func (p Parser) collectMessages(path string, opts conversation.LoadOptions) ([]transcript.Message, error) {
	noOutputs := opts
	noOutputs.IncludeToolOutputs = false
	messages, err := conversation.CollectMessages(p.Stream(path, noOutputs))
	if err != nil {
		slog.Warn("providers.claude.parser.collect_failed", "concern", concern, "component", "claude", "path", path, "err", err)
		return messages, fmt.Errorf("collect claude transcript messages: %w", err)
	}
	return messages, nil
}

func trimLine(line []byte) []byte {
	end := len(line)
	for end > 0 && (line[end-1] == '\n' || line[end-1] == '\r') {
		end--
	}
	return line[:end]
}

func trimTitle(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	title := strings.Join(fields, " ")
	if len(title) > 80 {
		return strings.TrimSpace(title[:80])
	}
	return title
}
