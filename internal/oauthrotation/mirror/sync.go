// Package mirror performs one-way import of a provider's upstream OAuth
// credentials into Clyde's per-account store. It reads the provider's declared
// MirrorSources, asks the provider for each token's account identity, and writes
// Clyde's per-account copy when the upstream copy is missing or fresher. It
// never writes back to any upstream source. It depends only on the provider
// contract and on injected path resolution; it imports no provider package and
// no parent rotation package, which keeps the import graph acyclic.
package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// Paths resolves on-disk locations for one account's mirrored copy. The
// rotation layer supplies these from its store_paths helpers so the mirror
// package stays free of any state-dir knowledge.
type Paths struct {
	// AccountDir returns the directory holding one account's files.
	AccountDir func(account provider.AccountID) string
	// CredentialsFile returns the per-account credentials file path.
	CredentialsFile func(account provider.AccountID) string
	// LabelFile returns the per-account label file path. The syncer writes a
	// provider-derived label here only when the file does not already exist, so
	// an operator-supplied label set at login time is never overwritten by a
	// harvest pass.
	LabelFile func(account provider.AccountID) string
}

// Modes carries the permissions the syncer applies when it creates account
// directories and credential files.
type Modes struct {
	FileMode os.FileMode
	DirMode  os.FileMode
}

// Syncer imports upstream credentials one-way into the per-account store. It
// keeps every account it has ever seen by never deleting an existing account
// directory; a keychain that now holds a different account adds a new entry
// rather than replacing the old one.
type Syncer struct {
	prov   provider.Provider
	paths  Paths
	modes  Modes
	logger *slog.Logger
}

// NewSyncer builds a Syncer for one registered provider. A nil logger falls
// back to [slog.Default].
func NewSyncer(prov provider.Provider, paths Paths, modes Modes, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{prov: prov, paths: paths, modes: modes, logger: logger}
}

// SyncResult reports what one Sync pass did, for logging by the caller.
// SourcesRead is the number of upstream MirrorSources the provider declared on
// this pass, regardless of whether each source was present or yielded an import.
// It lets the rotator surface the upstream-pass cost in its startup-load event.
type SyncResult struct {
	Imported    []provider.AccountID
	Skipped     []provider.AccountID
	SourcesRead int
}

// Sync runs one import pass. For each upstream MirrorSource it reads the raw
// credential, parses it, derives the account, and writes Clyde's copy when the
// account has no stored copy or when the upstream copy is fresher. now controls
// the comparison clock for testing.
func (s *Syncer) Sync(ctx context.Context, now time.Time) (SyncResult, error) {
	sources, err := s.prov.MirrorSources(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.sources_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"err", err.Error(),
		)
		return SyncResult{Imported: nil, Skipped: nil, SourcesRead: 0}, fmt.Errorf("list mirror sources for %q: %w", s.prov.Name(), err)
	}
	result := SyncResult{Imported: nil, Skipped: nil, SourcesRead: len(sources)}
	for _, source := range sources {
		upstream, identity, present, readErr := s.readSource(ctx, source)
		if readErr != nil {
			return result, readErr
		}
		if !present {
			continue
		}
		s.writeDerivedLabel(ctx, identity)
		wrote, writeErr := s.importOne(ctx, identity.Account, upstream)
		if writeErr != nil {
			return result, writeErr
		}
		if wrote {
			result.Imported = append(result.Imported, identity.Account)
		} else {
			result.Skipped = append(result.Skipped, identity.Account)
		}
	}
	return result, nil
}

// readSource reads one upstream source through the provider and resolves its
// account identity. The provider owns how each source Kind is read, so the
// mirror never reads a file or shells a keychain itself. A source that is not
// present (missing file, empty keychain item) returns present=false with no
// error so the caller skips it.
func (s *Syncer) readSource(ctx context.Context, source provider.MirrorSource) (provider.Credentials, provider.AccountIdentity, bool, error) {
	empty := provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}
	emptyIdentity := provider.AccountIdentity{Account: "", Label: ""}
	upstream, present, err := s.prov.ReadMirror(ctx, source)
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.source_read_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"kind", source.Kind,
			"location", source.Location,
			"err", err.Error(),
		)
		return empty, emptyIdentity, false, fmt.Errorf("read mirror source %q at %s: %w", source.Kind, source.Location, err)
	}
	if !present {
		return empty, emptyIdentity, false, nil
	}
	identity, err := s.prov.AccountIdentity(ctx, upstream.AccessToken)
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.source_account_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"location", source.Location,
			"err", err.Error(),
		)
		return empty, emptyIdentity, false, fmt.Errorf("derive account for mirror source %s: %w", source.Location, err)
	}
	return upstream, identity, true, nil
}

