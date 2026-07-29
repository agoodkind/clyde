// Package parser wires Cursor transcript and SQLite artifacts into Clyde's
// conversation index.
package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	concern                        = "providers.cursor.parser"
	artifactKindCursorAgent        = "cursor_agent_transcript"
	artifactKindCursorComposer     = "cursor_composer"
	artifactKindCursorBackground   = "cursor_background_composer"
	artifactKindCursorLegacyChat   = "cursor_legacy_chat"
	discoveredKindJSONL            = "jsonl"
	discoveredKindComposer         = "composer"
	discoveredKindLegacy           = "legacy"
	resolveLookupTimeout           = 5 * time.Second
	untitledCursorConversationText = "Untitled Cursor Conversation"
	untitledCursorChatText         = "Untitled Cursor Chat"
	maxTitleRunes                  = 80
)

type discoveredArtifact struct {
	Kind           string
	Path           string
	ConversationID string
	ProjectKey     string
	// Origin is decided during Discover, once the resume-link index over every
	// parent transcript is complete, so a record's origin never depends on the
	// order candidates are scanned in.
	Origin conversation.Origin
	// ParentConversationID names the conversation that owns a subagent
	// transcript: the id the provider stated in a resume link when there is one,
	// otherwise the containing directory.
	ParentConversationID string
	RootDir              string
	ComposerID           string
	ComposerHeader       cursorstore.ComposerHeader
	ComposerInfo         cursorstore.ComposerMetadata
	HasInfo              bool
	// MetadataComplete reports that every store a composer's metadata is drawn
	// from could be read. When it is false, a record built here would state an
	// absence that was never established, so PriorRecord answers instead wherever
	// the index already held one.
	MetadataComplete bool
	PriorRecord      conversation.Record
	HasPriorRecord   bool
	IsBackground     bool
	LegacyTab        cursorstore.LegacyChatTab
	WorkspaceRoot    string
}

// Parser discovers Cursor conversation artifacts and caches the rows needed by
// later scan stages.
type Parser struct {
	mu         sync.Mutex
	discovered map[string]discoveredArtifact
	// resumeLinks remembers each parent transcript's extracted resume links
	// against the file state they were read from, so an unchanged parent is never
	// re-read on a later refresh.
	resumeLinks map[string]resumeLinkCacheEntry
	// composerStamps remembers the stamp each composer was last given, so a pass
	// that cannot read a chat's bubbles can hand back the same stamp instead of
	// dropping the chat. The scan's prior records cannot serve: a composer record
	// carries no size, so rebuilding a stamp from one invents a value the chat
	// never had.
	composerStamps map[string]conversation.FileStamp
}

var _ conversation.Parser = (*Parser)(nil)

// New returns a Cursor conversation parser.
func New() *Parser {
	return &Parser{
		mu:             sync.Mutex{},
		discovered:     make(map[string]discoveredArtifact),
		resumeLinks:    make(map[string]resumeLinkCacheEntry),
		composerStamps: make(map[string]conversation.FileStamp),
	}
}

// Provider reports that this parser handles Cursor artifacts.
func (*Parser) Provider() providerid.Provider {
	return providerid.ProviderCursor
}

// Discover resolves local Cursor transcript and SQLite data roots, then returns
// scan candidates for modern JSONL transcripts, composers, and legacy chats.
func (p *Parser) Discover(ctx context.Context, prior map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	candidates := make([]conversation.ScanCandidate, 0)
	discovered := make(map[string]discoveredArtifact)
	seenConversationIDs := make(map[string]bool)

	jsonlCandidates, err := p.discoverJSONL(ctx, discovered, seenConversationIDs)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, jsonlCandidates...)

	p.mu.Lock()
	priorStamps := p.composerStamps
	p.mu.Unlock()

	sqliteCandidates, err := discoverSQLite(ctx, priorStamps, prior, discovered, seenConversationIDs)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, sqliteCandidates...)

	sort.SliceStable(candidates, func(i int, j int) bool {
		return candidates[i].Path < candidates[j].Path
	})

	composerStamps := make(map[string]conversation.FileStamp, len(candidates))
	for _, candidate := range candidates {
		if discovered[candidate.Path].Kind == discoveredKindComposer {
			composerStamps[candidate.Path] = candidate.Stamp
		}
	}

	p.mu.Lock()
	p.discovered = discovered
	p.composerStamps = composerStamps
	p.mu.Unlock()

	return candidates, nil
}

