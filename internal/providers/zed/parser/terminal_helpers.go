package parser

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	zedstore "goodkind.io/clyde/internal/providers/zed/store"
)

type terminalMetadataWithChannel struct {
	Metadata zedstore.SidebarTerminalThreadMetadata
	Channel  string
}

func emptyDiscoveredThread() discoveredThread {
	return discoveredThread{
		Row: zedstore.ThreadRow{
			ThreadID:       "",
			ParentThreadID: "",
			Summary:        "",
			UpdatedAt:      time.Time{},
			CreatedAt:      time.Time{},
			FolderPaths:    nil,
			DataType:       "",
			Data:           nil,
		},
		Thread: zedstore.ThreadDocument{
			Version:         "",
			Title:           "",
			UpdatedAt:       time.Time{},
			DetailedSummary: "",
			Model:           nil,
			SubagentContext: nil,
			Messages:        nil,
		},
		Metadata: zedstore.SidebarThreadMetadata{
			SessionID:         "",
			AgentID:           "",
			Title:             "",
			TitleOverride:     "",
			UpdatedAt:         time.Time{},
			CreatedAt:         time.Time{},
			FolderPaths:       nil,
			MainWorktreePaths: nil,
			Archived:          false,
		},
		Terminal: nil,
		Source:   "",
		RootDir:  "",
		Channel:  "",
	}
}

func newNativeDiscoveredThread(
	row zedstore.ThreadRow,
	thread zedstore.ThreadDocument,
	metadata zedstore.SidebarThreadMetadata,
	rootDir string,
	channel string,
) discoveredThread {
	return discoveredThread{
		Row:      row,
		Thread:   thread,
		Metadata: metadata,
		Terminal: nil,
		Source:   artifactKindZedThread,
		RootDir:  rootDir,
		Channel:  channel,
	}
}

func newTerminalDiscoveredThread(
	terminal zedstore.SidebarTerminalThreadMetadata,
	rootDir string,
	channel string,
) discoveredThread {
	return discoveredThread{
		Row: zedstore.ThreadRow{
			ThreadID:       "",
			ParentThreadID: "",
			Summary:        "",
			UpdatedAt:      time.Time{},
			CreatedAt:      time.Time{},
			FolderPaths:    nil,
			DataType:       "",
			Data:           nil,
		},
		Thread: zedstore.ThreadDocument{
			Version:         "",
			Title:           "",
			UpdatedAt:       time.Time{},
			DetailedSummary: "",
			Model:           nil,
			SubagentContext: nil,
			Messages:        nil,
		},
		Metadata: zedstore.SidebarThreadMetadata{
			SessionID:         "",
			AgentID:           "",
			Title:             "",
			TitleOverride:     "",
			UpdatedAt:         time.Time{},
			CreatedAt:         time.Time{},
			FolderPaths:       nil,
			MainWorktreePaths: nil,
			Archived:          false,
		},
		Terminal: &terminal,
		Source:   artifactKindZedTerm,
		RootDir:  rootDir,
		Channel:  channel,
	}
}

func readTerminalMetadataForRoot(ctx context.Context, root zedstore.DataRoot) []terminalMetadataWithChannel {
	terminalMetadata := make([]terminalMetadataWithChannel, 0)
	for _, dbPath := range root.MetadataDBPaths {
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.WarnContext(ctx, "providers.zed.parser.stat_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			continue
		}

		db, err := zedstore.OpenReadOnlyDatabase(ctx, dbPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.open_metadata_db_failed", "concern", concern, "path", dbPath, "err", err)
			continue
		}
		terminalRows, err := zedstore.ReadSidebarTerminalThreads(ctx, db)
		_ = db.Close()
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.read_terminal_metadata_failed", "concern", concern, "path", dbPath, "err", err)
			continue
		}

		channel := filepath.Base(filepath.Dir(dbPath))
		for _, row := range terminalRows {
			terminalMetadata = append(terminalMetadata, terminalMetadataWithChannel{Metadata: row, Channel: channel})
		}
	}
	return terminalMetadata
}

func resolveTerminalDiscoveredThread(
	ctx context.Context,
	root zedstore.DataRoot,
	channel string,
	terminalID string,
) (discoveredThread, bool, error) {
	terminal, ok, err := readSidebarTerminalMetadataForRoot(ctx, root, terminalID, channel)
	if err != nil {
		return emptyDiscoveredThread(), false, err
	}
	if !ok {
		return emptyDiscoveredThread(), false, nil
	}
	return newTerminalDiscoveredThread(terminal.Metadata, root.RootDir, terminal.Channel), true, nil
}

