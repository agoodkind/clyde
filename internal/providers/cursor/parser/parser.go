// Package parser reads Cursor agent transcript JSONL artifacts.
package parser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	concern                  = "providers.cursor.parser"
	artifactKindAgentLogJSON = "agent-transcript"
)

// Parser implements [conversation.Parser] for Cursor agent transcripts.
type Parser struct {
	HomeDir string
}

var _ conversation.Parser = (*Parser)(nil)

// New returns a Cursor transcript parser.
func New() *Parser {
	return &Parser{HomeDir: ""}
}

// Provider reports that this parser handles Cursor artifacts.
func (*Parser) Provider() providerid.Provider {
	return providerid.ProviderCursor
}

// Discover walks Cursor's user transcript store without parsing file bodies.
func (p *Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	root, err := p.projectsRoot()
	if err != nil {
		return nil, err
	}
	var out []conversation.ScanCandidate
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) || os.IsPermission(walkErr) {
				slog.WarnContext(ctx, "providers.cursor.parser.walk_skipped", "concern", concern, "component", "cursor", "path", path, "err", walkErr)
				return nil
			}
			return fmt.Errorf("walk Cursor transcript path %s: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("discover Cursor transcripts canceled: %w", err)
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if !strings.Contains(path, string(filepath.Separator)+"agent-transcripts"+string(filepath.Separator)) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.stat_failed", "concern", concern, "component", "cursor", "path", path, "err", err)
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return fmt.Errorf("stat Cursor transcript %s: %w", path, err)
		}
		out = append(out, conversation.ScanCandidate{
			Path: path,
			Stamp: conversation.FileStamp{
				Size:  info.Size(),
				Mtime: info.ModTime(),
			},
		})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("discover Cursor transcripts: %w", err)
	}
	return out, nil
}

// ScanRecord derives a record from the Cursor transcript file path and stamp.
func (*Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	nativeID := cursorNativeID(path)
	if nativeID == "" {
		return emptyRecord(), false
	}
	return conversation.Record{
		ID:            conversation.DerivedID(providerid.ProviderCursor, nativeID, path),
		Provider:      providerid.ProviderCursor,
		NativeID:      nativeID,
		Lineage:       nil,
		Title:         nativeID,
		WorkspaceRoot: cursorWorkspaceRoot(path),
		ArtifactPath:  path,
		ArtifactKind:  artifactKindAgentLogJSON,
		Model:         "",
		CreatedAt:     stamp.Mtime,
		UpdatedAt:     stamp.Mtime,
		SizeBytes:     stamp.Size,
		Archived:      false,
	}, true
}

// Stream yields Cursor transcript messages one at a time.
func (*Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		file, err := os.Open(path)
		if err != nil {
			yield(emptyMessage(), fmt.Errorf("open Cursor transcript %s: %w", path, err))
			return
		}
		defer func() { _ = file.Close() }()

		info, statErr := file.Stat()
		if statErr != nil {
			yield(emptyMessage(), fmt.Errorf("stat Cursor transcript %s: %w", path, statErr))
			return
		}
		fallbackTimestamp := info.ModTime()
		scanner := bufio.NewScanner(file)
		const maxScannerBuffer = 16 * 1024 * 1024
		scanner.Buffer(make([]byte, 0, 64*1024), maxScannerBuffer)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			message, ok, err := parseCursorLine(line)
			if err != nil {
				yield(emptyMessage(), fmt.Errorf("parse Cursor transcript %s:%d: %w", path, lineNumber, err))
				return
			}
			if !ok {
				continue
			}
			if message.Timestamp.IsZero() {
				message.Timestamp = fallbackTimestamp
			}
			if !opts.IncludeSystemMessages && message.Role == "system" {
				continue
			}
			if !yield(message, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(emptyMessage(), fmt.Errorf("read Cursor transcript %s: %w", path, err))
			return
		}
	}
}

func (p *Parser) projectsRoot() (string, error) {
	home := strings.TrimSpace(p.HomeDir)
	if home == "" {
		detected, err := os.UserHomeDir()
		if err != nil {
			wrapped := fmt.Errorf("resolve home dir: %w", err)
			slog.Warn("providers.cursor.parser.home_failed", "concern", concern, "component", "cursor", "err", wrapped)
			return "", wrapped
		}
		home = detected
	}
	return filepath.Join(home, ".cursor", "projects"), nil
}

type cursorJSONLLine struct {
	Role    string        `json:"role"`
	Message cursorMessage `json:"message"`
}

type cursorMessage struct {
	Content []cursorContent `json:"content"`
}

type cursorContent struct {
	Type  cursorContentType `json:"type"`
	Text  string            `json:"text"`
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Input json.RawMessage   `json:"input"`
}

type cursorContentType string

const (
	cursorContentTypeText    cursorContentType = "text"
	cursorContentTypeToolUse cursorContentType = "tool_use"
)

func parseCursorLine(line string) (transcript.Message, bool, error) {
	var raw cursorJSONLLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return emptyMessage(), false, err
	}
	role := strings.TrimSpace(raw.Role)
	if role == "" {
		return emptyMessage(), false, nil
	}
	textParts := make([]string, 0, len(raw.Message.Content))
	tools := make([]transcript.ToolCall, 0, len(raw.Message.Content))
	for _, content := range raw.Message.Content {
		switch content.Type {
		case cursorContentTypeText:
			if strings.TrimSpace(content.Text) != "" {
				textParts = append(textParts, content.Text)
			}
		case cursorContentTypeToolUse:
			tools = append(tools, transcript.ToolCall{
				ID:      content.ID,
				Name:    content.Name,
				Input:   transcript.ToolInputJSON{Raw: append(json.RawMessage(nil), content.Input...)},
				Output:  "",
				IsError: false,
			})
		default:
			continue
		}
	}
	if len(textParts) == 0 && len(tools) == 0 {
		return emptyMessage(), false, nil
	}
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              strings.Join(textParts, "\n\n"),
		Thinking:          "",
		HasTools:          len(tools) > 0,
		Tools:             tools,
	}, true, nil
}

func cursorNativeID(path string) string {
	if filepath.Ext(path) != ".jsonl" {
		return ""
	}
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return strings.TrimSpace(id)
}

func cursorWorkspaceRoot(path string) string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for index, part := range parts {
		if part != "projects" || index+1 >= len(parts) {
			continue
		}
		projectSegment := parts[index+1]
		if strings.HasPrefix(projectSegment, "Users-") {
			return string(filepath.Separator) + strings.ReplaceAll(projectSegment, "-", string(filepath.Separator))
		}
		return projectSegment
	}
	return ""
}

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
