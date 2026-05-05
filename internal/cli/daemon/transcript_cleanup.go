package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

// transcriptCleanupLoop returns a daemonsvc.ExtraLoop that hourly walks the
// per-chat transcript root and evicts <chat_key>.jsonl files that exceed the
// configured max-age and max-chat retention. Disabled when the transcript
// feature is off.
func transcriptCleanupLoop() daemonsvc.ExtraLoop {
	return func(log *slog.Logger) func() {
		cfg, err := config.LoadGlobalOrDefault()
		if err != nil {
			log.LogAttrs(context.Background(), slog.LevelWarn, "transcript.cleanup.config_load_failed",
				slog.String("component", "transcript-cleanup"),
				slog.Any("err", err),
			)
			return nil
		}
		if !cfg.Logging.Transcript.IsEnabled() {
			return nil
		}
		root := transcriptRoot(cfg)
		interval := time.Hour
		maxAge := time.Duration(cfg.Logging.Transcript.MaxAgeDays) * 24 * time.Hour
		maxChats := cfg.Logging.Transcript.MaxChats

		log.LogAttrs(context.Background(), slog.LevelInfo, "transcript.cleanup.scheduled",
			slog.String("component", "transcript-cleanup"),
			slog.String("root", root),
			slog.Int64("interval_ms", interval.Milliseconds()),
			slog.Int("max_age_days", cfg.Logging.Transcript.MaxAgeDays),
			slog.Int("max_chats", maxChats),
		)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.WarnContext(ctx, "transcript.cleanup.loop.panicked",
						"component", "transcript-cleanup",
						"panic", r,
					)
				}
			}()
			ticker := time.NewTicker(interval)
			defer func() { ticker.Stop() }()
			runTranscriptCleanup(ctx, log, root, maxAge, maxChats, time.Now)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runTranscriptCleanup(ctx, log, root, maxAge, maxChats, time.Now)
				}
			}
		}()
		return cancel
	}
}

func transcriptRoot(cfg *config.Config) string {
	base := strings.TrimSpace(cfg.Logging.Paths.Daemon)
	if base == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return filepath.Join(os.TempDir(), "clyde", "logs", "chats")
			}
			state = filepath.Join(home, ".local", "state")
		}
		base = filepath.Join(state, "clyde", "clyde-daemon.jsonl")
	}
	return filepath.Join(filepath.Dir(base), "logs", "chats")
}

// transcriptChatFile is a single per-chat transcript file under the root with
// its modification time, used as the unit of cleanup-loop bookkeeping.
type transcriptChatFile struct {
	name string
	path string
	mod  time.Time
}

// runTranscriptCleanup executes one cleanup pass. Exposed package-private for
// tests with an injected now func.
func runTranscriptCleanup(
	ctx context.Context,
	log *slog.Logger,
	root string,
	maxAge time.Duration,
	maxChats int,
	now func() time.Time,
) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.LogAttrs(ctx, slog.LevelWarn, "transcript.cleanup.readdir_failed",
			slog.String("component", "transcript-cleanup"),
			slog.String("root", root),
			slog.Any("err", err),
		)
		return
	}
	cutoff := now().Add(-maxAge)
	chats, evictedAge := evictAgedTranscriptFiles(ctx, log, root, entries, cutoff)
	evictedCount := evictExcessTranscriptFiles(ctx, log, chats, maxChats)
	log.LogAttrs(ctx, slog.LevelInfo, "transcript.cleanup.tick_completed",
		slog.String("component", "transcript-cleanup"),
		slog.Int("evicted_age", evictedAge),
		slog.Int("evicted_count", evictedCount),
		slog.Int("retained", len(chats)-evictedCount),
	)
}

// evictAgedTranscriptFiles removes per-chat files whose mtime is older than
// cutoff and returns the surviving files plus the eviction count. Directories
// (stale per-turn layout from earlier versions) are skipped silently.
func evictAgedTranscriptFiles(
	ctx context.Context,
	log *slog.Logger,
	root string,
	entries []os.DirEntry,
	cutoff time.Time,
) ([]transcriptChatFile, int) {
	chats := make([]transcriptChatFile, 0, len(entries))
	evictedAge := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		filePath := filepath.Join(root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		mod := info.ModTime()
		if !mod.IsZero() && mod.Before(cutoff) {
			err = os.Remove(filePath)
			if err != nil {
				log.LogAttrs(ctx, slog.LevelWarn, "transcript.cleanup.remove_failed",
					slog.String("component", "transcript-cleanup"),
					slog.String("path", filePath),
					slog.Any("err", err),
				)
				continue
			}
			evictedAge++
			continue
		}
		chats = append(chats, transcriptChatFile{name: entry.Name(), path: filePath, mod: mod})
	}
	return chats, evictedAge
}

// evictExcessTranscriptFiles enforces the max-chats cap by mtime LRU and
// returns the eviction count. The chats slice is sorted in place when
// eviction is needed.
func evictExcessTranscriptFiles(
	ctx context.Context,
	log *slog.Logger,
	chats []transcriptChatFile,
	maxChats int,
) int {
	if len(chats) <= maxChats {
		return 0
	}
	sort.Slice(chats, func(i, j int) bool {
		return chats[i].mod.Before(chats[j].mod)
	})
	excess := len(chats) - maxChats
	evicted := 0
	for i := range excess {
		err := os.Remove(chats[i].path)
		if err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "transcript.cleanup.remove_failed",
				slog.String("component", "transcript-cleanup"),
				slog.String("path", chats[i].path),
				slog.Any("err", err),
			)
			continue
		}
		evicted++
	}
	return evicted
}
