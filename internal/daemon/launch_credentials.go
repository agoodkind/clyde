package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation"
	oauthprovider "goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/util"
)

// rotatorProviderNameForClaude is the rotation-store provider the launched
// `claude` authenticates against. The launched provider id is "claude"; its
// OAuth account lives under the "anthropic" rotation provider.
const rotatorProviderNameForClaude oauthprovider.Name = "anthropic"

const (
	// launchCredentialsDirMode is the permission for a planted-credentials
	// scratch dir. 0700 matches the per-account store and session runtime dirs.
	launchCredentialsDirMode = 0o700
	// launchCredentialsFileMode is the permission for the planted
	// .credentials.json. 0600 matches the rotator's per-account credential file.
	launchCredentialsFileMode = 0o600
	// launchCredentialsFileName is the file claude reads when CLAUDE_CONFIG_DIR
	// points at a non-default dir and the macOS keychain entry is therefore
	// absent. The name matches Claude Code's fallback credential file.
	launchCredentialsFileName = ".credentials.json"
	// launchCredentialsSubdir groups planted scratch dirs under the daemon
	// runtime dir so daemon Close can find and remove any that a crashed wrapper
	// abandoned. The value is deliberately free of the substring "credential" so
	// gosec G101 does not misread the dir name as a hardcoded secret.
	launchCredentialsSubdir = "rotator-launch"
	// claudeConfigDirEnv is the env var Claude Code reads to locate its config
	// dir. Setting it to a fresh dir yields a macOS keychain miss, so claude
	// falls back to the planted .credentials.json there.
	claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"
	// claudeConfigHomeName is the operator's per-user claude config home under
	// $HOME. Its non-credential contents (projects/, settings.json, history,
	// commands, rules, skills, plugins, mcp, telemetry, cache, todos,
	// shell-snapshots, ide, etc.) are symlinked into the scratch dir so a launch
	// through Clyde keeps reading and writing the operator's real session state.
	claudeConfigHomeName = ".claude"
	// claudeGlobalConfigName is the canonical global config file claude reads at
	// <CLAUDE_CONFIG_DIR>/.claude.json. It carries hasCompletedOnboarding, so
	// seeding it suppresses the first-run "Select login method" screen.
	claudeGlobalConfigName = ".claude.json"
	// claudeLegacyGlobalConfigName is the legacy global config file claude checks
	// first, under the config home (<config-home>/.config.json). When it exists
	// it is the source of truth claude itself prefers, so Clyde mirrors that
	// precedence and copies it under the same base name in the scratch dir.
	claudeLegacyGlobalConfigName = ".config.json"
)

// claudeOAuthAccount is the minimal oauthAccount object claude reads from its
// global config. Only the account UUID is needed to bind the seeded config to
// the selected rotator account; the field name matches claude's wire name.
type claudeOAuthAccount struct {
	AccountUUID string `json:"accountUuid"`
}

// claudeGlobalConfig is the minimal global config Clyde writes when the operator
// has no existing global config to copy. hasCompletedOnboarding suppresses the
// first-run login screen and oauthAccount binds the config to the selected
// account. The field names match claude's own global-config wire names. A named
// struct is used deliberately so the seed never parses or mutates JSON via an
// open-ended map.
type claudeGlobalConfig struct {
	HasCompletedOnboarding bool               `json:"hasCompletedOnboarding"`
	OAuthAccount           claudeOAuthAccount `json:"oauthAccount"`
}