// ScanRecord turns one discovered Cursor artifact into a derived Clyde record
// without streaming the full transcript.
//
// A modern JSONL record carries the origin and parent that Discover resolved,
// because classification needs the resume-link index over every parent transcript
// and that is only complete once Discover returns.
//
// Cursor writes no origin marker inside a transcript: a subagent transcript
// carries the same top-level keys as its parent and names neither an agent nor a
// parent. Classification therefore rests on path shape, corroborated by the
// resume link where one exists. A future Cursor layout change would silently
// reclassify these as user conversations, which is why an unexpected resume link
// is logged during Discover.
//
// IsBackground is deliberately not read as an origin: a background composer is an
// agent the person started, not one a conversation dispatched.
//
// A composer's origin comes from the subagent flag Cursor writes beside it, which
// is a statement rather than an inference and so is trusted where the transcript
// path's shape is only corroborated. A composer with no metadata row stays at
// [conversation.OriginUnspecified] rather than being called the person's own,
// because nothing said either way. Legacy chats predate the flag and stay
// unspecified too, which is ingested rather than hidden.
func (p *Parser) ScanRecord(path string, stamp conversation.FileStamp) (conversation.Record, bool) {
	discovered, err := p.resolveDiscovered(path)
	if err != nil {
		slog.Warn("providers.cursor.parser.scan_resolve_failed", "concern", concern, "path", path, "err", err)
		return emptyRecord(), false
	}
	switch discovered.Kind {
	case discoveredKindJSONL:
		header, err := cursorjsonl.ScanHeader(path)
		if err != nil {
			slog.Warn("providers.cursor.parser.scan_header_failed", "concern", concern, "path", path, "err", err)
			return emptyRecord(), false
		}
		return conversation.Record{
			ID:            conversation.DerivedID(providerid.ProviderCursor, discovered.ConversationID, path),
			Provider:      providerid.ProviderCursor,
			NativeID:      discovered.ConversationID,
			Lineage:       cursorSpawnLineage(discovered.ParentConversationID),
			Origin:        discovered.Origin,
			Title:         firstNonEmptyString(truncateTitle(header.FirstUserText), untitledCursorConversationText),
			WorkspaceRoot: cursorjsonl.WorkspacePathFromProjectKey(discovered.ProjectKey),
			ArtifactPath:  path,
			ArtifactKind:  artifactKindCursorAgent,
			Model:         "",
			CreatedAt:     stamp.Mtime,
			UpdatedAt:     stamp.Mtime,
			SizeBytes:     stamp.Size,
			Archived:      false,
		}, true
	case discoveredKindComposer:
		if !discovered.MetadataComplete && discovered.HasPriorRecord {
			// A store that could not be read established nothing, and the record
			// built below would state an empty workspace, no archive, and no origin
			// as though it had. The scan rebuilds its whole record set from this
			// pass, so that record would replace a good one and, because the chat's
			// own artifact has not changed, nothing would ever prompt a correction.
			// The record already held is the better answer until the store reads.
			return discovered.PriorRecord, true
		}
		header := discovered.ComposerHeader
		workspaceTitle := ""
		workspaceRoot := ""
		archived := false
		origin := conversation.OriginUnspecified
		if discovered.HasInfo {
			workspaceTitle = discovered.ComposerInfo.Name
			workspaceRoot = discovered.ComposerInfo.WorkspaceRoot
			archived = discovered.ComposerInfo.Archived
			origin = composerOrigin(discovered.ComposerInfo.Subagent)
		}
		return conversation.Record{
			ID:            conversation.DerivedID(providerid.ProviderCursor, discovered.ComposerID, path),
			Provider:      providerid.ProviderCursor,
			NativeID:      discovered.ComposerID,
			Lineage:       nil,
			Origin:        origin,
			Title:         firstNonEmptyString(header.Name, workspaceTitle, untitledCursorConversationText),
			WorkspaceRoot: workspaceRoot,
			ArtifactPath:  path,
			ArtifactKind:  composerArtifactKind(discovered.IsBackground),
			Model:         "",
			CreatedAt:     msToTime(header.CreatedAt),
			UpdatedAt:     composerUpdatedAt(header),
			SizeBytes:     0,
			Archived:      archived,
		}, true
	case discoveredKindLegacy:
		tab := discovered.LegacyTab
		virtualPath, err := ParseVirtualPath(path)
		if err != nil {
			return emptyRecord(), false
		}
		legacyConversationID := virtualPath.ID
		return conversation.Record{
			ID:            conversation.DerivedID(providerid.ProviderCursor, legacyConversationID, path),
			Provider:      providerid.ProviderCursor,
			NativeID:      legacyConversationID,
			Lineage:       nil,
			Origin:        conversation.OriginUnspecified,
			Title:         firstNonEmptyString(tab.ChatTitle, untitledCursorChatText),
			WorkspaceRoot: discovered.WorkspaceRoot,
			ArtifactPath:  path,
			ArtifactKind:  artifactKindCursorLegacyChat,
			Model:         "",
			CreatedAt:     stamp.Mtime,
			UpdatedAt:     stamp.Mtime,
			SizeBytes:     0,
			Archived:      false,
		}, true
	default:
		return emptyRecord(), false
	}
}

