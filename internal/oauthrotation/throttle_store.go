package oauthrotation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

const (
	// throttleSchemaVersion is the on-disk schema version for the ledger.
	throttleSchemaVersion = 1
	// throttleLockTimeout bounds how long a writer waits for the file lock.
	throttleLockTimeout = 10 * time.Second
	// throttleLockRetry is the poll interval while waiting for the lock.
	throttleLockRetry = 100 * time.Millisecond
)

// throttleFile is the on-disk schema for one provider's throttle ledger.
type throttleFile struct {
	Version int                                  `json:"version"`
	Entries map[provider.AccountID]throttleEntry `json:"entries"`
}

// throttleEntry records one account's active throttle. UntilMS and ObservedMS
// are Unix milliseconds; Claim is the rate-limit window string; HTTPStatus is
// the upstream status that produced the throttle.
type throttleEntry struct {
	UntilMS    int64  `json:"until_ms"`
	ObservedMS int64  `json:"observed_ms"`
	Claim      string `json:"claim"`
	HTTPStatus int    `json:"http_status"`
}

// throttleStore is the flock'd JSON persister for one provider's throttle
// ledger. One file per provider lives at throttlePath(provider).
type throttleStore struct {
	provider provider.Name
	logger   *slog.Logger
}

// newThrottleStore returns a throttle store for one provider. A nil logger
// falls back to [slog.Default].
func newThrottleStore(name provider.Name, logger *slog.Logger) *throttleStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &throttleStore{provider: name, logger: logger}
}

// load reads the ledger and drops every entry whose UntilMS is at or before
// now. A missing file is treated as an empty ledger. now is passed in so
// callers control the clock for testing.
func (s *throttleStore) load(ctx context.Context, now time.Time) (map[provider.AccountID]throttleEntry, error) {
	release, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.readActiveLocked(ctx, now)
}

// save merges the provided active entries into the ledger, drops expired
// entries (by now), and writes the result atomically. now controls the clock.
func (s *throttleStore) save(ctx context.Context, now time.Time, updates map[provider.AccountID]throttleEntry) error {
	release, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer release()

	current, err := s.readActiveLocked(ctx, now)
	if err != nil {
		return err
	}
	maps.Copy(current, updates)
	pruned := dropExpired(current, now)
	return s.writeLocked(ctx, pruned)
}

// put records or replaces a single account's throttle entry.
func (s *throttleStore) put(ctx context.Context, now time.Time, account provider.AccountID, entry throttleEntry) error {
	return s.save(ctx, now, map[provider.AccountID]throttleEntry{account: entry})
}

// clear removes one account's throttle entry from the shared per-provider
// ledger and rewrites the file. It is called when an account is forgotten so a
// stale entry does not linger in the ledger after the account directory is
// deleted. A missing ledger is a no-op.
func (s *throttleStore) clear(ctx context.Context, account provider.AccountID) error {
	release, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer release()

	path := throttlePath(s.provider)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.clear_read_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", path,
			"err", err.Error(),
		)
		return fmt.Errorf("read throttle ledger %s: %w", path, err)
	}
	var file throttleFile
	if err := json.Unmarshal(data, &file); err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.clear_decode_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", path,
			"err", err.Error(),
		)
		return fmt.Errorf("decode throttle ledger %s: %w", path, err)
	}
	if file.Entries == nil {
		return nil
	}
	if _, present := file.Entries[account]; !present {
		return nil
	}
	delete(file.Entries, account)
	return s.writeLocked(ctx, file.Entries)
}

// readActiveLocked reads the file and returns only non-expired entries. The
// caller must hold the lock.
func (s *throttleStore) readActiveLocked(ctx context.Context, now time.Time) (map[provider.AccountID]throttleEntry, error) {
	path := throttlePath(s.provider)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[provider.AccountID]throttleEntry{}, nil
		}
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.read_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", path,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("read throttle ledger %s: %w", path, err)
	}
	var file throttleFile
	if err := json.Unmarshal(data, &file); err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.decode_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", path,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("decode throttle ledger %s: %w", path, err)
	}
	if file.Entries == nil {
		return map[provider.AccountID]throttleEntry{}, nil
	}
	return dropExpired(file.Entries, now), nil
}

// writeLocked atomically writes the ledger via a temp file and rename. The
// caller must hold the lock. It emits a structured event on success so the
// state-mutation boundary is observable.
func (s *throttleStore) writeLocked(ctx context.Context, entries map[provider.AccountID]throttleEntry) error {
	path := throttlePath(s.provider)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.mkdir_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"dir", dir,
			"err", err.Error(),
		)
		return fmt.Errorf("mkdir throttle dir %s: %w", dir, err)
	}
	file := throttleFile{Version: throttleSchemaVersion, Entries: entries}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.encode_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"err", err.Error(),
		)
		return fmt.Errorf("encode throttle ledger: %w", err)
	}
	tmp, err := os.CreateTemp(dir, throttleFileName+".tmp-*")
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.temp_create_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"dir", dir,
			"err", err.Error(),
		)
		return fmt.Errorf("create temp throttle ledger in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.temp_write_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", tmpName,
			"err", err.Error(),
		)
		return fmt.Errorf("write temp throttle ledger %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(credentialFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.temp_chmod_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", tmpName,
			"err", err.Error(),
		)
		return fmt.Errorf("chmod temp throttle ledger %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.temp_close_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", tmpName,
			"err", err.Error(),
		)
		return fmt.Errorf("close temp throttle ledger %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.rename_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"path", path,
			"err", err.Error(),
		)
		return fmt.Errorf("rename throttle ledger into place %s: %w", path, err)
	}
	s.logger.DebugContext(ctx, "oauthrotation.throttle.written",
		"component", "oauthrotation",
		"subcomponent", "throttle_store",
		"provider", string(s.provider),
		"path", path,
		"count", len(entries),
	)
	return nil
}

// lock acquires the per-provider file lock and returns a release function. The
// lock file sits beside the ledger so it shares the provider directory.
func (s *throttleStore) lock(ctx context.Context) (func(), error) {
	dir := providerDir(s.provider)
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.throttle.lock_mkdir_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"dir", dir,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("mkdir provider dir %s: %w", dir, err)
	}
	lockPath := throttlePath(s.provider) + ".lock"
	lock := flock.New(lockPath)
	lockCtx, cancel := context.WithTimeout(ctx, throttleLockTimeout)
	got, err := lock.TryLockContext(lockCtx, throttleLockRetry)
	if err != nil {
		cancel()
		s.logger.WarnContext(ctx, "oauthrotation.throttle.lock_failed",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"lock_path", lockPath,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("acquire throttle lock %s: %w", lockPath, err)
	}
	if !got {
		cancel()
		s.logger.WarnContext(ctx, "oauthrotation.throttle.lock_timeout",
			"component", "oauthrotation",
			"subcomponent", "throttle_store",
			"provider", string(s.provider),
			"lock_path", lockPath,
		)
		return nil, fmt.Errorf("acquire throttle lock %s: timed out", lockPath)
	}
	return func() {
		_ = lock.Unlock()
		cancel()
	}, nil
}

// dropExpired returns a new map containing only entries whose UntilMS is
// strictly after now.
func dropExpired(entries map[provider.AccountID]throttleEntry, now time.Time) map[provider.AccountID]throttleEntry {
	nowMS := now.UnixMilli()
	active := make(map[provider.AccountID]throttleEntry, len(entries))
	for account, entry := range entries {
		if entry.UntilMS > nowMS {
			active[account] = entry
		}
	}
	return active
}