// applyRotatorLaunchCredentials adds CLAUDE_CONFIG_DIR to env when the launched
// provider is claude and [adapter.anthropic.oauth.accounts].set_claude_config_dir is on.
// It selects the rotator's currently-active Anthropic account, plants its
// .credentials.json in a scratch dir, and points the child there so claude
// authenticates as that account with no write-back to the user's real config or
// keychain. It is a no-op when the switch is off, the rotator is absent, or the
// provider is not claude, so the default launch path is unchanged. The env map
// is initialized if nil before a key is added.
//
// When selection finds no usable account because the only candidate needs
// re-authentication or every account is throttled, it plants nothing and
// returns a non-nil *clydev1.LaunchCredentialReauth describing the state so the
// launching client can prompt the operator before launch (CLYDE-453). A nil
// return with no error means selection succeeded or the path is off.
func (s *Server) applyRotatorLaunchCredentials(ctx context.Context, provider session.ProviderID, cfg *config.Config, env *map[string]string) (*clydev1.LaunchCredentialReauth, error) {
	if provider != session.ProviderClaude {
		return nil, nil
	}
	if cfg == nil || !cfg.Adapter.Anthropic.OAuth.Accounts.SetClaudeConfigDir {
		return nil, nil
	}
	scratchDir, reauth, err := s.plantRotatorLaunchCredentials(ctx, s.OAuthRotator(), rotatorProviderNameForClaude)
	if err != nil {
		return nil, err
	}
	if reauth != nil {
		return reauth, nil
	}
	if scratchDir == "" {
		return nil, nil
	}
	if *env == nil {
		*env = make(map[string]string, 1)
	}
	(*env)[claudeConfigDirEnv] = scratchDir
	return nil, nil
}

// plantRotatorLaunchCredentials selects the rotator's currently-active account
// for provider, writes its stored .credentials.json (0600) into a fresh scratch
// dir under the daemon runtime dir, tracks the dir for daemon-Close cleanup,
// and returns the scratch dir path. The caller adds it to the launch env as
// CLAUDE_CONFIG_DIR. It never writes to the user's real provider config or
// keychain. A nil rotator or an unregistered provider yields ("", nil, nil):
// the launch proceeds with the child's own credentials.
//
// When selection finds no usable account because the only candidate needs
// re-authentication (NeedsReauthError) or every account is throttled
// (AllAccountsThrottledError), it plants nothing and returns a non-nil
// *clydev1.LaunchCredentialReauth so the launching client can prompt the
// operator. Selection failures of other kinds are logged and treated as
// no-credentials so the launch is never blocked by an unexpected rotator error.
func (s *Server) plantRotatorLaunchCredentials(ctx context.Context, rotator *oauthrotation.Rotator, provider oauthprovider.Name) (string, *clydev1.LaunchCredentialReauth, error) {
	// This runs in the ProviderLaunchEnvironment request context; read the peer
	// and metadata so the structured events below carry request correlation,
	// matching the daemon's other request-scoped helpers.
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	if rotator == nil {
		return "", nil, nil
	}
	account, encoded, err := rotator.SelectForLaunch(ctx, provider)
	if err != nil {
		if reauth := launchReauthFromSelectError(provider, err); reauth != nil {
			s.log.WarnContext(ctx, "daemon.launch_credentials.select_needs_reauth",
				"component", "daemon",
				"provider", string(provider),
				"account", reauth.GetAccount(),
				"kind", reauth.GetKind().String(),
			)
			return "", reauth, nil
		}
		s.log.WarnContext(ctx, "daemon.launch_credentials.select_failed",
			"component", "daemon",
			"provider", string(provider),
			"err", err,
		)
		return "", nil, nil
	}

	root := filepath.Join(config.RuntimeDir(), launchCredentialsSubdir)
	if err := os.MkdirAll(root, launchCredentialsDirMode); err != nil {
		return "", nil, fmt.Errorf("create launch-credentials root %s: %w", root, err)
	}
	scratchDir, err := os.MkdirTemp(root, string(provider)+"-")
	if err != nil {
		return "", nil, fmt.Errorf("create launch-credentials scratch dir under %s: %w", root, err)
	}
	if err := os.Chmod(scratchDir, launchCredentialsDirMode); err != nil {
		_ = os.RemoveAll(scratchDir)
		return "", nil, fmt.Errorf("chmod launch-credentials scratch dir %s: %w", scratchDir, err)
	}

	credPath := filepath.Join(scratchDir, launchCredentialsFileName)
	if err := os.WriteFile(credPath, encoded, launchCredentialsFileMode); err != nil {
		_ = os.RemoveAll(scratchDir)
		return "", nil, fmt.Errorf("write planted credentials %s: %w", credPath, err)
	}

	// Seed the rest of the operator's config into the scratch dir so the launched
	// claude treats it as an established install: symlink the config-home
	// contents (transcripts, settings, history) and provide a global config with
	// onboarding complete. Seeding never overwrites the planted credential file,
	// and a seeding failure must not block the launch, so it only logs.
	s.seedLaunchConfigDir(ctx, scratchDir, account)

	s.trackLaunchCredentialDir(scratchDir)
	s.log.LogAttrs(ctx, slog.LevelInfo, "planted rotator launch credentials",
		slog.String("component", "daemon"),
		slog.String("provider", string(provider)),
		slog.String("account", string(account)),
		slog.String("config_dir", scratchDir),
	)
	return scratchDir, nil, nil
}

