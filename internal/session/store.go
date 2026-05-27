package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/util"
)

// Compile-time check that FileStore implements Store.
var _ Store = (*FileStore)(nil)

const (
	metadataFile = "metadata.json"
	settingsFile = "settings.json"
)

// Store defines the interface for session storage operations.
type Store interface {
	// List returns all sessions, sorted by lastAccessed (most recent first)
	List() ([]*Session, error)

	// ListForWorkspace returns sessions whose WorkspaceRoot matches the given path,
	// sorted by lastAccessed (most recent first).
	ListForWorkspace(workspaceRoot string) ([]*Session, error)

	// Get retrieves a session by exact display name.
	Get(name string) (*Session, error)

	// Rename renames a session by updating metadata only. Storage stays keyed by
	// the durable ClydeUUID rather than the mutable exact display name.
	Rename(oldName, newName string) error

	// Search returns sessions matching query against exact display name,
	// provider session id, and context (case-insensitive substring match).
	Search(query string) ([]*Session, error)

	// Create creates a new session folder structure with metadata
	Create(session *Session) error

	// Update updates session metadata
	Update(session *Session) error

	// Delete removes a session folder and all its contents
	Delete(name string) error

	// Resolve finds a session using a multi-tier lookup:
	// 1. Exact display-name match
	// 2. Provider session id match (checks current and historical ids)
	// 3. Substring search (returns single match only)
	// 4. Transparent adoption: scans provider-owned artifacts for a session
	//    whose provider session id or provider-owned exact display name matches
	//    query, then registers a clyde session stub (metadata.json) and returns
	//    it.
	//    This tier is skipped on FileStores constructed with
	//    NewFileStoreReadOnly (for scan paths that must not recurse).
	// Returns (nil, nil) if no match is found.
	Resolve(query string) (*Session, error)

	// Exists checks if a session exists
	Exists(name string) bool

	// LoadSettings loads settings.json for a session (returns nil if not exists)
	LoadSettings(name string) (*Settings, error)

	// SaveSettings saves settings.json for a session
	SaveSettings(name string, settings *Settings) error
}

// FileStore implements Store using the filesystem.
type FileStore struct {
	clydeRoot string

	// discoveryCache memoizes provider discovery results so tier-4 Resolve misses
	// do not re-walk provider history roots on every call. nil when auto-adoption
	// is disabled (NewFileStoreReadOnly or NewFileStore).
	discoveryCache *discoveryCache

	// noAdopt disables tier 4 even when a cache is present. Used by
	// internal scan paths (prune, discovery scanner) that must not
	// trigger a recursive adopt while they are iterating transcripts.
	noAdopt bool
}

// NewFileStore creates a new FileStore without tier-4 auto-adoption.
// Used by tests and by code paths that construct a store for a
// non-global clyde root where scanning ~/.claude/projects is not
// meaningful.
func NewFileStore(clydeRoot string) *FileStore {
	return &FileStore{
		clydeRoot: clydeRoot,
	}
}

// NewGlobalFileStore creates a FileStore pointing at the global sessions directory.
// Creates the directory if it doesn't exist. The returned store has the
// tier-4 discovery cache attached so Resolve transparently adopts any provider
// session present on disk but not yet registered with clyde.
func NewGlobalFileStore() (*FileStore, error) {
	if err := config.EnsureGlobalSessionsDir(); err != nil {
		sessionLog.Warn("session.store.ensure_global_sessions_failed",
			"component", "session",
			"subcomponent", "store",
			"err", err,
		)
		return nil, fmt.Errorf("failed to create global sessions directory: %w", err)
	}
	if _, err := MigrateTitleMetadata(config.GlobalDataDir()); err != nil {
		return nil, err
	}
	fs := &FileStore{clydeRoot: config.GlobalDataDir()}
	if home, err := os.UserHomeDir(); err == nil {
		fs.discoveryCache = defaultDiscoveryCache(home)
		if _, err := MigrateTranscriptPath(config.GlobalDataDir(), home); err != nil {
			sessionLog.Warn("session.store.transcript_path_migration_failed",
				"component", "session",
				"subcomponent", "store",
				"err", err,
			)
		}
	} else {
		sessionLog.Warn("session.store.home_dir_failed",
			"component", "session",
			"subcomponent", "store",
			"err", err,
		)
	}
	return fs, nil
}

