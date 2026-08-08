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
// resume link where one exists. Cursor also writes a dispatched conversation's
// transcript twice, once under the dispatching conversation's subagents/
// directory and once as a top-level twin named by the same uuid, so a top-level
// transcript whose uuid has a subagents/ twin in the same project is the
// dispatched conversation itself and classifies as a subagent. A future Cursor
// layout change would silently reclassify these as user conversations, which is
// why an unexpected resume link is logged during Discover.
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
		if header.FirstUserTextUncertain {
			// The scan skipped a record it could not decode before settling on this
			// text, so the title is the earliest user message Clyde could read rather
			// than the conversation's first. The record carries that, but the daemon's
			// wire record has no such field, so it does not currently reach CLI or MCP
			// listings: inside the daemon and in this warning is where it is visible.
			slog.Warn("providers.cursor.parser.title_from_incomplete_header",
				"concern", concern, "path", path, "conversation_id", discovered.ConversationID)
		}
		return conversation.Record{
			ID:             conversation.DerivedID(providerid.ProviderCursor, discovered.ConversationID, path),
			Provider:       providerid.ProviderCursor,
			NativeID:       discovered.ConversationID,
			Selector:       "",
			Lineage:        cursorSpawnLineage(discovered.ParentConversationID),
			Origin:         discovered.Origin,
			Title:          firstNonEmptyString(truncateTitle(header.FirstUserText), untitledCursorConversationText),
			TitleUncertain: header.FirstUserTextUncertain,
			WorkspaceRoot:  cursorjsonl.WorkspacePathFromProjectKey(discovered.ProjectKey),
			ArtifactPath:   path,
			ArtifactKind:   artifactKindCursorAgent,
			Model:          "",
			CreatedAt:      stamp.Mtime,
			UpdatedAt:      stamp.Mtime,
			SizeBytes:      stamp.Size,
			Archived:       false,
			// Cursor's agent transcripts carry no request id.
			LatestRequestID: "",
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
			ID:             conversation.DerivedID(providerid.ProviderCursor, discovered.ComposerID, path),
			Provider:       providerid.ProviderCursor,
			NativeID:       discovered.ComposerID,
			Selector:       "",
			Lineage:        nil,
			Origin:         origin,
			Title:          firstNonEmptyString(header.Name, workspaceTitle, untitledCursorConversationText),
			TitleUncertain: false,
			WorkspaceRoot:  workspaceRoot,
			ArtifactPath:   path,
			ArtifactKind:   composerArtifactKind(discovered.IsBackground),
			Model:          "",
			CreatedAt:      msToTime(header.CreatedAt),
			UpdatedAt:      composerUpdatedAt(header),
			SizeBytes:      0,
			Archived:       archived,
			// The chat header the scan already read carries the request id of the
			// chat's latest turn, so recording it here costs no extra read. Earlier
			// requests are not in the header and resolve through the live lookup.
			LatestRequestID: header.LatestChatGenerationUUID,
		}, true
	case discoveredKindLegacy:
		tab := discovered.LegacyTab
		virtualPath, err := ParseVirtualPath(path)
		if err != nil {
			return emptyRecord(), false
		}
		legacyConversationID := virtualPath.ID
		return conversation.Record{
			ID:             conversation.DerivedID(providerid.ProviderCursor, legacyConversationID, path),
			Provider:       providerid.ProviderCursor,
			NativeID:       legacyConversationID,
			Selector:       "",
			Lineage:        nil,
			Origin:         conversation.OriginUnspecified,
			Title:          firstNonEmptyString(tab.ChatTitle, untitledCursorChatText),
			TitleUncertain: false,
			WorkspaceRoot:  discovered.WorkspaceRoot,
			ArtifactPath:   path,
			ArtifactKind:   artifactKindCursorLegacyChat,
			Model:          "",
			CreatedAt:      stamp.Mtime,
			UpdatedAt:      stamp.Mtime,
			SizeBytes:      0,
			Archived:       false,
			// Cursor's legacy chat panel records no request id.
			LatestRequestID: "",
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
			Path:     file.Path,
			Selector: "",
			Stamp: conversation.FileStamp{
				Size:  info.Size(),
				Mtime: info.ModTime(),
			},
		})
		// The path shape decides whether a transcript is a subagent conversation:
		// its own place under a subagents/ directory, or a top-level twin whose
		// uuid appears under one in the same project. The resume link corroborates
		// that call and supplies the parent reference when it exists, because the
		// provider stated that link outright. A resume link that disagrees with
		// the path is logged and otherwise ignored: acting on it alone would hide
		// a conversation the layout says is the user's own, which is the loss this
		// classification exists to prevent.
		resumeParentID, resumedByAConversation := resumeLinks.parentOf(file.ConversationID)
		parentConversationID := firstNonEmptyString(file.ParentConversationID, file.TwinSubagentParentID)
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
	db, availability := openOptionalDatabase(ctx, root.GlobalDBPath, "global_db")
	if availability != databaseAvailabilityOpen {
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
			Path:     path,
			Selector: "",
			Stamp:    stampCoveringMetadata(stamp, info, hasInfo),
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

func discoverLegacyForRoot(
	ctx context.Context,
	root cursorstore.DataRoot,
	discovered map[string]discoveredArtifact,
) []conversation.ScanCandidate {
	listing, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_workspace_entries_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return nil
	}
	if listing.Unreadable > 0 {
		slog.WarnContext(ctx, "providers.cursor.parser.workspace_entries_partially_read", "concern", concern, "path", root.WorkspaceStorageDir, "listed", len(listing.Entries), "unreadable", listing.Unreadable)
	}
	rootHash := RootHash(root.RootDir)
	candidates := make([]conversation.ScanCandidate, 0)
	for _, entry := range listing.Entries {
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
	db, availability := openOptionalDatabase(ctx, entry.StateDBPath, "workspace_db")
	if availability != databaseAvailabilityOpen {
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
			Path:     path,
			Selector: "",
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
	// every parent transcript. The path shape is the classifier either way,
	// including the subagents/ twin of a top-level uuid, so this record carries
	// the same origin and parent Discover would have given it rather than an
	// unspecified origin that would bypass the setting.
	parentConversationID := firstNonEmptyString(file.ParentConversationID, file.TwinSubagentParentID)
	var artifact discoveredArtifact
	artifact.Kind = discoveredKindJSONL
	artifact.Path = file.Path
	artifact.ConversationID = file.ConversationID
	artifact.ProjectKey = file.ProjectKey
	artifact.Origin = originFromParentConversationID(parentConversationID)
	artifact.ParentConversationID = parentConversationID
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
	db, availability := openOptionalDatabase(ctx, root.GlobalDBPath, "global_db")
	if availability != databaseAvailabilityOpen {
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
	listing, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.list_workspace_entries_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return emptyDiscoveredArtifact(), fmt.Errorf("list cursor workspace entries: %w", err)
	}
	for _, entry := range listing.Entries {
		if entry.WorkspaceHash != workspaceHash {
			continue
		}
		return resolveLegacyDiscoveredForEntry(ctx, path, tabID, entry)
	}
	if listing.Unreadable > 0 {
		slog.WarnContext(ctx, "providers.cursor.parser.workspace_entries_partially_read", "concern", concern, "path", root.WorkspaceStorageDir, "workspace_hash", workspaceHash, "unreadable", listing.Unreadable)
		return emptyDiscoveredArtifact(), fmt.Errorf("cursor workspace %q not found among the %d workspaces that could be read, and %d could not", workspaceHash, len(listing.Entries), listing.Unreadable)
	}
	return emptyDiscoveredArtifact(), fmt.Errorf("cursor workspace %q not found", workspaceHash)
}

func resolveLegacyDiscoveredForEntry(
	ctx context.Context,
	path string,
	tabID string,
	entry cursorstore.WorkspaceEntry,
) (discoveredArtifact, error) {
	db, availability := openOptionalDatabase(ctx, entry.StateDBPath, "workspace_db")
	if availability != databaseAvailabilityOpen {
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

// databaseAvailability says what happened when Clyde reached for one of Cursor's
// databases.
//
// The two failures are not the same answer and must not share one. A database
// that is not there was read successfully as absent, so its store holds nothing;
// a database that is there and would not open was not read, so its store may hold
// exactly what the caller was looking for. Cursor's default data root is returned
// whether or not it exists, so on a machine where Cursor was never installed the
// first case is the ordinary one.
type databaseAvailability uint8

const (
	// databaseAvailabilityOpen means the database is open and the handle is usable.
	databaseAvailabilityOpen databaseAvailability = iota
	// databaseAvailabilityAbsent means the database is not there.
	databaseAvailabilityAbsent
	// databaseAvailabilityUnreadable means the database is there and would not
	// open, which is what a locked or permission-denied store looks like.
	databaseAvailabilityUnreadable
)

func openOptionalDatabase(ctx context.Context, path string, component string) (*sql.DB, databaseAvailability) {
	if _, err := os.Stat(path); err != nil {
		if cursorstore.StatSaysAbsent(err, path) {
			return nil, databaseAvailabilityAbsent
		}
		slog.WarnContext(ctx, "providers.cursor.parser.stat_db_failed", "concern", concern, "component", component, "path", path, "err", err)
		return nil, databaseAvailabilityUnreadable
	}
	db, err := cursorstore.OpenReadOnlyDatabase(ctx, path)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.open_db_failed", "concern", concern, "component", component, "path", path, "err", err)
		return nil, databaseAvailabilityUnreadable
	}
	return db, databaseAvailabilityOpen
}

// readWorkspaceRoot reads the folder a workspace is open on. A workspace stored
// without a descriptor is a window opened on no folder, so it has no root rather
// than an unreadable one, and saying so is not a failure worth a log line.
func readWorkspaceRoot(ctx context.Context, entry cursorstore.WorkspaceEntry) string {
	if entry.WorkspaceJSONPath == "" {
		return ""
	}
	workspaceRoot, err := cursorstore.ReadWorkspaceFolderPath(entry.WorkspaceJSONPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.read_workspace_folder_failed", "concern", concern, "path", entry.WorkspaceJSONPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return ""
	}
	return workspaceRoot
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