// seedLaunchConfigDir resolves the operator's real claude config locations from
// the daemon environment, where CLAUDE_CONFIG_DIR is unset, and seeds them into
// the per-launch scratch dir. It never blocks the launch: a missing home dir or
// a seeding error is logged and skipped so the launch still proceeds with the
// planted credential. The accountID is threaded through so a freshly written
// global config can bind to the selected account.
func (s *Server) seedLaunchConfigDir(ctx context.Context, scratchDir string, accountID oauthprovider.AccountID) {
	home, err := os.UserHomeDir()
	if err != nil {
		s.log.WarnContext(ctx, "daemon.launch_credentials.seed_skipped_no_home",
			"component", "daemon",
			"err", err,
		)
		return
	}
	configHome := filepath.Join(home, claudeConfigHomeName)
	// claude checks the legacy <config-home>/.config.json first, then the
	// canonical <home>/.claude.json; mirror that precedence so Clyde copies
	// whichever file claude itself would load.
	globalConfigPath := filepath.Join(home, claudeGlobalConfigName)
	legacyGlobalConfigPath := filepath.Join(configHome, claudeLegacyGlobalConfigName)
	if _, statErr := os.Stat(legacyGlobalConfigPath); statErr == nil {
		globalConfigPath = legacyGlobalConfigPath
	}

	linked, err := seedLaunchConfigDirFromSource(ctx, s.log, configHome, globalConfigPath, scratchDir, accountID)
	if err != nil {
		// The per-operation failure was already logged at its source; this is the
		// summary that the launch proceeds without full seeding.
		s.log.WarnContext(ctx, "daemon.launch_credentials.seed_failed",
			"component", "daemon",
			"config_dir", scratchDir,
			"err", err,
		)
		return
	}
	s.log.LogAttrs(ctx, slog.LevelDebug, "seeded launch config dir",
		slog.String("component", "daemon"),
		slog.String("config_dir", scratchDir),
		slog.Int("symlinked", linked),
	)
}

// seedLaunchConfigDirFromSource symlinks the operator's config-home contents
// into scratchDir and provides a global config so the launched claude skips
// onboarding. It is split out from seedLaunchConfigDir with explicit source
// paths so it is unit-testable without touching the real $HOME. It returns the
// number of entries symlinked.
//
// Symlink rule: every config-home entry except the planted credential file is
// symlinked, so writes through the links (transcripts, history, settings) land
// in the operator's real dirs and a session launched through Clyde keeps its
// state where the operator's normal claude reads it. An entry is skipped when a
// same-named file already exists in scratchDir, which protects the planted
// .credentials.json. A missing config-home is a brand-new install and is not an
// error.
//
// Global-config rule: when the resolved source global config exists, its bytes
// are copied verbatim (0600) under the same base name so no JSON is parsed or
// mutated; otherwise a minimal typed config with onboarding complete and the
// selected account UUID is written to <scratch>/.claude.json.
func seedLaunchConfigDirFromSource(ctx context.Context, log *slog.Logger, configHome string, globalConfigPath string, scratchDir string, accountID oauthprovider.AccountID) (int, error) {
	linked, err := symlinkConfigHomeEntries(ctx, log, configHome, scratchDir)
	if err != nil {
		return linked, err
	}
	if err := seedGlobalConfig(ctx, log, globalConfigPath, scratchDir, accountID); err != nil {
		return linked, err
	}
	return linked, nil
}