// writeDerivedLabel writes the provider-derived label (an email or display
// name) into the per-account label file, but only when the identity carries a
// label and no label file already exists. The existing-file guard means an
// operator label recorded at login time wins over the harvested one, and a
// later harvest pass does not rewrite an unchanged label. A write failure is
// logged and swallowed so a label problem never blocks credential import. It is
// a no-op when Paths.LabelFile is nil.
func (s *Syncer) writeDerivedLabel(ctx context.Context, identity provider.AccountIdentity) {
	if s.paths.LabelFile == nil || identity.Account == "" || identity.Label == "" {
		return
	}
	labelPath := s.paths.LabelFile(identity.Account)
	if _, err := os.Stat(labelPath); err == nil {
		return
	} else if !os.IsNotExist(err) {
		s.logger.WarnContext(ctx, "oauthrotation.mirror.label_stat_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(identity.Account),
			"path", labelPath,
			"err", err.Error(),
		)
		return
	}
	dir := s.paths.AccountDir(identity.Account)
	if err := os.MkdirAll(dir, s.modes.DirMode); err != nil {
		s.logger.WarnContext(ctx, "oauthrotation.mirror.label_mkdir_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(identity.Account),
			"dir", dir,
			"err", err.Error(),
		)
		return
	}
	if err := os.WriteFile(labelPath, []byte(identity.Label), s.modes.FileMode); err != nil {
		s.logger.WarnContext(ctx, "oauthrotation.mirror.label_write_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(identity.Account),
			"path", labelPath,
			"err", err.Error(),
		)
		return
	}
	s.logger.InfoContext(ctx, "oauthrotation.mirror.label_derived",
		"component", "oauthrotation",
		"subcomponent", "mirror",
		"provider", string(s.prov.Name()),
		"account", string(identity.Account),
		"label", identity.Label,
	)
}

// importOne writes the upstream credential into the per-account store when no
// stored copy exists or the stored copy is staler. It returns whether it wrote.
func (s *Syncer) importOne(ctx context.Context, account provider.AccountID, upstream provider.Credentials) (bool, error) {
	credPath := s.paths.CredentialsFile(account)
	stored, found, err := s.readStored(ctx, credPath)
	if err != nil {
		return false, err
	}
	if found && !isFresher(upstream, stored) {
		return false, nil
	}
	if err := s.writeStored(ctx, account, upstream); err != nil {
		return false, err
	}
	return true, nil
}

// readStored loads the existing per-account copy if present.
func (s *Syncer) readStored(ctx context.Context, credPath string) (provider.Credentials, bool, error) {
	empty := provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}
	raw, err := os.ReadFile(credPath)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, false, nil
		}
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.stored_read_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"path", credPath,
			"err", err.Error(),
		)
		return empty, false, fmt.Errorf("read stored credential %s: %w", credPath, err)
	}
	stored, err := s.prov.ParseStored(raw)
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.stored_parse_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"path", credPath,
			"err", err.Error(),
		)
		return empty, false, fmt.Errorf("parse stored credential %s: %w", credPath, err)
	}
	return stored, true, nil
}

// writeStored encodes and writes the credential into the per-account file. It
// emits a structured event on success so the state-mutation boundary is
// observable.
func (s *Syncer) writeStored(ctx context.Context, account provider.AccountID, cred provider.Credentials) error {
	encoded, err := s.prov.EncodeStored(cred)
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.encode_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(account),
			"err", err.Error(),
		)
		return fmt.Errorf("encode credential for account %q: %w", account, err)
	}
	dir := s.paths.AccountDir(account)
	if err := os.MkdirAll(dir, s.modes.DirMode); err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.mkdir_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(account),
			"dir", dir,
			"err", err.Error(),
		)
		return fmt.Errorf("mkdir account dir %s: %w", dir, err)
	}
	credPath := s.paths.CredentialsFile(account)
	tmp, err := os.CreateTemp(dir, filepath.Base(credPath)+".tmp-*")
	if err != nil {
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.temp_create_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(account),
			"dir", dir,
			"err", err.Error(),
		)
		return fmt.Errorf("create temp credential in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.temp_write_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(account),
			"path", tmpName,
			"err", err.Error(),
		)
		return fmt.Errorf("write temp credential %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(s.modes.FileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.temp_chmod_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(account),
			"path", tmpName,
			"err", err.Error(),
		)
		return fmt.Errorf("chmod temp credential %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.temp_close_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(account),
			"path", tmpName,
			"err", err.Error(),
		)
		return fmt.Errorf("close temp credential %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, credPath); err != nil {
		_ = os.Remove(tmpName)
		s.logger.ErrorContext(ctx, "oauthrotation.mirror.rename_failed",
			"component", "oauthrotation",
			"subcomponent", "mirror",
			"provider", string(s.prov.Name()),
			"account", string(account),
			"path", credPath,
			"err", err.Error(),
		)
		return fmt.Errorf("rename credential into place %s: %w", credPath, err)
	}
	s.logger.InfoContext(ctx, "oauthrotation.mirror.imported",
		"component", "oauthrotation",
		"subcomponent", "mirror",
		"provider", string(s.prov.Name()),
		"account", string(account),
		"expires_at_ms", cred.ExpiresAt.UnixMilli(),
		"fingerprint", cred.Fingerprint,
	)
	return nil
}

// isFresher reports whether the upstream credential should replace the stored
// one. A later ExpiresAt is fresher; on an equal expiry a differing
// Fingerprint is treated as fresher so a rotated token of the same lifetime
// still imports.
func isFresher(upstream, stored provider.Credentials) bool {
	if upstream.ExpiresAt.After(stored.ExpiresAt) {
		return true
	}
	if upstream.ExpiresAt.Equal(stored.ExpiresAt) {
		return upstream.Fingerprint != stored.Fingerprint
	}
	return false
}