// NewGlobalFileStoreReadOnly returns a NewGlobalFileStore variant with
// tier 4 disabled. Scan and prune paths use this to avoid recursive
// adoption while they walk transcripts directly.
func NewGlobalFileStoreReadOnly() (*FileStore, error) {
	if err := config.EnsureGlobalSessionsDir(); err != nil {
		sessionLog.Warn("session.store.ensure_global_sessions_failed",
			"component", "session",
			"subcomponent", "store",
			"readonly", true,
			"err", err,
		)
		return nil, fmt.Errorf("failed to create global sessions directory: %w", err)
	}
	if _, err := MigrateTitleMetadata(config.GlobalDataDir()); err != nil {
		return nil, err
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := MigrateTranscriptPath(config.GlobalDataDir(), home); err != nil {
			sessionLog.Warn("session.store.transcript_path_migration_failed",
				"component", "session",
				"subcomponent", "store",
				"readonly", true,
				"err", err,
			)
		}
	}
	return &FileStore{clydeRoot: config.GlobalDataDir(), noAdopt: true}, nil
}

// CanonicalWorkspaceRoot normalizes a workspace path for equality checks.
// Existing directories are resolved through EvalSymlinks so clyde <dir>,
// hook-adopted metadata, and user-edited basedirs compare against the same
// stable representation. Missing paths still normalize to an absolute clean
// path so callers can handle stale metadata deterministically.
func CanonicalWorkspaceRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			switch {
			case path == "~":
				path = home
			case strings.HasPrefix(path, "~/"):
				path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
			}
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
}

// List returns all sessions, sorted by lastAccessed (most recent first).
func (fs *FileStore) List() ([]*Session, error) {
	sessions, err := fs.loadStoredSessions()
	if err != nil {
		return nil, err
	}

	sessions = dedupeSessionsByIdentity(sessions)

	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Metadata.LastAccessed.Equal(sessions[j].Metadata.LastAccessed) {
			return sessions[i].Name < sessions[j].Name
		}
		return sessions[i].Metadata.LastAccessed.After(sessions[j].Metadata.LastAccessed)
	})

	return sessions, nil
}

func dedupeSessionsByIdentity(sessions []*Session) []*Session {
	if len(sessions) <= 1 {
		return sessions
	}
	bestByKey := make(map[string]*Session, len(sessions))
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		key := IdentityKey(sess)
		existing := bestByKey[key]
		if existing == nil || PreferIdentityWinner(sess, existing) {
			bestByKey[key] = sess
		}
	}
	out := make([]*Session, 0, len(bestByKey))
	for _, sess := range bestByKey {
		out = append(out, sess)
	}
	if len(out) != len(sessions) {
		sessionResolveLog.Logger().Info("session.list.deduped",
			"component", "session",
			"subcomponent", "store",
			"sessions_before", len(sessions),
			"sessions_after", len(out),
		)
	}
	return out
}

// ListForWorkspace returns sessions whose WorkspaceRoot matches the given path,
// sorted by lastAccessed (most recent first).
func (fs *FileStore) ListForWorkspace(workspaceRoot string) ([]*Session, error) {
	all, err := fs.List()
	if err != nil {
		return nil, err
	}

	canonicalRoot := CanonicalWorkspaceRoot(workspaceRoot)
	var result []*Session
	for _, sess := range all {
		if CanonicalWorkspaceRoot(sess.Metadata.WorkspaceRoot) == canonicalRoot {
			result = append(result, sess)
		}
	}
	return result, nil
}

// Get retrieves a session by exact display name.
func (fs *FileStore) Get(name string) (*Session, error) {
	if err := ValidateDisplayName(name); err != nil {
		return nil, err
	}
	return fs.findSessionByName(name)
}

