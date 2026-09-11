package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
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
	info           os.FileInfo
	completeOffset int64
	header         cursorjsonl.TranscriptHeader
	resumedIDs     []string
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

// cursorTranscriptToolInput carries the one input field that names an existing
// subagent conversation. Cursor writes it when a conversation resumes an agent
// thread it started earlier; a first spawn names nothing, which is why the link
// covers a minority of subagent transcripts.
type cursorTranscriptToolInput struct {
	Resume string `json:"resume"`
}

// buildResumeLinkIndex decodes new complete parent records once for both headers
// and resume links. Continuation is in memory and rebuilt after process startup.
func (p *Parser) buildResumeLinkIndex(
	ctx context.Context,
	files []cursorjsonl.TranscriptFile,
	priorRecords map[string]conversation.Record,
	priorStamps map[string]conversation.FileStamp,
) (resumeLinkIndex, error) {
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
		if err := ctx.Err(); err != nil {
			return resumeLinkIndex{}, fmt.Errorf("scan cursor resume links: %w", err)
		}
		info, err := os.Stat(file.Path)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.resume_stat_failed", "concern", concern, "path", file.Path, "err", err)
			continue
		}
		if cached, reused, reuseErr := p.resumeLinksFromPrior(ctx, file, info, prior, priorRecords, priorStamps); reused {
			if reuseErr != nil {
				return resumeLinkIndex{}, reuseErr
			}
			fresh[file.Path] = cached
			addResumeLinks(parentByResumedID, file.ConversationID, cached.resumedIDs)
			continue
		}
		cached, ok := prior[file.Path]
		if ok && os.SameFile(cached.info, info) && cached.info.Size() == info.Size() && cached.info.ModTime().Equal(info.ModTime()) {
			fresh[file.Path] = cached
			addResumeLinks(parentByResumedID, file.ConversationID, cached.resumedIDs)
			continue
		}
		if !ok || !os.SameFile(cached.info, info) || info.Size() <= cached.info.Size() {
			var empty resumeLinkCacheEntry
			cached = empty
		}
		current, err := scanResumeLinks(ctx, file.Path, info, cached)
		if err != nil {
			return resumeLinkIndex{}, err
		}
		fresh[file.Path] = current
		addResumeLinks(parentByResumedID, file.ConversationID, current.resumedIDs)
	}

	p.mu.Lock()
	p.resumeLinks = fresh
	p.mu.Unlock()

	return resumeLinkIndex{parentByResumedID: parentByResumedID}, nil
}

func (p *Parser) resumeLinksFromPrior(
	ctx context.Context,
	file cursorjsonl.TranscriptFile,
	info os.FileInfo,
	prior map[string]resumeLinkCacheEntry,
	priorRecords map[string]conversation.Record,
	priorStamps map[string]conversation.FileStamp,
) (resumeLinkCacheEntry, bool, error) {
	var empty resumeLinkCacheEntry
	if _, inMemory := prior[file.Path]; inMemory {
		return empty, false, nil
	}
	stamp, found := priorStamps[file.Path]
	if !found || stamp.Size > info.Size() {
		return empty, false, nil
	}
	cached := resumeLinkEntryFromPrior(file, info, stamp, priorRecords)
	if stamp.Size == info.Size() {
		return cached, true, nil
	}
	current, err := scanResumeLinks(ctx, file.Path, info, cached)
	if err != nil {
		return empty, true, err
	}
	return current, true, nil
}

func resumeLinkEntryFromPrior(
	file cursorjsonl.TranscriptFile,
	info os.FileInfo,
	stamp conversation.FileStamp,
	priorRecords map[string]conversation.Record,
) resumeLinkCacheEntry {
	header := cursorjsonl.TranscriptHeader{
		ConversationID:         file.ConversationID,
		FirstUserText:          "",
		HasMessages:            false,
		FirstUserTextUncertain: false,
	}
	if record, ok := priorRecords[file.Path]; ok {
		header.FirstUserText = record.Title
		header.HasMessages = record.Title != ""
		header.FirstUserTextUncertain = record.TitleUncertain
	}
	resumedIDs := make([]string, 0)
	for _, record := range priorRecords {
		if record.Lineage == nil || record.Lineage.ParentNativeID != file.ConversationID {
			continue
		}
		resumedIDs = append(resumedIDs, record.NativeID)
	}
	return resumeLinkCacheEntry{
		info:           info,
		completeOffset: stamp.Size,
		header:         header,
		resumedIDs:     resumedIDs,
	}
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

func scanResumeLinks(ctx context.Context, path string, info os.FileInfo, prior resumeLinkCacheEntry) (resumeLinkCacheEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resume_open_failed", "concern", concern, "path", path, "err", err)
		return resumeLinkCacheEntry{}, fmt.Errorf("open cursor resume transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	resumedIDs := append([]string(nil), prior.resumedIDs...)
	header, offset, err := cursorjsonl.ScanAppend(ctx, path, file, prior.completeOffset, prior.header, func(message cursorjsonl.TranscriptMessage) {
		for _, part := range message.Parts {
			if part.Type != cursorjsonl.PartTypeToolUse || !cursorSpawnToolNames[part.ToolName] {
				continue
			}
			var input cursorTranscriptToolInput
			if json.Unmarshal(part.ToolInput, &input) != nil {
				continue
			}
			resumed := strings.TrimSpace(input.Resume)
			if resumed == "" {
				continue
			}
			resumedIDs = append(resumedIDs, resumed)
		}
	})
	if err != nil {
		return resumeLinkCacheEntry{}, fmt.Errorf("scan cursor resume links %s: %w", path, err)
	}
	return resumeLinkCacheEntry{info: info, completeOffset: offset, header: header, resumedIDs: resumedIDs}, nil
}