// Stream lazily yields transcript-shaped messages for one discovered Cursor
// artifact.
func (p *Parser) Stream(path string, opts conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		discovered, err := p.resolveDiscovered(path)
		if err != nil {
			yield(emptyMessage(), err)
			return
		}
		switch discovered.Kind {
		case discoveredKindJSONL:
			err := cursorjsonl.StreamMessages(path, func(message cursorjsonl.TranscriptMessage) error {
				mapped, include := mapJSONLMessage(message, opts)
				if !include {
					return nil
				}
				if !yield(mapped, nil) {
					return cursorjsonl.ErrStopStreaming
				}
				return nil
			})
			if err != nil && !errors.Is(err, cursorjsonl.ErrStopStreaming) {
				yield(emptyMessage(), err)
			}
		case discoveredKindComposer:
			streamComposer(discovered, opts, yield)
		case discoveredKindLegacy:
			for _, bubble := range discovered.LegacyTab.Bubbles {
				mapped, include := mapLegacyBubble(bubble, opts)
				if !include {
					continue
				}
				if !yield(mapped, nil) {
					return
				}
			}
		}
	}
}

func (p *Parser) discoverJSONL(
	ctx context.Context,
	discovered map[string]discoveredArtifact,
	seenConversationIDs map[string]bool,
) ([]conversation.ScanCandidate, error) {
	roots, err := cursorjsonl.ResolveProjectRoots()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_project_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve cursor project roots: %w", err)
	}
	files, err := cursorjsonl.DiscoverTranscriptFiles(roots)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.discover_transcripts_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("discover cursor transcript files: %w", err)
	}
	resumeLinks := p.buildResumeLinkIndex(ctx, files)
	unexpectedResumeLinks := 0

	candidates := make([]conversation.ScanCandidate, 0, len(files))
	for _, file := range files {
		info, err := os.Stat(file.Path)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.stat_jsonl_failed", "concern", concern, "path", file.Path, "err", err)
			continue
		}
		candidates = append(candidates, conversation.ScanCandidate{
			Path: file.Path,
			Stamp: conversation.FileStamp{
				Size:  info.Size(),
				Mtime: info.ModTime(),
			},
		})
		// The path shape decides whether a transcript is a subagent conversation.
		// The resume link corroborates that call and supplies the parent reference
		// when it exists, because the provider stated that link outright. A resume
		// link that disagrees with the path is logged and otherwise ignored: acting
		// on it alone would hide a conversation the layout says is the user's own,
		// which is the loss this classification exists to prevent.
		resumeParentID, resumedByAConversation := resumeLinks.parentOf(file.ConversationID)
		parentConversationID := file.ParentConversationID
		origin := originFromParentConversationID(parentConversationID)
		switch {
		case origin == conversation.OriginSubagent && resumedByAConversation:
			parentConversationID = resumeParentID
		case resumedByAConversation:
			unexpectedResumeLinks++
		}

		var artifact discoveredArtifact
		artifact.Kind = discoveredKindJSONL
		artifact.Path = file.Path
		artifact.ConversationID = file.ConversationID
		artifact.ProjectKey = file.ProjectKey
		artifact.Origin = origin
		artifact.ParentConversationID = parentConversationID
		discovered[file.Path] = artifact
		seenConversationIDs[file.ConversationID] = true
	}
	if unexpectedResumeLinks > 0 {
		// A resumed conversation that is not under a subagents/ directory means the
		// path rule no longer describes where Cursor puts these, which would
		// otherwise go unnoticed until subagent conversations quietly returned to
		// the index. These conversations stay user origin, so this is an early
		// warning rather than a reclassification.
		slog.InfoContext(ctx, "providers.cursor.parser.resume_link_outside_subagents_dir",
			"concern", concern, "component", "cursor", "count", unexpectedResumeLinks)
	}
	return candidates, nil
}