// Search returns sessions matching query against name, display title, provider
// session id, and context (case-insensitive substring match).
func (fs *FileStore) Search(query string) ([]*Session, error) {
	sessions, err := fs.List()
	if err != nil {
		return nil, err
	}
	q := normalizeSessionLookup(query)
	var matches []*Session
	for _, sess := range sessions {
		if strings.Contains(normalizeSessionLookup(sess.Name), q) ||
			strings.Contains(normalizeSessionLookup(sess.Metadata.Title), q) ||
			strings.Contains(normalizeSessionLookup(sess.Metadata.ProviderSessionID()), q) ||
			strings.Contains(normalizeSessionLookup(sess.Metadata.Context), q) {
			matches = append(matches, sess)
		}
	}
	return matches, nil
}

// Resolve finds a session using a multi-tier lookup strategy.
// Returns (nil, nil) if no match is found (not an error, caller decides behavior).
func (fs *FileStore) Resolve(query string) (*Session, error) {
	if query == "" {
		return nil, nil
	}
	if sess := fs.resolveFromStore(query); sess != nil {
		return sess, nil
	}

	if fs.noAdopt || fs.discoveryCache == nil {
		sessionResolveLog.Logger().Debug("session.resolve.miss",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"tier4", "disabled",
			"reason", adoptDisabledReason(fs),
		)
		return nil, nil
	}

	sessionResolveLog.Logger().Debug("session.resolve.tier4_started",
		"component", "session",
		"subcomponent", "resolve",
		"query", query,
	)
	adopted, err := fs.adoptFromDiscovery(query)
	if err != nil {
		sessionLog.Warn("session.resolve.tier4_failed",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"err", err,
		)
		return nil, nil
	}
	if adopted == nil {
		sessionResolveLog.Logger().Debug("session.resolve.tier4_no_match",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
		)
		return nil, nil
	}
	sessionResolveLog.Logger().Info("session.resolve.tier4_adopted",
		"component", "session",
		"subcomponent", "resolve",
		"query", query,
		"session", adopted.Name,
		"session_id", adopted.Metadata.ProviderSessionID(),
		"display_title", adopted.Metadata.Title,
		"clyde_uuid", adopted.Metadata.ClydeUUID,
	)
	if sess := fs.resolveFromStore(query); sess != nil {
		return sess, nil
	}
	return adopted, nil
}

// resolveFromStore performs tiers 1-3 without attempting adoption. The
// tier-4 path calls this both before and after adopting so the adopted
// entry is surfaced through the same lookup chain the caller would have
// hit if the session had always been registered.
func (fs *FileStore) resolveFromStore(query string) *Session {
	if sess, err := fs.Get(query); err == nil {
		sessionResolveLog.Logger().Debug("session.resolve.tier1_hit",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"session", sess.Name,
		)
		return sess
	}

	sessions, listErr := fs.List()
	if listErr == nil {
		if sess := exactTitleMatch(sessions, query); sess != nil {
			sessionResolveLog.Logger().Debug("session.resolve.tier1_display_title_hit",
				"component", "session",
				"subcomponent", "resolve",
				"query", query,
				"session", sess.Name,
				"display_title", sess.Metadata.Title,
			)
			return sess
		}
		for _, sess := range sessions {
			if matchType := exactSessionIDMatchType(sess, query); matchType != "" {
				sessionResolveLog.Logger().Debug("session.resolve.tier2_hit",
					"component", "session",
					"subcomponent", "resolve",
					"query", query,
					"session", sess.Name,
					"match", matchType,
				)
				return sess
			}
		}
	}

	matches, err := fs.Search(query)
	if err != nil || len(matches) == 0 {
		return nil
	}
	if len(matches) == 1 {
		sessionResolveLog.Logger().Debug("session.resolve.tier3_hit",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"session", matches[0].Name,
		)
		return matches[0]
	}
	sessionResolveLog.Logger().Debug("session.resolve.tier3_ambiguous",
		"component", "session",
		"subcomponent", "resolve",
		"query", query,
		"match_count", len(matches),
	)
	return nil
}

