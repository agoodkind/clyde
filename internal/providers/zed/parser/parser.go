// Package parser reads local Zed thread databases and implements the earliest
// Zed parser hooks for Clyde's conversation index.
package parser

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	zedstore "goodkind.io/clyde/internal/providers/zed/store"
	"goodkind.io/clyde/internal/transcript"
)

const concern = "providers.zed.parser"

type discoveredThread struct {
	Row      zedstore.ThreadRow
	Metadata zedstore.SidebarThreadMetadata
	RootDir  string
	Channel  string
}

type metadataWithChannel struct {
	Metadata zedstore.SidebarThreadMetadata
	Channel  string
}

// Parser discovers Zed thread candidates and caches the rows needed by later
// scan stages.
type Parser struct {
	mu         sync.Mutex
	discovered map[string]discoveredThread
}

var _ conversation.Parser = (*Parser)(nil)

// New returns a Zed conversation parser.
func New() *Parser {
	return &Parser{mu: sync.Mutex{}, discovered: make(map[string]discoveredThread)}
}

// Provider reports that this parser handles Zed artifacts.
func (*Parser) Provider() providerid.Provider {
	return providerid.ProviderZed
}

// Discover resolves local Zed data roots, reads thread rows plus sidebar
// metadata, and returns native Zed thread candidates.
func (p *Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	roots, err := zedstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.resolve_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve zed data roots: %w", err)
	}

	candidates := make([]conversation.ScanCandidate, 0)
	discovered := make(map[string]discoveredThread)
	for _, root := range roots {
		rows, err := readThreadRowsForRoot(ctx, root)
		if err != nil {
			return nil, err
		}
		rows = parsableThreadRows(ctx, root.ThreadsDBPath, rows)
		if len(rows) == 0 {
			continue
		}

		metadataBySession, err := readMetadataForRoot(ctx, root)
		if err != nil {
			return nil, err
		}

		for _, row := range rows {
			metadata, ok := metadataBySession[row.SessionID]
			if !ok || metadata.Metadata.AgentID != "" {
				continue
			}
			path := BuildVirtualPath(root.RootDir, metadata.Channel, row.SessionID)
			if path == "" {
				continue
			}
			candidates = append(candidates, conversation.ScanCandidate{
				Path:  path,
				Stamp: conversation.FileStamp{Size: int64(len(row.Data)), Mtime: row.UpdatedAt},
			})
			discovered[path] = discoveredThread{
				Row:      row,
				Metadata: metadata.Metadata,
				RootDir:  root.RootDir,
				Channel:  metadata.Channel,
			}
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Path < candidates[j].Path
	})

	p.mu.Lock()
	p.discovered = discovered
	p.mu.Unlock()

	return candidates, nil
}

// ScanRecord returns no record until a later branch adds Zed thread header
// decoding.
func (*Parser) ScanRecord(string, conversation.FileStamp) (conversation.Record, bool) {
	var record conversation.Record
	return record, false
}

// Stream yields no messages until a later branch adds Zed transcript parsing.
func (*Parser) Stream(string, conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

func readThreadRowsForRoot(ctx context.Context, root zedstore.DataRoot) ([]zedstore.ThreadRow, error) {
	if _, err := os.Stat(root.ThreadsDBPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		slog.WarnContext(ctx, "providers.zed.parser.stat_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, fmt.Errorf("stat zed threads db %s: %w", root.ThreadsDBPath, err)
	}

	db, err := zedstore.OpenReadOnlyDatabase(ctx, root.ThreadsDBPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.open_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, fmt.Errorf("open zed threads db %s: %w", root.ThreadsDBPath, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := zedstore.ReadThreadRows(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.read_threads_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return nil, fmt.Errorf("read zed thread rows from %s: %w", root.ThreadsDBPath, err)
	}

	return rows, nil
}

func readMetadataForRoot(ctx context.Context, root zedstore.DataRoot) (map[string]metadataWithChannel, error) {
	metadataBySession := make(map[string]metadataWithChannel)
	for _, dbPath := range root.MetadataDBPaths {
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.WarnContext(ctx, "providers.zed.parser.stat_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			return nil, fmt.Errorf("stat zed metadata db %s: %w", dbPath, err)
		}

		db, err := zedstore.OpenReadOnlyDatabase(ctx, dbPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.open_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			return nil, fmt.Errorf("open zed metadata db %s: %w", dbPath, err)
		}

		metadataRows, err := zedstore.ReadSidebarThreads(ctx, db)
		_ = db.Close()
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.read_metadata_failed", "concern", concern, "path", dbPath, "err", err)
			return nil, fmt.Errorf("read zed sidebar rows from %s: %w", dbPath, err)
		}

		channel := filepath.Base(filepath.Dir(dbPath))
		for _, row := range metadataRows {
			current, ok := metadataBySession[row.SessionID]
			if !ok || row.UpdatedAt.After(current.Metadata.UpdatedAt) {
				metadataBySession[row.SessionID] = metadataWithChannel{Metadata: row, Channel: channel}
			}
		}
	}

	return metadataBySession, nil
}

func parsableThreadRows(ctx context.Context, path string, rows []zedstore.ThreadRow) []zedstore.ThreadRow {
	filtered := make([]zedstore.ThreadRow, 0, len(rows))
	for _, row := range rows {
		if _, err := zedstore.ParseThreadDocument(row.DataType, row.Data); err == nil {
			filtered = append(filtered, row)
			continue
		} else {
			slog.WarnContext(ctx, "providers.zed.parser.parse_thread_payload_failed", "concern", concern, "path", path, "session_id", row.SessionID, "data_type", string(row.DataType), "err", err)
		}
	}
	return filtered
}