// originFromParentConversationID reads the origin off the path shape, which is
// the one classifier both the Discover pass and the direct resolve can apply.
// A transcript nested under another conversation's subagents/ directory carries
// that conversation's id here; a conversation's own transcript carries none.
func originFromParentConversationID(parentConversationID string) conversation.Origin {
	if parentConversationID == "" {
		return conversation.OriginUser
	}
	return conversation.OriginSubagent
}

func discoverSQLite(
	ctx context.Context,
	priorStamps map[string]conversation.FileStamp,
	prior map[string]conversation.Record,
	discovered map[string]discoveredArtifact,
	seenConversationIDs map[string]bool,
) ([]conversation.ScanCandidate, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve cursor data roots: %w", err)
	}

	candidates := make([]conversation.ScanCandidate, 0)
	for _, root := range roots {
		candidates = append(candidates, discoverComposersForRoot(ctx, root, priorStamps, prior, discovered, seenConversationIDs)...)
		candidates = append(candidates, discoverLegacyForRoot(ctx, root, discovered)...)
	}
	return candidates, nil
}

func discoverComposersForRoot(
	ctx context.Context,
	root cursorstore.DataRoot,
	priorStamps map[string]conversation.FileStamp,
	prior map[string]conversation.Record,
	discovered map[string]discoveredArtifact,
	seenConversationIDs map[string]bool,
) []conversation.ScanCandidate {
	db, ok := openOptionalDatabase(ctx, root.GlobalDBPath, "global_db")
	if !ok {
		return nil
	}
	defer func() { _ = db.Close() }()

	composerIDs, err := cursorstore.ListComposerIDs(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_composers_failed", "concern", concern, "path", root.GlobalDBPath, "err", err)
		return nil
	}
	backgroundIDs := backgroundComposerIDSet(ctx, db, root.GlobalDBPath)
	metadataIndex := composerMetadataIndex(ctx, db, root)
	rootHash := RootHash(root.RootDir)

	candidates := make([]conversation.ScanCandidate, 0, len(composerIDs))
	for _, composerID := range composerIDs {
		if seenConversationIDs[composerID] {
			continue
		}
		header, found, err := cursorstore.ReadComposerHeader(ctx, db, composerID)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.read_composer_header_failed", "concern", concern, "path", root.GlobalDBPath, "composer_id", composerID, "err", err)
			continue
		}
		if !found {
			continue
		}
		path := BuildVirtualPath(rootHash, VirtualKindComposer, composerID)
		if path == "" {
			continue
		}
		stamp, admitted := composerScanStamp(ctx, db, root.GlobalDBPath, composerID, header, priorStamps[path])
		if !admitted {
			continue
		}
		info, hasInfo := metadataIndex.ByComposerID[composerID]
		candidates = append(candidates, conversation.ScanCandidate{
			Path:  path,
			Stamp: stampCoveringMetadata(stamp, info, hasInfo),
		})
		priorRecord, hasPriorRecord := prior[path]
		var artifact discoveredArtifact
		artifact.Kind = discoveredKindComposer
		artifact.Path = path
		artifact.MetadataComplete = metadataIndex.Err == nil
		artifact.PriorRecord = priorRecord
		artifact.HasPriorRecord = hasPriorRecord
		artifact.RootDir = root.RootDir
		artifact.ComposerID = composerID
		artifact.ComposerHeader = header
		artifact.ComposerInfo = info
		artifact.HasInfo = hasInfo
		artifact.IsBackground = backgroundIDs[composerID]
		discovered[path] = artifact
	}
	return candidates
}

// composerUpdatedAt is the most recent time a chat is known to have changed.
//
// `lastUpdatedAt` alone puts a chat Cursor never stamped at the zero time, which
// sorts it below every dated conversation in a listing ordered newest first. The
// chats this parser admits by their stored bubbles are exactly the ones missing
// that stamp, so the 2,189-message agent run would land last of roughly 2,470
// records and never appear in a default listing. Its creation time is a real
// lower bound on when it changed, and it is the newest one the header carries.
func composerUpdatedAt(header cursorstore.ComposerHeader) time.Time {
	if header.LastUpdatedAt > header.CreatedAt {
		return msToTime(header.LastUpdatedAt)
	}
	return msToTime(header.CreatedAt)
}