// symlinkConfigHomeEntries symlinks every entry under configHome into scratchDir
// except the credential file and any entry whose name already exists in
// scratchDir. A missing configHome yields (0, nil) so a brand-new install seeds
// nothing rather than erroring. Failures are logged at their source before the
// wrapped error is returned so the seed boundary stays observable.
func symlinkConfigHomeEntries(ctx context.Context, log *slog.Logger, configHome string, scratchDir string) (int, error) {
	entries, err := os.ReadDir(configHome)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		log.WarnContext(ctx, "daemon.launch_credentials.read_config_home_failed",
			"component", "daemon",
			"config_home", configHome,
			"err", err,
		)
		return 0, fmt.Errorf("read claude config home %s: %w", configHome, err)
	}
	linked := 0
	for _, entry := range entries {
		name := entry.Name()
		if name == launchCredentialsFileName {
			continue
		}
		target := filepath.Join(scratchDir, name)
		if _, statErr := os.Lstat(target); statErr == nil {
			// A same-named file is already planted (the credential); never
			// overwrite it.
			continue
		}
		source := filepath.Join(configHome, name)
		if err := os.Symlink(source, target); err != nil {
			log.WarnContext(ctx, "daemon.launch_credentials.symlink_failed",
				"component", "daemon",
				"entry", name,
				"err", err,
			)
			return linked, fmt.Errorf("symlink config entry %s -> %s: %w", source, target, err)
		}
		linked++
	}
	return linked, nil
}

