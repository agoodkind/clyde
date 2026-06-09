package codexstore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// SessionIndexEntry is the typed append-only row Codex writes to
// CODEX_HOME/session_index.jsonl. The latest row wins for name/id lookups.
type SessionIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
	UpdatedAt  string `json:"updated_at"`
}

// SessionIndex holds the latest known thread names from Codex's append-only
// session index.
type SessionIndex struct {
	entriesByID []SessionIndexEntry
}

// ReadSessionIndex is part of Clyde's typed adapter surface.
func ReadSessionIndex(ctx context.Context, path string) (SessionIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionIndex{entriesByID: nil}, nil
		}
		slog.WarnContext(ctx, "codex.store.session_index.open_failed", "concern", "providers.codex.store", "path", path, "err", err)
		return SessionIndex{}, fmt.Errorf("open codex session index %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	latest := make(map[string]SessionIndexEntry)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry SessionIndexEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entry.ID = strings.TrimSpace(entry.ID)
		entry.ThreadName = strings.TrimSpace(entry.ThreadName)
		if entry.ID == "" || entry.ThreadName == "" {
			continue
		}
		latest[entry.ID] = entry
	}
	if err := scanner.Err(); err != nil {
		return SessionIndex{}, fmt.Errorf("scan codex session index %s: %w", path, err)
	}
	out := make([]SessionIndexEntry, 0, len(latest))
	for _, entry := range latest {
		out = append(out, entry)
	}
	return SessionIndex{entriesByID: out}, nil
}

// ThreadName is part of Clyde's typed adapter surface.
func (idx SessionIndex) ThreadName(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	for _, entry := range idx.entriesByID {
		if entry.ID == id {
			return entry.ThreadName
		}
	}
	return ""
}