// composerScanStamp decides whether one chat is a conversation Clyde should hold,
// and returns the stamp that says when to re-read it. The second result is false
// for a chat with nothing stored to deliver.
//
// A chat's header reference list is not a complete index of its stored bubbles,
// so it decides neither whether the chat exists nor when it changed. Measured on
// a real store, 631 of 2,470 chats list no references, 9 of those hold stored
// bubbles anyway, and one of the 9 is a 2,189-message agent run. Trusting the
// empty list drops those 9 conversations out of Clyde entirely, so `conversation
// list` never names them and a request id belonging to one resolves to nothing.
//
// The remaining 622 stay out. They hold no bubble row at all, so they are the
// drafts and empty panes the guard was written for, and admitting them would put
// 622 conversations with nothing to read into every listing and every search.
//
// A chat whose key range could not be read is not one of the 622. Dropping it
// here does not merely skip it: the scan rebuilds the record set from the
// candidates this pass returns, so a chat left out loses the record it already
// had. One busy database while Cursor checkpoints would take a conversation out
// of the index until a later pass succeeded. Such a chat keeps its previous
// stamp when it has one, which re-admits it unchanged, and is reported when it
// does not.
func composerScanStamp(
	ctx context.Context,
	db *sql.DB,
	globalDBPath string,
	composerID string,
	header cursorstore.ComposerHeader,
	priorStamp conversation.FileStamp,
) (conversation.FileStamp, bool) {
	stock, err := cursorstore.ReadComposerBubbleStock(ctx, db, composerID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.composer_bubble_stock_failed", "concern", concern, "path", globalDBPath, "composer_id", composerID, "err", err)
		return keepPriorComposer(ctx, globalDBPath, composerID, priorStamp)
	}
	if !stock.Conclusive {
		if priorStamp.Size == 0 && priorStamp.Mtime.IsZero() {
			slog.WarnContext(ctx, "providers.cursor.parser.composer_admitted_unread", "concern", concern, "path", globalDBPath, "composer_id", composerID, "stored_rows", stock.StoredRows)
			return composerStamp(header, stock), true
		}
		return keepPriorComposer(ctx, globalDBPath, composerID, priorStamp)
	}
	if len(header.FullConversationHeadersOnly) == 0 {
		if !stock.HasContent {
			return conversation.FileStamp{Size: 0, Mtime: time.Time{}}, false
		}
		slog.DebugContext(ctx, "providers.cursor.parser.composer_admitted_by_stored_bubbles", "concern", concern, "path", globalDBPath, "composer_id", composerID, "stored_rows", stock.StoredRows)
	}
	return composerStamp(header, stock), true
}

// composerStamp carries the bubble range's content revision beside the header's
// last update time. The revision changes on additions, removals, and replacements
// even when Cursor leaves the header unchanged.
func composerStamp(
	header cursorstore.ComposerHeader,
	stock cursorstore.ComposerBubbleStock,
) conversation.FileStamp {
	return conversation.FileStamp{Size: stock.Revision, Mtime: msToTime(header.LastUpdatedAt)}
}

// keepPriorComposer re-admits a chat whose stored bubbles could not be read, on
// the stamp the last successful pass gave it, so a transient read failure does
// not remove a conversation Clyde already holds.
//
// It takes the previous stamp rather than rebuilding one from the record, because
// a composer record carries no size: reconstructing it would hand back a stamp
// the chat never had, which compares unequal, re-runs the scan and changes the
// fingerprint the engine keys re-embedding on. A chat with no previous stamp has
// nothing to preserve and is reported rather than quietly dropped.
func keepPriorComposer(
	ctx context.Context,
	globalDBPath string,
	composerID string,
	priorStamp conversation.FileStamp,
) (conversation.FileStamp, bool) {
	if priorStamp.Size == 0 && priorStamp.Mtime.IsZero() {
		slog.WarnContext(ctx, "providers.cursor.parser.composer_dropped_unread", "concern", concern, "path", globalDBPath, "composer_id", composerID)
		return conversation.FileStamp{Size: 0, Mtime: time.Time{}}, false
	}
	slog.WarnContext(ctx, "providers.cursor.parser.composer_kept_on_prior_stamp", "concern", concern, "path", globalDBPath, "composer_id", composerID)
	return priorStamp, true
}

func discoverLegacyForRoot(
	ctx context.Context,
	root cursorstore.DataRoot,
	discovered map[string]discoveredArtifact,
) []conversation.ScanCandidate {
	entries, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_workspace_entries_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return nil
	}
	rootHash := RootHash(root.RootDir)
	candidates := make([]conversation.ScanCandidate, 0)
	for _, entry := range entries {
		candidates = append(candidates, discoverLegacyForEntry(ctx, rootHash, entry, discovered)...)
	}
	return candidates
}

