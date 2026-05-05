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
// per-chat transcript root and evicts directories that exceed the configured
// max-age and max-chat retention. Disabled when the transcript feature is
// off.
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
	type chatDir struct {
		name      string
		path      string
		newestMod time.Time
	}
	chats := make([]chatDir, 0, len(entries))
	cutoff := now().Add(-maxAge)
	evictedAge := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirPath := filepath.Join(root, entry.Name())
		newest := newestFileMtime(dirPath)
		if !newest.IsZero() && newest.Before(cutoff) {
			if err := os.RemoveAll(dirPath); err != nil {
				log.LogAttrs(ctx, slog.LevelWarn, "transcript.cleanup.remove_failed",
					slog.String("component", "transcript-cleanup"),
					slog.String("path", dirPath),
					slog.Any("err", err),
				)
				continue
			}
			evictedAge++
			continue
		}
		chats = append(chats, chatDir{name: entry.Name(), path: dirPath, newestMod: newest})
	}
	evictedCount := 0
	if len(chats) > maxChats {
		sort.Slice(chats, func(i, j int) bool {
			return chats[i].newestMod.Before(chats[j].newestMod)
		})
		excess := len(chats) - maxChats
		for i := 0; i < excess; i++ {
			if err := os.RemoveAll(chats[i].path); err != nil {
				log.LogAttrs(ctx, slog.LevelWarn, "transcript.cleanup.remove_failed",
					slog.String("component", "transcript-cleanup"),
					slog.String("path", chats[i].path),
					slog.Any("err", err),
				)
				continue
			}
			evictedCount++
		}
	}
	log.LogAttrs(ctx, slog.LevelInfo, "transcript.cleanup.tick_completed",
		slog.String("component", "transcript-cleanup"),
		slog.Int("evicted_age", evictedAge),
		slog.Int("evicted_count", evictedCount),
		slog.Int("retained", len(chats)-evictedCount),
	)
}

func newestFileMtime(dir string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}