func resolveNativeDiscoveredThread(
	ctx context.Context,
	root zedstore.DataRoot,
	channel string,
	sessionID string,
) (discoveredThread, bool, error) {
	row, ok, err := readThreadRowByIDForRoot(ctx, root, sessionID)
	if err != nil {
		return emptyDiscoveredThread(), false, err
	}
	if !ok {
		return emptyDiscoveredThread(), false, nil
	}
	metadata, ok, err := readSidebarThreadMetadataForRoot(ctx, root, sessionID, channel)
	if err != nil {
		return emptyDiscoveredThread(), false, err
	}
	if !ok || metadata.Metadata.AgentID != "" {
		return emptyDiscoveredThread(), false, nil
	}
	thread, err := zedstore.ParseThreadDocument(row.DataType, row.Data)
	if err != nil {
		slog.WarnContext(ctx, "providers.zed.parser.parse_thread_document_failed", "concern", concern, "path", root.ThreadsDBPath, "session_id", sessionID, "err", err)
		return emptyDiscoveredThread(), false, fmt.Errorf("parse zed thread document: %w", err)
	}
	return newNativeDiscoveredThread(row, thread, metadata.Metadata, root.RootDir, metadata.Channel), true, nil
}

func readSidebarTerminalMetadataForRoot(
	ctx context.Context,
	root zedstore.DataRoot,
	terminalID string,
	channel string,
) (terminalMetadataWithChannel, bool, error) {
	var emptyMetadata terminalMetadataWithChannel

	for _, dbPath := range root.MetadataDBPaths {
		dbChannel := filepath.Base(filepath.Dir(dbPath))
		if dbChannel != channel {
			continue
		}
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.WarnContext(ctx, "providers.zed.parser.stat_terminal_metadata_db_failed", "concern", concern, "path", dbPath, "terminal_id", terminalID, "err", err)
			return emptyMetadata, false, fmt.Errorf("stat zed terminal metadata db %s: %w", dbPath, err)
		}

		db, err := zedstore.OpenReadOnlyDatabase(ctx, dbPath)
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.open_terminal_metadata_db_failed", "concern", concern, "path", dbPath, "terminal_id", terminalID, "err", err)
			return emptyMetadata, false, fmt.Errorf("open zed terminal metadata db %s: %w", dbPath, err)
		}
		row, ok, err := zedstore.ReadSidebarTerminalByID(ctx, db, terminalID)
		_ = db.Close()
		if err != nil {
			slog.WarnContext(ctx, "providers.zed.parser.read_sidebar_terminal_by_id_failed", "concern", concern, "path", dbPath, "terminal_id", terminalID, "err", err)
			return emptyMetadata, false, fmt.Errorf("read zed terminal sidebar row %q from %s: %w", terminalID, dbPath, err)
		}
		if ok {
			return terminalMetadataWithChannel{Metadata: row, Channel: dbChannel}, true, nil
		}
	}
	return emptyMetadata, false, nil
}

func terminalSessionID(terminalID string) string {
	trimmed := strings.TrimSpace(terminalID)
	if trimmed == "" {
		return ""
	}
	return terminalPathPrefix + trimmed
}

func terminalMetadataStamp(metadata zedstore.SidebarTerminalThreadMetadata) conversation.FileStamp {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.Join([]string{
		metadata.TerminalID,
		metadata.Title,
		metadata.CustomTitle,
		metadata.CreatedAt.Format(time.RFC3339Nano),
		metadata.WorkingDirectory,
		strings.Join(metadata.FolderPaths, "\n"),
		strings.Join(metadata.MainWorktreePaths, "\n"),
		metadata.RemoteConnection,
	}, "\n")))
	return conversation.FileStamp{
		Size:  int64(hash.Sum64() & 0x7fffffffffffffff),
		Mtime: metadata.CreatedAt,
	}
}

func terminalMetadataUnavailableText(metadata zedstore.SidebarTerminalThreadMetadata) string {
	title := firstNonEmptyString(metadata.CustomTitle, metadata.Title, metadata.TerminalID)
	return "Zed terminal metadata exists for " + title + ", but Clyde did not find transcript content for that terminal entry."
}