func discoverLegacyForEntry(
	ctx context.Context,
	rootHash string,
	entry cursorstore.WorkspaceEntry,
	discovered map[string]discoveredArtifact,
) []conversation.ScanCandidate {
	db, ok := openOptionalDatabase(ctx, entry.StateDBPath, "workspace_db")
	if !ok {
		return nil
	}
	defer func() { _ = db.Close() }()

	chatData, found, err := cursorstore.ReadLegacyChatData(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_legacy_chat_failed", "concern", concern, "path", entry.StateDBPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return nil
	}
	if !found {
		return nil
	}
	workspaceRoot := readWorkspaceRoot(ctx, entry)
	stampMtime := time.Time{}
	stateDBInfo, statErr := os.Stat(entry.StateDBPath)
	if statErr == nil {
		stampMtime = stateDBInfo.ModTime()
	}
	candidates := make([]conversation.ScanCandidate, 0, len(chatData.Tabs))
	for _, tab := range chatData.Tabs {
		if len(tab.Bubbles) == 0 {
			continue
		}
		id := legacyID(entry.WorkspaceHash, tab.TabID)
		path := BuildVirtualPath(rootHash, VirtualKindLegacy, id)
		if path == "" {
			continue
		}
		candidates = append(candidates, conversation.ScanCandidate{
			Path: path,
			Stamp: conversation.FileStamp{
				Size:  int64(len(tab.Bubbles)),
				Mtime: stampMtime,
			},
		})
		var artifact discoveredArtifact
		artifact.Kind = discoveredKindLegacy
		artifact.Path = path
		artifact.LegacyTab = tab
		artifact.WorkspaceRoot = workspaceRoot
		discovered[path] = artifact
	}
	return candidates
}

func streamComposer(
	discovered discoveredArtifact,
	opts conversation.LoadOptions,
	yield func(transcript.Message, error) bool,
) {
	ctx := context.Background()

	dbPath, err := globalDBPathForDiscovered(ctx, discovered)
	if err != nil {
		yield(emptyMessage(), err)
		return
	}
	db, err := cursorstore.OpenReadOnlyDatabase(ctx, dbPath)
	if err != nil {
		yield(emptyMessage(), fmt.Errorf("open cursor global db %s: %w", dbPath, err))
		return
	}
	defer func() { _ = db.Close() }()

	// Ordering needs every stored bubble's write time before it can place any of
	// them, because the header's reference list is not a complete index of the
	// chat and following it lazily would stream a conversation with real turns
	// missing. Only the ordering is eager: the store settles the order from a
	// digest of each row and then reads the bubbles themselves one at a time, so
	// declining a message here stops the read.
	// The id comes from the artifact rather than the header payload, because the
	// artifact's id is the one derived from the key the rows are stored under.
	err = cursorstore.StreamComposerBubbles(ctx, db, discovered.ComposerID, discovered.ComposerHeader, func(bubble cursorstore.Bubble) bool {
		mapped, include := mapComposerBubble(bubble, opts)
		if !include {
			return true
		}
		return yield(mapped, nil)
	})
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.stream_composer_failed", "concern", concern, "path", dbPath, "composer_id", discovered.ComposerID, "err", err)
		yield(emptyMessage(), fmt.Errorf("load cursor composer %q bubbles: %w", discovered.ComposerID, err))
		return
	}
}

func (p *Parser) resolveDiscovered(path string) (discoveredArtifact, error) {
	var emptyDiscovered discoveredArtifact

	p.mu.Lock()
	discovered, ok := p.discovered[path]
	p.mu.Unlock()
	if ok {
		return discovered, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveLookupTimeout)
	defer cancel()

	var resolved discoveredArtifact
	var err error
	if !strings.HasPrefix(path, virtualPathPrefix) {
		resolved, err = resolveJSONLDiscovered(ctx, path)
	} else {
		resolved, err = resolveVirtualDiscovered(ctx, path)
	}
	if err != nil {
		return emptyDiscovered, err
	}
	p.mu.Lock()
	p.discovered[path] = resolved
	p.mu.Unlock()
	return resolved, nil
}

