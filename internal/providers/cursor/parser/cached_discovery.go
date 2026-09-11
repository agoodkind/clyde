package parser

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

func (p *Parser) discoverCachedSQLite(
	ctx context.Context,
	prior map[string]conversation.Record,
	priorStamps map[string]conversation.FileStamp,
	seenConversationIDs map[string]bool,
) ([]conversation.ScanCandidate, []conversation.ScanCandidate, bool) {
	sources, err := cursorSourceStamps(ctx)
	if err != nil || !sameCursorSourceStamps(sources, priorStamps) {
		return nil, nil, false
	}
	candidates := make([]conversation.ScanCandidate, 0, len(prior))
	for _, record := range prior {
		if record.Provider != providerid.ProviderCursor || record.ArtifactPath == "" ||
			record.ArtifactKind == artifactKindCursorAgent || seenConversationIDs[record.NativeID] {
			continue
		}
		stamp, ok := priorStamps[cachedRecordStampKey(record.ArtifactPath, record.Selector)]
		if !ok {
			return nil, nil, false
		}
		candidates = append(candidates, conversation.ScanCandidate{
			Path:            record.ArtifactPath,
			Selector:        record.Selector,
			Stamp:           stamp,
			MetadataChanged: false,
		})
		seenConversationIDs[record.NativeID] = true
	}
	return candidates, cursorSourceCandidates(sources), true
}

func (p *Parser) sourceCheckpointCandidates(ctx context.Context) []conversation.ScanCandidate {
	sources, err := cursorSourceStamps(ctx)
	if err != nil {
		return nil
	}
	return cursorSourceCandidates(sources)
}

func cursorSourceStamps(ctx context.Context) (map[string]conversation.FileStamp, error) {
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve cursor data roots: %w", err)
	}
	sources := make(map[string]conversation.FileStamp)
	for _, root := range roots {
		addCursorSourceStamp(sources, root.GlobalDBPath)
		addCursorSourceStamp(sources, root.GlobalDBPath+"-wal")
		addCursorSourceStamp(sources, root.WorkspaceStorageDir)
		listing, listingErr := root.ListWorkspaceEntries()
		if listingErr != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.list_workspace_entries_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", listingErr)
			return nil, fmt.Errorf("list cursor workspace entries: %w", listingErr)
		}
		for _, entry := range listing.DiscoveryEntries() {
			addCursorSourceStamp(sources, entry.StateDBPath)
			addCursorSourceStamp(sources, entry.StateDBPath+"-wal")
			if entry.WorkspaceJSONPath != "" {
				addCursorSourceStamp(sources, entry.WorkspaceJSONPath)
			}
		}
	}
	return sources, nil
}

func addCursorSourceStamp(sources map[string]conversation.FileStamp, path string) {
	info, err := os.Stat(path)
	if err != nil {
		sources[path] = conversation.FileStamp{Size: 0, Mtime: time.Time{}}
		return
	}
	sources[path] = conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()}
}

func sameCursorSourceStamps(sources map[string]conversation.FileStamp, prior map[string]conversation.FileStamp) bool {
	priorSources := make(map[string]conversation.FileStamp)
	for key, stamp := range prior {
		if !strings.HasPrefix(key, checkpointPrefix) {
			continue
		}
		priorSources[strings.TrimPrefix(key, checkpointPrefix)] = stamp
	}
	if len(priorSources) == 0 || len(priorSources) != len(sources) {
		return false
	}
	for path, stamp := range sources {
		priorStamp, ok := priorSources[path]
		if !ok || !priorStamp.Equal(stamp) {
			return false
		}
	}
	return true
}

func cursorSourceCandidates(sources map[string]conversation.FileStamp) []conversation.ScanCandidate {
	candidates := make([]conversation.ScanCandidate, 0, len(sources))
	for path, stamp := range sources {
		candidates = append(candidates, conversation.ScanCandidate{
			Path: checkpointPrefix + path, Selector: "", Stamp: stamp, MetadataChanged: false,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates
}

func cachedRecordStampKey(path, selector string) string {
	if selector == "" {
		return path
	}
	return path + "\x00" + selector
}

func (p *Parser) finishDiscovery(
	candidates []conversation.ScanCandidate,
	prior map[string]conversation.Record,
	discovered map[string]discoveredArtifact,
) ([]conversation.ScanCandidate, error) {
	for i := range candidates {
		artifact := discovered[candidates[i].Path]
		previous, ok := prior[candidates[i].Path]
		if !ok || artifact.Kind != discoveredKindJSONL {
			continue
		}
		previousParent := ""
		if previous.Lineage != nil {
			previousParent = previous.Lineage.ParentNativeID
		}
		candidates[i].MetadataChanged = previous.Origin != artifact.Origin || previousParent != artifact.ParentConversationID
		if artifact.TranscriptHeader != nil {
			header := artifact.TranscriptHeader
			title := firstNonEmptyString(truncateTitle(header.FirstUserText), untitledCursorConversationText)
			candidates[i].MetadataChanged = candidates[i].MetadataChanged || previous.Title != title || previous.TitleUncertain != header.FirstUserTextUncertain
		}
	}

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