// adoptFromDiscovery runs the tier-4 scan and adoption loop. It asks
// the discovery cache for the current transcript list, looks for an
// entry whose sessionId or provider-owned exact display name matches query, runs
// AdoptUnknown to materialize a metadata.json, and invalidates the
// cache so a subsequent miss does not see the same transcript as
// unknown. Returns the adopted session, or nil when no transcript
// matches the query.
func (fs *FileStore) adoptFromDiscovery(query string) (*Session, error) {
	results, err := fs.discoveryCache.Get()
	if err != nil {
		sessionLog.Warn("session.resolve.tier4_scan_failed",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"err", err,
		)
		return nil, fmt.Errorf("discovery scan: %w", err)
	}

	var match *DiscoveryResult
	for i := range results {
		r := &results[i]
		if r.IsAutoName || r.IsSubagent || r.ProviderSessionID() == "" {
			continue
		}
		if strings.TrimSpace(r.ProviderSessionID()) == strings.TrimSpace(query) {
			if match == nil || shouldPreferDiscoveryResult(*r, *match) {
				match = r
			}
			continue
		}
		if exactDiscoveryDisplayNameMatch(*r, query) {
			if match == nil || shouldPreferDiscoveryResult(*r, *match) {
				match = r
			}
		}
	}
	if match == nil {
		sessionResolveLog.Logger().Debug("session.resolve.tier4_scan_empty",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"scanned", len(results),
		)
		return nil, nil
	}

	sessionResolveLog.Logger().Debug("session.resolve.tier4_scan_matched",
		"component", "session",
		"subcomponent", "resolve",
		"query", query,
		"session_id", match.ProviderSessionID(),
		"transcript", match.PrimaryArtifactPath(),
		"provider_name", match.GetName(),
	)

	// If the direct session ID is already registered, reconcile only the legacy
	// exact-name alias so the provider-owned title is not persisted again after
	// initial adoption.
	if existing := fs.findByProviderSessionID(match.ProviderIdentity()); existing != nil {
		return fs.reconcileExisting(existing, match, query)
	}

	adopted, adoptErr := AdoptUnknown(fs, []DiscoveryResult{*match})
	fs.discoveryCache.Invalidate()
	if adoptErr != nil {
		sessionLog.Warn("session.resolve.tier4_adopt_failed",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"session_id", match.ProviderSessionID(),
			"err", adoptErr,
		)
		return nil, fmt.Errorf("adopt: %w", adoptErr)
	}
	if len(adopted) == 0 {
		sessionResolveLog.Logger().Debug("session.resolve.tier4_adopt_skipped",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"session_id", match.ProviderSessionID(),
		)
		if sess := fs.resolveFromStore(query); sess != nil {
			return sess, nil
		}
		return nil, nil
	}

	stub := adopted[0]
	return &Session{Name: stub.Name, Metadata: stub.Metadata, storageKey: stub.Metadata.ClydeUUID}, nil
}

// adoptDisabledReason labels why tier 4 was skipped, for structured
// logs. Keeps the skip branch in Resolve a single slog call.
func adoptDisabledReason(fs *FileStore) string {
	if fs.noAdopt {
		return "read_only_store"
	}
	if fs.discoveryCache == nil {
		return "no_discovery_cache"
	}
	return "unknown"
}

func exactTitleMatch(sessions []*Session, query string) *Session {
	trimmedQuery := strings.TrimSpace(query)
	if trimmedQuery == "" {
		return nil
	}

	var match *Session
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		if strings.TrimSpace(sess.Metadata.Title) != trimmedQuery {
			continue
		}
		if match != nil {
			sessionResolveLog.Logger().Debug("session.resolve.tier1_display_title_ambiguous",
				"component", "session",
				"subcomponent", "resolve",
				"query", query,
				"first_session", match.Name,
				"next_session", sess.Name,
			)
			return nil
		}
		match = sess
	}
	return match
}