func resolveJSONLDiscovered(ctx context.Context, path string) (discoveredArtifact, error) {
	if _, err := os.Stat(path); err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.stat_jsonl_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("stat cursor transcript %s: %w", path, err)
	}
	file, ok, err := cursorjsonl.MatchTranscriptFile(path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.match_transcript_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("match cursor transcript file: %w", err)
	}
	if !ok {
		slog.WarnContext(ctx, "providers.cursor.parser.jsonl_path_not_found", "concern", concern, "path", path)
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor transcript path not discovered: %s", path)
	}
	// The direct resolve has no resume-link index, which Discover builds over
	// every parent transcript. The path shape is the classifier either way, so
	// this record carries the same origin and parent Discover would have given it
	// rather than an unspecified origin that would bypass the setting.
	var artifact discoveredArtifact
	artifact.Kind = discoveredKindJSONL
	artifact.Path = file.Path
	artifact.ConversationID = file.ConversationID
	artifact.ProjectKey = file.ProjectKey
	artifact.Origin = originFromParentConversationID(file.ParentConversationID)
	artifact.ParentConversationID = file.ParentConversationID
	return artifact, nil
}

func resolveVirtualDiscovered(ctx context.Context, path string) (discoveredArtifact, error) {
	virtualPath, err := ParseVirtualPath(path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.virtual_path_parse_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("parse cursor virtual path: %w", err)
	}
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "path", path, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("resolve cursor data roots: %w", err)
	}
	for _, root := range roots {
		if RootHash(root.RootDir) != virtualPath.RootHash {
			continue
		}
		switch virtualPath.Kind {
		case VirtualKindComposer:
			return resolveComposerDiscovered(ctx, root, path, virtualPath.ID)
		case VirtualKindLegacy:
			return resolveLegacyDiscovered(ctx, root, path, virtualPath.ID)
		}
	}
	slog.WarnContext(ctx, "providers.cursor.parser.virtual_path_not_found", "concern", concern, "path", path, "kind", virtualPath.Kind, "id", virtualPath.ID)
	return emptyDiscoveredArtifact(), fmt.Errorf("cursor path not discovered: %s", path)
}

func resolveComposerDiscovered(
	ctx context.Context,
	root cursorstore.DataRoot,
	path string,
	composerID string,
) (discoveredArtifact, error) {
	db, ok := openOptionalDatabase(ctx, root.GlobalDBPath, "global_db")
	if !ok {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor global db not found: %s", root.GlobalDBPath)
	}
	defer func() { _ = db.Close() }()

	header, found, err := cursorstore.ReadComposerHeader(ctx, db, composerID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_composer_header_failed", "concern", concern, "path", root.GlobalDBPath, "composer_id", composerID, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("read cursor composer header %q: %w", composerID, err)
	}
	if !found {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor composer %q not found", composerID)
	}
	metadataIndex := composerMetadataIndex(ctx, db, root)
	info, hasInfo := metadataIndex.ByComposerID[composerID]
	backgroundIDs := backgroundComposerIDSet(ctx, db, root.GlobalDBPath)
	var artifact discoveredArtifact
	artifact.Kind = discoveredKindComposer
	artifact.Path = path
	// This path resolves one chat on demand, outside any scan, so there is no
	// prior record to fall back to and the metadata read stands on its own.
	artifact.MetadataComplete = true
	artifact.RootDir = root.RootDir
	artifact.ComposerID = composerID
	artifact.ComposerHeader = header
	artifact.ComposerInfo = info
	artifact.HasInfo = hasInfo
	artifact.IsBackground = backgroundIDs[composerID]
	return artifact, nil
}

func resolveLegacyDiscovered(
	ctx context.Context,
	root cursorstore.DataRoot,
	path string,
	id string,
) (discoveredArtifact, error) {
	workspaceHash, tabID, ok := splitLegacyID(id)
	if !ok {
		return emptyDiscoveredArtifact(), fmt.Errorf("invalid cursor legacy id %q", id)
	}
	entries, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_workspace_entries_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("list cursor workspace entries: %w", err)
	}
	for _, entry := range entries {
		if entry.WorkspaceHash != workspaceHash {
			continue
		}
		return resolveLegacyDiscoveredForEntry(ctx, path, tabID, entry)
	}
	return emptyDiscoveredArtifact(), fmt.Errorf("cursor workspace %q not found", workspaceHash)
}

