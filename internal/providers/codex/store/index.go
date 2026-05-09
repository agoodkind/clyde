package codexstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/slogger"
)

var currentTime = time.Now

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

func ReadSessionIndex(path string) (SessionIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionIndex{}, nil
		}
		return SessionIndex{}, err
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
		return SessionIndex{}, err
	}
	out := make([]SessionIndexEntry, 0, len(latest))
	for _, entry := range latest {
		out = append(out, entry)
	}
	return SessionIndex{entriesByID: out}, nil
}

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

// NormalizeThreadName matches Codex's thread/name/set normalization boundary:
// whitespace-only names are rejected, and otherwise the trimmed title is stored.
func NormalizeThreadName(name string) (string, bool) {
	normalized := strings.TrimSpace(name)
	return normalized, normalized != ""
}

// AppendThreadName records a Codex thread/name/set-compatible title update in
// CODEX_HOME/session_index.jsonl. The file is append-only, and latest row wins.
func AppendThreadName(paths StorePaths, threadID, name string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return errors.New("missing codex thread id")
	}
	normalized, ok := NormalizeThreadName(name)
	if !ok {
		return errors.New("thread name must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(paths.SessionIndexPath), 0o755); err != nil {
		// TODO: thread ctx for correlation
		slog.Warn("codex.store.session_index.mkdir_failed",
			"component", "codex",
			"subcomponent", "store",
			"concern", slogger.ConcernProviderCodexLifecycle,
			"path", paths.SessionIndexPath,
			"err", err,
		)
		return fmt.Errorf("create codex session index directory: %w", err)
	}
	file, err := os.OpenFile(paths.SessionIndexPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		// TODO: thread ctx for correlation
		slog.Warn("codex.store.session_index.open_failed",
			"component", "codex",
			"subcomponent", "store",
			"concern", slogger.ConcernProviderCodexLifecycle,
			"path", paths.SessionIndexPath,
			"err", err,
		)
		return fmt.Errorf("open codex session index for append: %w", err)
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(SessionIndexEntry{
		ID:         threadID,
		ThreadName: normalized,
		UpdatedAt:  currentTime().UTC().Format(time.RFC3339),
	}); err != nil {
		// TODO: thread ctx for correlation
		slog.Warn("codex.store.session_index.encode_failed",
			"component", "codex",
			"subcomponent", "store",
			"concern", slogger.ConcernProviderCodexLifecycle,
			"path", paths.SessionIndexPath,
			"err", err,
		)
		return fmt.Errorf("append codex session index entry: %w", err)
	}
	if err := file.Close(); err != nil {
		// TODO: thread ctx for correlation
		slog.Warn("codex.store.session_index.close_failed",
			"component", "codex",
			"subcomponent", "store",
			"concern", slogger.ConcernProviderCodexLifecycle,
			"path", paths.SessionIndexPath,
			"err", err,
		)
		return fmt.Errorf("close codex session index after append: %w", err)
	}
	return nil
}