func exactDiscoveryDisplayNameMatch(result DiscoveryResult, query string) bool {
	displayName := strings.TrimSpace(result.Title())
	if displayName == "" {
		return false
	}
	return displayName == strings.TrimSpace(query)
}

func normalizeSessionLookup(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// findByProviderSessionID returns the first registered session whose current or
// historical provider identity matches id, or nil when none match. Used by tier
// 4 to detect a session that was previously auto-adopted under a name that does
// not reflect the provider-owned observed name.
func (fs *FileStore) findByProviderSessionID(id ProviderSessionID) *Session {
	id = id.Normalized()
	if id.IsZero() {
		return nil
	}
	sessions, err := fs.List()
	if err != nil {
		sessionLog.Warn("session.resolve.find_by_provider_identity_list_failed",
			"component", "session",
			"subcomponent", "resolve",
			"provider", id.Provider,
			"session_id", id.ID,
			"err", err,
		)
		return nil
	}
	for _, sess := range sessions {
		for _, identity := range HistoricalIdentities(sess) {
			if identity.Normalized() == id {
				return sess
			}
		}
	}
	return nil
}

func exactSessionIDMatchType(sess *Session, query string) string {
	query = strings.TrimSpace(query)
	if query == "" || sess == nil {
		return ""
	}
	if !MatchesAnySessionID(sess, query) {
		return ""
	}
	if current, ok := CurrentIdentity(sess); ok && current.ID == query {
		return "current_session_id"
	}
	for _, identity := range HistoricalIdentities(sess) {
		if identity.ID == query && identity.ID != strings.TrimSpace(sess.Metadata.ProviderSessionID()) {
			return "previous_session_id"
		}
	}
	return ""
}

// reconcileExisting updates an already-adopted session's legacy exact-name
// alias without touching the Clyde-owned persisted title.
func (fs *FileStore) reconcileExisting(existing *Session, match *DiscoveryResult, query string) (*Session, error) {
	observedName := match.Title()

	names, err := buildExistingNameSet(fs)
	if err != nil {
		sessionLog.Warn("session.resolve.reconcile_name_set_failed",
			"component", "session",
			"subcomponent", "resolve",
			"err", err,
		)
		return existing, nil
	}
	delete(names, existing.Name)

	target := UniqueDisplayName(observedName, names)
	if target == "" || target == existing.Name {
		sessionResolveLog.Logger().Debug("session.resolve.reconcile_no_rename",
			"component", "session",
			"subcomponent", "resolve",
			"query", query,
			"session", existing.Name,
			"provider_name", observedName,
			"reason_empty", target == "",
		)
		return existing, nil
	}

	oldName := existing.Name
	existing.Name = target
	existing.Metadata.Name = target
	if err := fs.Update(existing); err != nil {
		if resolved := fs.findByProviderSessionID(match.ProviderIdentity()); resolved != nil {
			if resolved.Name == target {
				return resolved, nil
			}
		}
		sessionLog.Warn("session.resolve.reconcile_rename_failed",
			"component", "session",
			"subcomponent", "resolve",
			"old_name", oldName,
			"new_name", target,
			"session_id", existing.Metadata.ProviderSessionID(),
			"err", err,
		)
		existing.Name = oldName
		existing.Metadata.Name = oldName
		return existing, nil
	}
	sessionResolveLog.Logger().Info("session.resolve.reconcile_renamed",
		"component", "session",
		"subcomponent", "resolve",
		"old_name", oldName,
		"new_name", target,
		"session_id", existing.Metadata.ProviderSessionID(),
		"display_title", observedName,
		"query", query,
	)
	if renamed, getErr := fs.Get(target); getErr == nil {
		return renamed, nil
	}
	return existing, nil
}

func shouldPreferDiscoveryResult(candidate DiscoveryResult, current DiscoveryResult) bool {
	if candidate.GetName() != "" && current.GetName() == "" {
		return true
	}
	if current.GetName() != "" && candidate.GetName() == "" {
		return false
	}
	if !candidate.FirstEntryTime.Equal(current.FirstEntryTime) {
		return candidate.FirstEntryTime.After(current.FirstEntryTime)
	}
	if candidate.WorkspaceRoot != "" && current.WorkspaceRoot == "" {
		return true
	}
	if current.WorkspaceRoot != "" && candidate.WorkspaceRoot == "" {
		return false
	}
	if candidate.PrimaryArtifactPath() != current.PrimaryArtifactPath() {
		return candidate.PrimaryArtifactPath() < current.PrimaryArtifactPath()
	}
	return candidate.ProviderSessionID() < current.ProviderSessionID()
}

// DiscoverySyncResult describes the effect of reconciling one discovered
// provider session against the Clyde registry.
type DiscoverySyncResult struct {
	Session *Session
	OldName string
	Adopted bool
	Updated bool
}

// SyncDiscoveryResults reconciles discovered provider sessions against the
// registry, adopting unknown rows and updating known rows when provider-owned
// names drift.
func (fs *FileStore) SyncDiscoveryResults(results []DiscoveryResult) ([]DiscoverySyncResult, error) {
	if fs == nil {
		return nil, nil
	}
	bestByProviderKey := make(map[string]DiscoveryResult, len(results))
	for _, result := range results {
		if result.IsAutoName || result.IsSubagent || result.ProviderSessionID() == "" || isClydeScratch(result.WorkspaceRoot) {
			continue
		}
		key := result.ProviderSessionKey()
		current, ok := bestByProviderKey[key]
		if !ok || shouldPreferDiscoveryResult(result, current) {
			bestByProviderKey[key] = result
		}
	}

	keys := make([]string, 0, len(bestByProviderKey))
	for key := range bestByProviderKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]DiscoverySyncResult, 0, len(keys))
	for _, key := range keys {
		result := bestByProviderKey[key]
		if existing := fs.findByProviderSessionID(result.ProviderIdentity()); existing != nil {
			beforeName := existing.Name
			updated, err := fs.reconcileExisting(existing, &result, result.ProviderSessionID())
			if err != nil {
				return nil, err
			}
			if updated != nil && updated.Name != beforeName {
				out = append(out, DiscoverySyncResult{
					Session: updated,
					OldName: beforeName,
					Adopted: false,
					Updated: true,
				})
			}
			continue
		}

		adopted, err := AdoptUnknown(fs, []DiscoveryResult{result})
		if err != nil {
			return nil, err
		}
		if len(adopted) == 0 {
			continue
		}
		stub := adopted[0]
		out = append(out, DiscoverySyncResult{
			Session: &Session{Name: stub.Name, Metadata: stub.Metadata, storageKey: stub.Metadata.ClydeUUID},
			OldName: "",
			Adopted: true,
			Updated: true,
		})
	}
	return out, nil
}