func resolveLegacyDiscoveredForEntry(
	ctx context.Context,
	path string,
	tabID string,
	entry cursorstore.WorkspaceEntry,
) (discoveredArtifact, error) {
	db, ok := openOptionalDatabase(ctx, entry.StateDBPath, "workspace_db")
	if !ok {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor workspace db not found: %s", entry.StateDBPath)
	}
	defer func() { _ = db.Close() }()

	chatData, found, err := cursorstore.ReadLegacyChatData(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_legacy_chat_failed", "concern", concern, "path", entry.StateDBPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("read cursor legacy chat: %w", err)
	}
	if !found {
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor legacy chat data not found")
	}
	for _, tab := range chatData.Tabs {
		if tab.TabID != tabID || len(tab.Bubbles) == 0 {
			continue
		}
		var artifact discoveredArtifact
		artifact.Kind = discoveredKindLegacy
		artifact.Path = path
		artifact.LegacyTab = tab
		artifact.WorkspaceRoot = readWorkspaceRoot(ctx, entry)
		return artifact, nil
	}
	return emptyDiscoveredArtifact(), fmt.Errorf("cursor legacy tab %q not found", tabID)
}

func openOptionalDatabase(ctx context.Context, path string, component string) (*sql.DB, bool) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		slog.WarnContext(ctx, "providers.cursor.parser.stat_db_failed", "concern", concern, "component", component, "path", path, "err", err)
		return nil, false
	}
	db, err := cursorstore.OpenReadOnlyDatabase(ctx, path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.open_db_failed", "concern", concern, "component", component, "path", path, "err", err)
		return nil, false
	}
	return db, true
}

func backgroundComposerIDSet(ctx context.Context, db *sql.DB, path string) map[string]bool {
	backgroundIDs := make(map[string]bool)
	backgroundComposers, err := cursorstore.ListBackgroundComposers(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_background_composers_failed", "concern", concern, "path", path, "err", err)
		return backgroundIDs
	}
	for _, backgroundComposer := range backgroundComposers {
		backgroundIDs[backgroundComposer.ComposerID] = true
	}
	return backgroundIDs
}

func readWorkspaceRoot(ctx context.Context, entry cursorstore.WorkspaceEntry) string {
	workspaceRoot, err := cursorstore.ReadWorkspaceFolderPath(entry.WorkspaceJSONPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_workspace_folder_failed", "concern", concern, "path", entry.WorkspaceJSONPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return ""
	}
	return workspaceRoot
}

func globalDBPathForDiscovered(
	ctx context.Context,
	discovered discoveredArtifact,
) (string, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "path", discovered.Path, "err", err)
		return "", fmt.Errorf("resolve cursor data roots: %w", err)
	}
	if discovered.RootDir != "" {
		for _, root := range roots {
			if root.RootDir == discovered.RootDir {
				return root.GlobalDBPath, nil
			}
		}
	}
	virtualPath, err := ParseVirtualPath(discovered.Path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.virtual_path_parse_failed", "concern", concern, "path", discovered.Path, "err", err)
		return "", fmt.Errorf("parse cursor virtual path: %w", err)
	}
	for _, root := range roots {
		if RootHash(root.RootDir) == virtualPath.RootHash {
			return root.GlobalDBPath, nil
		}
	}
	slog.WarnContext(ctx, "providers.cursor.parser.root_not_found", "concern", concern, "path", discovered.Path)
	return "", fmt.Errorf("cursor root not found for %s", discovered.Path)
}

func composerArtifactKind(isBackground bool) string {
	if isBackground {
		return artifactKindCursorBackground
	}
	return artifactKindCursorComposer
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ms*int64(time.Millisecond))
}

// cursorSpawnLineage records the conversation that dispatched a subagent
// transcript. Cursor names its parent either through the containing directory or
// through a resume link, so unlike Claude the relationship is real data rather
// than an inference, and it lands in the same typed lineage Codex and Zed use.
func cursorSpawnLineage(parentConversationID string) *conversation.Lineage {
	parentConversationID = strings.TrimSpace(parentConversationID)
	if parentConversationID == "" {
		return nil
	}
	return &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindSpawn,
		ParentProvider:    providerid.ProviderCursor,
		ParentNativeID:    parentConversationID,
		ParentMessageUUID: "",
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func truncateTitle(title string) string {
	trimmed := strings.TrimSpace(title)
	runes := []rune(trimmed)
	if len(runes) <= maxTitleRunes {
		return trimmed
	}
	return string(runes[:maxTitleRunes])
}

func emptyRecord() conversation.Record {
	var record conversation.Record
	return record
}

func emptyDiscoveredArtifact() discoveredArtifact {
	var artifact discoveredArtifact
	return artifact
}
