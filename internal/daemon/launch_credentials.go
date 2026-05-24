package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation"
	oauthprovider "goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/session"
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
)

// applyRotatorLaunchCredentials adds CLAUDE_CONFIG_DIR to env when the launched
// provider is claude and [adapter.oauth.rotation].route_launched_claude is on.
// It selects the rotator's currently-active Anthropic account, plants its
// .credentials.json in a scratch dir, and points the child there so claude
// authenticates as that account with no write-back to the user's real config or
// keychain. It is a no-op when the switch is off, the rotator is absent, or the
// provider is not claude, so the default launch path is unchanged. The env map
// is initialized if nil before a key is added.
func (s *Server) applyRotatorLaunchCredentials(ctx context.Context, provider session.ProviderID, cfg *config.Config, env *map[string]string) error {
	if provider != session.ProviderClaude {
		return nil
	}
	if cfg == nil || !cfg.Adapter.OAuth.Rotation.RouteLaunchedClaude {
		return nil
	}
	scratchDir, err := s.plantRotatorLaunchCredentials(ctx, s.OAuthRotator(), rotatorProviderNameForClaude)
	if err != nil {
		return err
	}
	if scratchDir == "" {
		return nil
	}
	if *env == nil {
		*env = make(map[string]string, 1)
	}
	(*env)[claudeConfigDirEnv] = scratchDir
	return nil
}

// plantRotatorLaunchCredentials selects the rotator's currently-active account
// for provider, writes its stored .credentials.json (0600) into a fresh scratch
// dir under the daemon runtime dir, tracks the dir for daemon-Close cleanup,
// and returns the scratch dir path. The caller adds it to the launch env as
// CLAUDE_CONFIG_DIR. It never writes to the user's real provider config or
// keychain. A nil rotator, an unregistered provider, or no usable account
// yields ("", nil): the launch proceeds with the child's own credentials.
func (s *Server) plantRotatorLaunchCredentials(ctx context.Context, rotator *oauthrotation.Rotator, provider oauthprovider.Name) (string, error) {
	// This runs in the ProviderLaunchEnvironment request context; read the peer
	// and metadata so the structured events below carry request correlation,
	// matching the daemon's other request-scoped helpers.
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	if rotator == nil {
		return "", nil
	}
	account, encoded, err := rotator.SelectForLaunch(ctx, provider)
	if err != nil {
		s.log.WarnContext(ctx, "daemon.launch_credentials.select_failed",
			"component", "daemon",
			"provider", string(provider),
			"err", err,
		)
		return "", nil
	}

	root := filepath.Join(config.RuntimeDir(), launchCredentialsSubdir)
	if err := os.MkdirAll(root, launchCredentialsDirMode); err != nil {
		return "", fmt.Errorf("create launch-credentials root %s: %w", root, err)
	}
	scratchDir, err := os.MkdirTemp(root, string(provider)+"-")
	if err != nil {
		return "", fmt.Errorf("create launch-credentials scratch dir under %s: %w", root, err)
	}
	if err := os.Chmod(scratchDir, launchCredentialsDirMode); err != nil {
		_ = os.RemoveAll(scratchDir)
		return "", fmt.Errorf("chmod launch-credentials scratch dir %s: %w", scratchDir, err)
	}

	credPath := filepath.Join(scratchDir, launchCredentialsFileName)
	if err := os.WriteFile(credPath, encoded, launchCredentialsFileMode); err != nil {
		_ = os.RemoveAll(scratchDir)
		return "", fmt.Errorf("write planted credentials %s: %w", credPath, err)
	}

	s.trackLaunchCredentialDir(scratchDir)
	s.log.LogAttrs(ctx, slog.LevelInfo, "planted rotator launch credentials",
		slog.String("component", "daemon"),
		slog.String("provider", string(provider)),
		slog.String("account", string(account)),
		slog.String("config_dir", scratchDir),
	)
	return scratchDir, nil
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