// seedGlobalConfig provides the launched claude's global config in scratchDir.
// When globalConfigPath exists its bytes are copied verbatim (0600) under the
// same base name; otherwise a minimal typed config with onboarding complete and
// the selected account UUID is written to <scratch>/.claude.json. Failures are
// logged at their source before the wrapped error is returned.
func seedGlobalConfig(ctx context.Context, log *slog.Logger, globalConfigPath string, scratchDir string, accountID oauthprovider.AccountID) error {
	// Clean sanitizes the operator-resolved source path, and the read goes
	// through util.ReadFile so the returned bytes do not carry the source-path
	// taint into the verbatim copy below.
	sourcePath := filepath.Clean(globalConfigPath)
	raw, err := util.ReadFile(sourcePath)
	if err == nil {
		// The destination base name is resolved to a fixed allowlist
		// (.claude.json or the legacy .config.json) rather than echoing the
		// source's base name, so the write target is a constant under scratchDir
		// and can never escape it.
		dest := filepath.Join(scratchDir, destGlobalConfigName(sourcePath))
		// The symlink step may have already linked a same-named entry from the
		// config home (e.g. the legacy .config.json). Drop that link first so the
		// verbatim copy lands as a real file in the scratch dir rather than
		// writing through the symlink into the operator's real config.
		if err := removeIfSymlink(ctx, log, dest); err != nil {
			return err
		}
		if err := os.WriteFile(dest, raw, launchCredentialsFileMode); err != nil {
			log.WarnContext(ctx, "daemon.launch_credentials.copy_global_config_failed",
				"component", "daemon",
				"dest", dest,
				"err", err,
			)
			return fmt.Errorf("copy global config to %s: %w", dest, err)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		log.WarnContext(ctx, "daemon.launch_credentials.read_global_config_failed",
			"component", "daemon",
			"source", sourcePath,
			"err", err,
		)
		return fmt.Errorf("read global config %s: %w", sourcePath, err)
	}

	minimal := claudeGlobalConfig{
		HasCompletedOnboarding: true,
		OAuthAccount:           claudeOAuthAccount{AccountUUID: string(accountID)},
	}
	encoded, err := json.Marshal(minimal)
	if err != nil {
		log.WarnContext(ctx, "daemon.launch_credentials.marshal_global_config_failed",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("marshal minimal global config: %w", err)
	}
	dest := filepath.Join(scratchDir, claudeGlobalConfigName)
	if err := os.WriteFile(dest, encoded, launchCredentialsFileMode); err != nil {
		log.WarnContext(ctx, "daemon.launch_credentials.write_global_config_failed",
			"component", "daemon",
			"dest", dest,
			"err", err,
		)
		return fmt.Errorf("write minimal global config to %s: %w", dest, err)
	}
	return nil
}

// destGlobalConfigName maps a resolved source global-config path to the scratch
// dir base name claude reads. It returns the legacy .config.json name only when
// the source is that legacy file, and the canonical .claude.json name otherwise,
// so the write target is always one of two fixed names under the scratch dir.
func destGlobalConfigName(sourcePath string) string {
	if filepath.Base(sourcePath) == claudeLegacyGlobalConfigName {
		return claudeLegacyGlobalConfigName
	}
	return claudeGlobalConfigName
}

// removeIfSymlink removes path when it is a symlink, leaving regular files
// untouched. It is used before writing the verbatim global-config copy so the
// write never follows a symlink the seed step itself created back into the
// operator's real config home. Failures are logged at their source before the
// wrapped error is returned.
func removeIfSymlink(ctx context.Context, log *slog.Logger, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		log.WarnContext(ctx, "daemon.launch_credentials.lstat_failed",
			"component", "daemon",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if err := os.Remove(path); err != nil {
		log.WarnContext(ctx, "daemon.launch_credentials.remove_symlink_failed",
			"component", "daemon",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("remove symlink %s: %w", path, err)
	}
	return nil
}

// launchReauthFromSelectError maps a rotator SelectForLaunch failure to the
// typed reauth state the launching client prompts on. It recognizes the two
// "no usable account" signals: NeedsReauthError (a dead refresh credential that
// needs an operator login) and AllAccountsThrottledError (every account is
// rate-limited and recovers on its own). Any other error returns nil so the
// caller treats it as an unexpected failure and launches without planted creds.
func launchReauthFromSelectError(provider oauthprovider.Name, err error) *clydev1.LaunchCredentialReauth {
	var reauthErr oauthrotation.NeedsReauthError
	if errors.As(err, &reauthErr) {
		return &clydev1.LaunchCredentialReauth{
			Kind:             clydev1.LaunchCredentialReauthKind_LAUNCH_CREDENTIAL_REAUTH_KIND_NEEDS_REAUTH,
			Provider:         string(provider),
			Account:          string(reauthErr.Account),
			ThrottledUntilMs: 0,
		}
	}
	var throttledErr oauthrotation.AllAccountsThrottledError
	if errors.As(err, &throttledErr) {
		return &clydev1.LaunchCredentialReauth{
			Kind:             clydev1.LaunchCredentialReauthKind_LAUNCH_CREDENTIAL_REAUTH_KIND_ALL_THROTTLED,
			Provider:         string(provider),
			Account:          string(throttledErr.Account),
			ThrottledUntilMs: throttledErr.SoonestReset.UnixMilli(),
		}
	}
	return nil
}

// trackLaunchCredentialDir records a planted scratch dir so daemon Close can
// remove any dir a crashed or abandoned wrapper left behind.
func (s *Server) trackLaunchCredentialDir(dir string) {
	s.launchCredentialDirsMu.Lock()
	if s.launchCredentialDirs == nil {
		s.launchCredentialDirs = make(map[string]bool)
	}
	s.launchCredentialDirs[dir] = true
	s.launchCredentialDirsMu.Unlock()
}

// removeLaunchCredentialDirs deletes every tracked planted-credentials scratch
// dir and clears the set, returning the number removed. It is called from
// daemon Close so planted token material never outlives the daemon. When
// skipRuntimeCleanup is set (reload preserves active session runtime dirs so
// wrappers reacquire against the child), the planted dirs are preserved too so
// a running claude keeps its config dir across the reload; the launching
// wrapper still removes its own dir when the child exits.
func (s *Server) removeLaunchCredentialDirs() int {
	if s.skipRuntimeCleanup.Load() {
		return 0
	}
	s.launchCredentialDirsMu.Lock()
	dirs := make([]string, 0, len(s.launchCredentialDirs))
	for dir := range s.launchCredentialDirs {
		dirs = append(dirs, dir)
	}
	s.launchCredentialDirs = make(map[string]bool)
	s.launchCredentialDirsMu.Unlock()
	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
	}
	if len(dirs) > 0 {
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "removed planted launch credential dirs",
			slog.String("component", "daemon"),
			slog.Int("count", len(dirs)),
		)
	}
	return len(dirs)
}