// Rename renames a session by updating exact display-name metadata only.
func (fs *FileStore) Rename(oldName, newName string) error {
	if err := ValidateDisplayName(oldName); err != nil {
		sessionLog.Warn("session.store.rename_invalid_old_name",
			"component", "session",
			"subcomponent", "store",
			"session", oldName,
			"err", err,
		)
		return fmt.Errorf("invalid old name: %w", err)
	}
	if err := ValidateDisplayName(newName); err != nil {
		sessionLog.Warn("session.store.rename_invalid_new_name",
			"component", "session",
			"subcomponent", "store",
			"session", newName,
			"err", err,
		)
		return fmt.Errorf("invalid new name: %w", err)
	}

	sess, err := fs.Get(oldName)
	if err != nil {
		return fmt.Errorf("session '%s' not found", oldName)
	}
	if fs.Exists(newName) {
		return fmt.Errorf("session '%s' already exists", newName)
	}

	sess.Name = newName
	sess.Metadata.Name = newName
	sess.Metadata.Title = newName
	if err := fs.Update(sess); err != nil {
		sessionLog.Warn("session.store.rename_metadata_update_failed",
			"component", "session",
			"subcomponent", "store",
			"old_session", oldName,
			"new_session", newName,
			"err", err,
		)
		return fmt.Errorf("failed to update session metadata: %w", err)
	}
	return nil
}

