package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	zedstore "goodkind.io/clyde/internal/providers/zed/store"
	"goodkind.io/clyde/internal/transcript"
)

// ReadContextWindow reloads the selected thread instead of cached discovery.
func (*Parser) ReadContextWindow(ctx context.Context, path, selector string, start, end int, options conversation.LoadOptions, visit func([]transcript.Message) error) error {
	if selector != "" {
		return errors.New("zed context does not accept a selector")
	}
	virtual, err := ParseVirtualPath(path)
	if err != nil {
		return err
	}
	roots, err := zedstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		return &contextReadError{operation: "resolve zed context roots", cause: err}
	}
	for _, root := range roots {
		if RootHash(root.RootDir) != virtual.RootHash {
			continue
		}
		return withZedContextDatabases(ctx, root, virtual, func(metadata, threads *sql.DB) error {
			discovered, readErr := readFreshContextThread(ctx, root, virtual, metadata, threads)
			if readErr != nil {
				return readErr
			}
			stream := iter.Seq2[transcript.Message, error](func(yield func(transcript.Message, error) bool) {
				streamDiscoveredContext(discovered, options, yield)
			})
			if err := conversation.VisitContextWindow(ctx, stream, start, end, visit); err != nil {
				return &contextReadError{operation: "visit zed context", cause: err}
			}
			return nil
		})
	}
	return fmt.Errorf("zed context source not found: %s", path)
}

func withZedContextDatabases(ctx context.Context, root zedstore.DataRoot, virtual VirtualPath, read func(*sql.DB, *sql.DB) error) (err error) {
	paths := zedContextDatabasePaths(root, virtual)
	if len(paths) == 0 {
		return errors.New("zed context channel is absent")
	}
	mains := make([]conversation.ContextSourceFile, 0, len(paths))
	files := make([]conversation.ContextSourceFile, 0, 3*len(paths))
	for _, path := range paths {
		mains = append(mains, conversation.ContextSourceFile{Path: path, Required: true})
		files = append(files, zedContextFiles(path)...)
	}
	dbs := make([]*sql.DB, 0, len(paths))
	defer func() {
		for _, db := range dbs {
			if closeErr := db.Close(); closeErr != nil {
				err = errors.Join(err, &contextReadError{operation: "close zed context database", cause: closeErr})
			}
		}
	}()
	err = conversation.WithStableContextSources(ctx, mains, func() error {
		for _, path := range paths {
			db, openErr := zedstore.OpenContextReadOnlyDatabase(ctx, path)
			if openErr != nil {
				return &contextReadError{operation: "open zed context database", cause: openErr}
			}
			dbs = append(dbs, db)
		}
		return nil
	})
	if err != nil {
		return &contextReadError{operation: "admit zed context databases", cause: err}
	}
	var threads *sql.DB
	if len(dbs) == 2 {
		threads = dbs[1]
	}
	if err := conversation.WithStableContextSources(ctx, files, func() error { return read(dbs[0], threads) }); err != nil {
		return &contextReadError{operation: "verify zed context databases", cause: err}
	}
	return nil
}

func zedContextDatabasePaths(root zedstore.DataRoot, virtual VirtualPath) []string {
	for _, path := range root.MetadataDBPaths {
		if filepath.Base(filepath.Dir(path)) != virtual.Channel {
			continue
		}
		_, terminal := strings.CutPrefix(virtual.SessionID, terminalPathPrefix)
		if terminal {
			return []string{path}
		}
		return []string{path, root.ThreadsDBPath}
	}
	return nil
}

func readFreshContextThread(ctx context.Context, root zedstore.DataRoot, virtual VirtualPath, metadataDB, threads *sql.DB) (discoveredThread, error) {
	terminalID, terminal := strings.CutPrefix(virtual.SessionID, terminalPathPrefix)
	if terminal {
		metadata, found, err := zedstore.ReadSidebarTerminalByID(ctx, metadataDB, terminalID)
		if err != nil {
			return emptyDiscoveredThread(), &contextReadError{operation: "read zed context terminal", cause: err}
		}
		if !found {
			return emptyDiscoveredThread(), errors.New("zed context terminal is absent")
		}
		return newTerminalDiscoveredThread(metadata, root.RootDir, virtual.Channel), nil
	}
	metadata, found, err := zedstore.ReadSidebarThreadBySession(ctx, metadataDB, virtual.SessionID)
	if err != nil {
		return emptyDiscoveredThread(), &contextReadError{operation: "read zed context metadata", cause: err}
	}
	if !found {
		return emptyDiscoveredThread(), errors.New("zed context metadata is absent")
	}
	row, found, err := zedstore.ReadThreadRowByID(ctx, threads, virtual.SessionID)
	if err != nil {
		return emptyDiscoveredThread(), &contextReadError{operation: "read zed context thread", cause: err}
	}
	if !found {
		return emptyDiscoveredThread(), errors.New("zed context thread is absent")
	}
	thread, err := zedstore.ParseThreadDocument(row.DataType, row.Data)
	if err != nil {
		return emptyDiscoveredThread(), &contextReadError{operation: "parse zed context thread", cause: err}
	}
	return newNativeDiscoveredThread(row, thread, metadata, root.RootDir, virtual.Channel), nil
}

func zedContextFiles(path string) []conversation.ContextSourceFile {
	return []conversation.ContextSourceFile{
		{Path: path, Required: true},
		{Path: path + "-wal", Required: false},
		{Path: path + "-shm", Required: false},
	}
}

type contextReadError struct {
	operation string
	cause     error
}

func (failure *contextReadError) Error() string {
	return failure.operation + ": " + failure.cause.Error()
}

func (failure *contextReadError) Unwrap() error {
	return failure.cause
}
