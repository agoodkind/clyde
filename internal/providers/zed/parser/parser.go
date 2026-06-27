// Package parser discovers local Zed thread databases and implements the first
// Zed parser hooks for Clyde's conversation index.
package parser

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	zedstore "goodkind.io/clyde/internal/providers/zed/store"
	"goodkind.io/clyde/internal/transcript"
)

const concern = "providers.zed.parser"

// Parser discovers Zed thread database artifacts for later header and stream
// parsing work.
type Parser struct{}

var _ conversation.Parser = Parser{}

// New returns a Zed conversation parser.
func New() Parser {
	return Parser{}
}

// Provider reports that this parser handles Zed artifacts.
func (Parser) Provider() providerid.Provider {
	return providerid.ProviderZed
}

// Discover resolves local Zed data roots and returns readable thread database
// candidates for later scan stages.
func (Parser) Discover(ctx context.Context, _ map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	roots, err := zedstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.resolve_roots_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("resolve zed data roots: %w", err)
	}

	candidates := make([]conversation.ScanCandidate, 0, len(roots))
	for _, root := range roots {
		warmSidebarMetadata(ctx, root)
		candidate, ok := discoverThreadsDatabase(ctx, root)
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// ScanRecord returns no record until a later branch adds Zed thread header
// decoding.
func (Parser) ScanRecord(string, conversation.FileStamp) (conversation.Record, bool) {
	var record conversation.Record
	return record, false
}

// Stream yields no messages until a later branch adds Zed transcript parsing.
func (Parser) Stream(string, conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

func emptyCandidate() conversation.ScanCandidate {
	var candidate conversation.ScanCandidate
	return candidate
}

func discoverThreadsDatabase(ctx context.Context, root zedstore.DataRoot) (conversation.ScanCandidate, bool) {
	info, err := os.Stat(root.ThreadsDBPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.WarnContext(ctx, "providers.zed.parser.stat_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		}
		return emptyCandidate(), false
	}

	db, err := zedstore.OpenReadOnlyDatabase(ctx, root.ThreadsDBPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.open_threads_db_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return emptyCandidate(), false
	}
	defer func() { _ = db.Close() }()

	exists, err := zedstore.TableExists(ctx, db, "threads")
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.check_threads_table_failed", "concern", concern, "path", root.ThreadsDBPath, "err", err)
		return emptyCandidate(), false
	}
	if !exists {
		return emptyCandidate(), false
	}

	return conversation.ScanCandidate{
		Path: root.ThreadsDBPath,
		Stamp: conversation.FileStamp{
			Size:  info.Size(),
			Mtime: info.ModTime(),
		},
	}, true
}

func warmSidebarMetadata(ctx context.Context, root zedstore.DataRoot) {
	for _, dbPath := range root.MetadataDBPaths {
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		db, err := zedstore.OpenReadOnlyDatabase(ctx, dbPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.open_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			continue
		}
		_, readErr := zedstore.ReadSidebarThreads(ctx, db)
		_ = db.Close()
		if readErr != nil {
			slog.WarnContext(ctx, "providers.zed.parser.read_metadata_failed", "concern", concern, "path", dbPath, "err", readErr)
		}
	}
}