// Create creates a new session folder structure with metadata.
func (fs *FileStore) Create(session *Session) error {
	if session == nil {
		return fmt.Errorf("nil session")
	}
	if err := ValidateDisplayName(session.Name); err != nil {
		return err
	}
	if fs.Exists(session.Name) {
		return fmt.Errorf("session '%s' already exists", session.Name)
	}

	session.Metadata.Name = session.Name
	dirKey := session.ensureClydeUUID("")
	sessionDir := config.GetSessionDir(fs.clydeRoot, dirKey)
	if util.DirExists(sessionDir) {
		return fmt.Errorf("session storage '%s' already exists", dirKey)
	}
	if err := util.EnsureDir(sessionDir); err != nil {
		sessionLog.Warn("session.store.create_dir_failed",
			"component", "session",
			"subcomponent", "store",
			"session", session.Name,
			"path", sessionDir,
			"err", err,
		)
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	metadataPath := filepath.Join(sessionDir, metadataFile)
	session.Metadata.NormalizeProviderState()
	if err := util.WriteJSON(metadataPath, session.Metadata); err != nil {
		sessionLog.Warn("session.store.create_metadata_failed",
			"component", "session",
			"subcomponent", "store",
			"session", session.Name,
			"path", metadataPath,
			"err", err,
		)
		return fmt.Errorf("failed to write session metadata: %w", err)
	}
	session.storageKey = dirKey
	return nil
}

// Update updates session metadata.
func (fs *FileStore) Update(session *Session) error {
	if session == nil {
		return fmt.Errorf("nil session")
	}
	if err := ValidateDisplayName(session.Name); err != nil {
		return err
	}

	session.Metadata.Name = session.Name
	currentKey := strings.TrimSpace(session.storageKey)
	if currentKey == "" {
		existing, err := fs.findSessionByName(session.Name)
		if err != nil {
			return fmt.Errorf("session '%s' not found", session.Name)
		}
		currentKey = existing.StorageKey()
	}
	targetKey := session.ensureClydeUUID(currentKey)
	if currentKey == "" {
		currentKey = targetKey
	}

	currentDir := config.GetSessionDir(fs.clydeRoot, currentKey)
	targetDir := config.GetSessionDir(fs.clydeRoot, targetKey)
	if !util.DirExists(currentDir) {
		return fmt.Errorf("session '%s' not found", session.Name)
	}
	if currentKey != targetKey {
		if util.DirExists(targetDir) {
			return fmt.Errorf("session storage '%s' already exists", targetKey)
		}
		if err := os.Rename(currentDir, targetDir); err != nil {
			sessionLog.Warn("session.store.migrate_dir_failed",
				"component", "session",
				"subcomponent", "store",
				"session", session.Name,
				"old_dir", currentDir,
				"new_dir", targetDir,
				"err", err,
			)
			return fmt.Errorf("migrate session directory: %w", err)
		}
	}

	metadataPath := filepath.Join(targetDir, metadataFile)
	session.Metadata.NormalizeProviderState()
	if err := util.WriteJSON(metadataPath, session.Metadata); err != nil {
		sessionLog.Warn("session.store.update_metadata_failed",
			"component", "session",
			"subcomponent", "store",
			"session", session.Name,
			"path", metadataPath,
			"err", err,
		)
		return fmt.Errorf("failed to update session metadata: %w", err)
	}

	session.storageKey = targetKey
	return nil
}

// Delete removes a session folder and all its contents.
func (fs *FileStore) Delete(name string) error {
	if err := ValidateDisplayName(name); err != nil {
		return err
	}

	sess, err := fs.findSessionByName(name)
	if err != nil {
		return fmt.Errorf("session '%s' not found", name)
	}
	sessionDir := config.GetSessionDir(fs.clydeRoot, sess.StorageKey())
	return util.RemoveAll(sessionDir)
}

// Exists checks if a session exists.
func (fs *FileStore) Exists(name string) bool {
	if name == "" {
		return false
	}
	_, err := fs.findSessionByName(name)
	return err == nil
}

// LoadSettings loads settings.json for a session (returns nil if not exists).
func (fs *FileStore) LoadSettings(name string) (*Settings, error) {
	if err := ValidateDisplayName(name); err != nil {
		return nil, err
	}

	sess, err := fs.findSessionByName(name)
	if err != nil {
		return nil, fmt.Errorf("session '%s' not found", name)
	}
	settingsPath := filepath.Join(config.GetSessionDir(fs.clydeRoot, sess.StorageKey()), settingsFile)
	if !util.FileExists(settingsPath) {
		return nil, nil
	}

	var settings Settings
	if err := util.ReadJSON(settingsPath, &settings); err != nil {
		sessionLog.Warn("session.store.settings_read_failed",
			"component", "session",
			"subcomponent", "store",
			"session", name,
			"path", settingsPath,
			"err", err,
		)
		return nil, fmt.Errorf("failed to read settings: %w", err)
	}

	return &settings, nil
}

// SaveSettings saves settings.json for a session.
func (fs *FileStore) SaveSettings(name string, settings *Settings) error {
	if err := ValidateDisplayName(name); err != nil {
		return err
	}

	sess, err := fs.findSessionByName(name)
	if err != nil {
		return fmt.Errorf("session '%s' not found", name)
	}
	settingsPath := filepath.Join(config.GetSessionDir(fs.clydeRoot, sess.StorageKey()), settingsFile)
	return util.WriteJSON(settingsPath, settings)
}

func (fs *FileStore) loadStoredSessions() ([]*Session, error) {
	sessionsDir := config.GetSessionsDir(fs.clydeRoot)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Session{}, nil
		}
		return nil, err
	}

	sessions := make([]*Session, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sess, err := fs.readSessionFromDir(entry.Name())
		if err != nil {
			continue
		}
		sessions = append(sessions, sess)
	}
	hydrateParentSessionNames(sessions)
	return sessions, nil
}

