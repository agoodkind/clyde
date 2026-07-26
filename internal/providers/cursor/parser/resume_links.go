package parser

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
)

const (
	// cursorToolUseBlockType names the assistant content block that carries a tool
	// call in a Cursor transcript.
	cursorToolUseBlockType = "tool_use"
	// resumeScanBufferMax bounds a single transcript line while scanning for
	// resume links. A longer line is skipped rather than failing the scan.
	resumeScanBufferMax = 4 * 1024 * 1024
)

// cursorSpawnToolNames are the tool calls through which a Cursor conversation
// dispatches an agent. Only these carry a resume link worth trusting.
var cursorSpawnToolNames = map[string]bool{
	"Task":     true,
	"Subagent": true,
}

// resumeLinkCacheEntry is one parent transcript's extracted resume links,
// remembered against the file state they were read from so an unchanged parent
// is never re-read.
type resumeLinkCacheEntry struct {
	size       int64
	mtime      time.Time
	resumedIDs []string
}

// resumeLinkIndex maps a conversation id a parent resumed to the parent's own
// conversation id. It is built during Discover, before any candidate is
// classified, so a record's origin never depends on scan order.
type resumeLinkIndex struct {
	parentByResumedID map[string]string
}

// parentOf returns the conversation that resumed the given id. The lookup is an
// exact match on the id the provider wrote, never a substring: a false positive
// here would classify a real user conversation as a subagent and drop it from the
// index.
func (index resumeLinkIndex) parentOf(conversationID string) (string, bool) {
	if len(index.parentByResumedID) == 0 {
		return "", false
	}
	parentID, ok := index.parentByResumedID[strings.TrimSpace(conversationID)]
	if !ok || parentID == "" {
		return "", false
	}
	return parentID, true
}

// cursorTranscriptLine is the slice of a Cursor transcript record that can carry
// a spawn tool call. Everything else in the record is ignored.
type cursorTranscriptLine struct {
	Message cursorTranscriptMessageBody `json:"message"`
}

type cursorTranscriptMessageBody struct {
	Content []cursorTranscriptContentBlock `json:"content"`
}

type cursorTranscriptContentBlock struct {
	Type  string                    `json:"type"`
	Name  string                    `json:"name"`
	Input cursorTranscriptToolInput `json:"input"`
}

// cursorTranscriptToolInput carries the one input field that names an existing
// subagent conversation. Cursor writes it when a conversation resumes an agent
// thread it started earlier; a first spawn names nothing, which is why the link
// covers a minority of subagent transcripts.
type cursorTranscriptToolInput struct {
	Resume string `json:"resume"`
}

// buildResumeLinkIndex reads every parent transcript that changed since the last
// refresh and collects the conversation ids their spawn tool calls resume.
//
// This is the one Cursor scan that reads a transcript end to end rather than a
// bounded header, so its cost grows with corpus bytes. The per-file cache keyed on
// size and modification time keeps the steady state near zero: only a parent that
// actually changed is re-read.
func (p *Parser) buildResumeLinkIndex(ctx context.Context, files []cursorjsonl.TranscriptFile) resumeLinkIndex {
	parentByResumedID := make(map[string]string)
	fresh := make(map[string]resumeLinkCacheEntry, len(files))

	p.mu.Lock()
	prior := p.resumeLinks
	p.mu.Unlock()

	for _, file := range files {
		if file.ParentConversationID != "" {
			// Only a conversation's own transcript spawns agents.
			continue
		}
		if ctx.Err() != nil {
			break
		}
		info, err := os.Stat(file.Path)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.resume_stat_failed", "concern", concern, "path", file.Path, "err", err)
			continue
		}
		cached, ok := prior[file.Path]
		if ok && cached.size == info.Size() && cached.mtime.Equal(info.ModTime()) {
			fresh[file.Path] = cached
			addResumeLinks(parentByResumedID, file.ConversationID, cached.resumedIDs)
			continue
		}
		resumedIDs := readResumedConversationIDs(ctx, file.Path)
		fresh[file.Path] = resumeLinkCacheEntry{size: info.Size(), mtime: info.ModTime(), resumedIDs: resumedIDs}
		addResumeLinks(parentByResumedID, file.ConversationID, resumedIDs)
	}

	p.mu.Lock()
	p.resumeLinks = fresh
	p.mu.Unlock()

	return resumeLinkIndex{parentByResumedID: parentByResumedID}
}

// addResumeLinks records each resumed id against the conversation that resumed
// it, keeping the first parent when two conversations name the same id.
func addResumeLinks(parentByResumedID map[string]string, parentConversationID string, resumedIDs []string) {
	for _, resumedID := range resumedIDs {
		if resumedID == "" || resumedID == parentConversationID {
			continue
		}
		if _, exists := parentByResumedID[resumedID]; exists {
			continue
		}
		parentByResumedID[resumedID] = parentConversationID
	}
}

// readResumedConversationIDs scans one parent transcript for the conversation ids
// its Task and Subagent tool calls resume.
func readResumedConversationIDs(ctx context.Context, path string) []string {
	file, err := os.Open(path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resume_open_failed", "concern", concern, "path", path, "err", err)
		return nil
	}
	defer func() { _ = file.Close() }()

	var resumedIDs []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), resumeScanBufferMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record cursorTranscriptLine
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		for _, block := range record.Message.Content {
			if block.Type != cursorToolUseBlockType || !cursorSpawnToolNames[block.Name] {
				continue
			}
			resumed := strings.TrimSpace(block.Input.Resume)
			if resumed == "" {
				continue
			}
			resumedIDs = append(resumedIDs, resumed)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resume_scan_failed", "concern", concern, "path", path, "err", scanErr)
	}
	return resumedIDs
}
