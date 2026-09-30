package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

// ReadContextWindow bypasses cached headers and transcripts for virtual stores.
func (*Parser) ReadContextWindow(ctx context.Context, path, selector string, start, end int, options conversation.LoadOptions, visit func([]transcript.Message) error) error {
	if selector != "" {
		return errors.New("cursor context does not accept a selector")
	}
	if !strings.HasPrefix(path, virtualPathPrefix) {
		err := conversation.WithStableContextSources(ctx, []conversation.ContextSourceFile{{Path: path, Required: true}}, func() error {
			return visitCursorContext(ctx, streamPhysicalContext(ctx, path, options), start, end, visit)
		})
		if err != nil {
			return &contextReadError{operation: "read physical cursor context", cause: err}
		}
		return nil
	}
	virtual, err := ParseVirtualPath(path)
	if err != nil {
		return err
	}
	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		return &contextReadError{operation: "resolve cursor context roots", cause: err}
	}
	for _, root := range roots {
		if RootHash(root.RootDir) == virtual.RootHash {
			return readVirtualCursorContext(ctx, root, virtual, path, options, start, end, visit)
		}
	}
	return fmt.Errorf("cursor context source not found: %s", path)
}

func readVirtualCursorContext(ctx context.Context, root cursorstore.DataRoot, virtual CursorVirtualPath, path string, options conversation.LoadOptions, start, end int, visit func([]transcript.Message) error) error {
	switch virtual.Kind {
	case VirtualKindComposer:
		return readComposerContext(ctx, root, virtual.ID, path, options, start, end, visit)
	case VirtualKindLegacy:
		return readLegacyContext(ctx, root, virtual.ID, options, start, end, visit)
	default:
		return errors.New("unsupported cursor context kind")
	}
}

func readComposerContext(ctx context.Context, root cursorstore.DataRoot, id, path string, options conversation.LoadOptions, start, end int, visit func([]transcript.Message) error) error {
	err := withCursorContextDatabase(ctx, root.GlobalDBPath, func(db *sql.DB) error {
		header, found, readErr := cursorstore.ReadComposerHeader(ctx, db, id)
		if readErr != nil {
			return &contextReadError{operation: "read cursor context header", cause: readErr}
		}
		if !found {
			return errors.New("cursor context composer is absent")
		}
		var artifact discoveredArtifact
		artifact.Kind, artifact.Path, artifact.RootDir = discoveredKindComposer, path, root.RootDir
		artifact.ComposerID, artifact.ComposerHeader = id, header
		stream := iter.Seq2[transcript.Message, error](func(yield func(transcript.Message, error) bool) {
			streamComposerDatabase(ctx, db, root.GlobalDBPath, artifact, options, yield)
		})
		return visitCursorContext(ctx, stream, start, end, visit)
	})
	if err != nil {
		return &contextReadError{operation: "read composer context", cause: err}
	}
	return nil
}

func readLegacyContext(ctx context.Context, root cursorstore.DataRoot, id string, options conversation.LoadOptions, start, end int, visit func([]transcript.Message) error) error {
	workspace, tab, ok := splitLegacyID(id)
	if !ok {
		return errors.New("invalid cursor context legacy identity")
	}
	listing, err := root.ListWorkspaceEntries()
	if err != nil {
		return &contextReadError{operation: "list cursor context workspaces", cause: err}
	}
	for _, entry := range listing.Entries {
		if entry.WorkspaceHash != workspace {
			continue
		}
		err = withCursorContextDatabase(ctx, entry.StateDBPath, func(db *sql.DB) error {
			chat, found, readErr := cursorstore.ReadLegacyChatData(ctx, db)
			if readErr != nil {
				return &contextReadError{operation: "read cursor context legacy chat", cause: readErr}
			}
			if !found {
				return errors.New("cursor context legacy chat is absent")
			}
			for _, current := range chat.Tabs {
				if current.TabID == tab && len(current.Bubbles) > 0 {
					return visitCursorContext(ctx, streamLegacyContext(current, options), start, end, visit)
				}
			}
			return errors.New("cursor context legacy tab is absent")
		})
		if err != nil {
			return &contextReadError{operation: "read legacy cursor context", cause: err}
		}
		return nil
	}
	return errors.New("cursor context workspace is absent")
}

func withCursorContextDatabase(ctx context.Context, path string, read func(*sql.DB) error) (err error) {
	var db *sql.DB
	err = conversation.WithStableContextSources(ctx, []conversation.ContextSourceFile{{Path: path, Required: true}}, func() error {
		var openErr error
		db, openErr = cursorstore.OpenReadOnlyDatabase(ctx, path)
		if openErr != nil {
			return &contextReadError{operation: "open cursor context database", cause: openErr}
		}
		return nil
	})
	if db != nil {
		defer func() {
			if closeErr := db.Close(); closeErr != nil {
				err = errors.Join(err, &contextReadError{operation: "close cursor context database", cause: closeErr})
			}
		}()
	}
	if err != nil {
		return &contextReadError{operation: "admit cursor context database", cause: err}
	}
	if err := conversation.WithStableContextSources(ctx, cursorContextFiles(path), func() error { return read(db) }); err != nil {
		return &contextReadError{operation: "verify cursor context database", cause: err}
	}
	return nil
}

func streamLegacyContext(tab cursorstore.LegacyChatTab, options conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		for _, bubble := range tab.Bubbles {
			message, include := mapLegacyBubble(bubble, options)
			if include && !yield(message, nil) {
				return
			}
		}
	}
}

func streamPhysicalContext(ctx context.Context, path string, options conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		err := cursorjsonl.StreamMessages(path, func(message cursorjsonl.TranscriptMessage) error {
			if err := ctx.Err(); err != nil {
				return &contextReadError{operation: "read cursor context transcript", cause: err}
			}
			mapped, include := mapJSONLMessage(message, options)
			if include && !yield(mapped, nil) {
				return cursorjsonl.ErrStopStreaming
			}
			return nil
		})
		if err != nil && !errors.Is(err, cursorjsonl.ErrStopStreaming) {
			yield(emptyMessage(), err)
		}
	}
}

func visitCursorContext(ctx context.Context, stream iter.Seq2[transcript.Message, error], start, end int, visit func([]transcript.Message) error) error {
	if err := conversation.VisitContextWindow(ctx, stream, start, end, visit); err != nil {
		return &contextReadError{operation: "visit cursor context", cause: err}
	}
	return nil
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

func cursorContextFiles(path string) []conversation.ContextSourceFile {
	return []conversation.ContextSourceFile{
		{Path: path, Required: true},
		{Path: path + "-wal", Required: false},
		{Path: path + "-shm", Required: false},
	}
}

func streamComposer(discovered discoveredArtifact, options conversation.LoadOptions, yield func(transcript.Message, error) bool) {
	streamComposerContext(context.Background(), discovered, options, yield)
}