func (fs *FileStore) readSessionFromDir(storageKey string) (*Session, error) {
	sessionDir := config.GetSessionDir(fs.clydeRoot, storageKey)
	metadataPath := filepath.Join(sessionDir, metadataFile)

	var metadata Metadata
	if err := util.ReadJSON(metadataPath, &metadata); err != nil {
		sessionLog.Warn("session.store.metadata_read_failed",
			"component", "session",
			"subcomponent", "store",
			"storage_key", storageKey,
			"path", metadataPath,
			"err", err,
		)
		return nil, fmt.Errorf("failed to read session metadata: %w", err)
	}

	metadata.NormalizeProviderState()
	name := strings.TrimSpace(metadata.Name)
	if name == "" {
		name = storageKey
	}
	sess := &Session{
		Name:       name,
		Metadata:   metadata,
		storageKey: storageKey,
	}
	if looksLikeUUID(storageKey) && strings.TrimSpace(sess.Metadata.ClydeUUID) == "" {
		sess.Metadata.ClydeUUID = storageKey
	}
	sess.Metadata.Name = sess.Name
	return sess, nil
}

func (fs *FileStore) findSessionByName(name string) (*Session, error) {
	sessions, err := fs.loadStoredSessions()
	if err != nil {
		return nil, err
	}
	for _, sess := range sessions {
		if sess.Name == name {
			return sess, nil
		}
	}
	return nil, fmt.Errorf("session '%s' not found", name)
}

func hydrateParentSessionNames(sessions []*Session) {
	if len(sessions) == 0 {
		return
	}
	namesByUUID := make(map[string]string, len(sessions))
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		if uuidValue := strings.TrimSpace(sess.ClydeUUID()); uuidValue != "" {
			namesByUUID[uuidValue] = sess.Name
		}
	}
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		parentUUID := strings.TrimSpace(sess.Metadata.ParentClydeUUID)
		if parentUUID == "" {
			continue
		}
		if parentName, ok := namesByUUID[parentUUID]; ok {
			sess.Metadata.ParentSession = parentName
		}
	}
}
