package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/slogger"
)

// newLaunchCredentialsServer builds a daemon Server backed by a rotator that has
// one logged-in "anthropic" account, with the runtime dir redirected into a
// temp dir so planted scratch dirs are isolated and removed with the test.
func newLaunchCredentialsServer(t *testing.T, account string) (*Server, *oauthrotation.Rotator) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	rotator := oauthrotation.NewRotator(nil)
	rotator.Register(&oauthRPCFakeProvider{
		name:         "anthropic",
		authorizeURL: "https://example.test/authorize",
		loginAccount: account,
	})
	if _, err := rotator.Login(
		context.Background(),
		provider.Name("anthropic"),
		provider.LoginOptions{Email: "user@example.test", Label: "primary"},
		func(string) {},
	); err != nil {
		t.Fatalf("seed login: %v", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	srv := newServerState(slogger.Concern(slogger.ConcernProcessDaemonLifecycle).Logger(), watcher)
	srv.SetOAuthRotatorAccessor(func() *oauthrotation.Rotator { return rotator })
	return srv, rotator
}

// launchCredentialsConfig builds a Config whose only meaningful field for the
// launch-credentials path is the set-claude-config-dir switch.
func launchCredentialsConfig(setClaudeConfigDir bool) config.Config {
	return config.Config{
		Adapter: config.AdapterConfig{
			Anthropic: config.AdapterAnthropic{
				OAuth: config.AdapterOAuth{
					Accounts: config.AdapterOAuthRotation{
						SetClaudeConfigDir: setClaudeConfigDir,
					},
				},
			},
		},
	}
}

// TestApplyRotatorLaunchCredentialsPlantsConfigDir confirms that with the switch
// on the daemon plants a 0600 .credentials.json for the selected account and
// returns CLAUDE_CONFIG_DIR pointing at the scratch dir.
func TestApplyRotatorLaunchCredentialsPlantsConfigDir(t *testing.T) {
	srv, _ := newLaunchCredentialsServer(t, "acct-launch")

	cfg := launchCredentialsConfig(true)

	env := map[string]string{}
	reauth, err := srv.applyRotatorLaunchCredentials(context.Background(), session.ProviderClaude, &cfg, &env)
	if err != nil {
		t.Fatalf("applyRotatorLaunchCredentials: %v", err)
	}
	if reauth != nil {
		t.Fatalf("unexpected reauth signal with a usable account: %+v", reauth)
	}

	configDir := env[claudeConfigDirEnv]
	if configDir == "" {
		t.Fatalf("CLAUDE_CONFIG_DIR not set, env=%v", env)
	}
	credPath := filepath.Join(configDir, launchCredentialsFileName)
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat planted credentials: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != launchCredentialsFileMode {
			t.Fatalf("planted credentials mode = %o, want %o", perm, launchCredentialsFileMode)
		}
	}

	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("read planted credentials: %v", err)
	}
	var stored oauthRPCStored
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode planted credentials: %v", err)
	}
	if stored.AccessToken != "acct-launch:token" {
		t.Fatalf("planted access token = %q, want acct-launch:token", stored.AccessToken)
	}

	// Close removes the planted scratch dir so no credential material is left.
	srv.Close()
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("scratch dir still present after Close: stat err = %v", err)
	}
}

// TestApplyRotatorLaunchCredentialsDisabledByDefault confirms the launch path is
// untouched when the switch is off: no CLAUDE_CONFIG_DIR and no scratch dir.
func TestApplyRotatorLaunchCredentialsDisabledByDefault(t *testing.T) {
	srv, _ := newLaunchCredentialsServer(t, "acct-launch")

	cfg := launchCredentialsConfig(false)

	env := map[string]string{}
	reauth, err := srv.applyRotatorLaunchCredentials(context.Background(), session.ProviderClaude, &cfg, &env)
	if err != nil {
		t.Fatalf("applyRotatorLaunchCredentials: %v", err)
	}
	if reauth != nil {
		t.Fatalf("unexpected reauth signal with switch off: %+v", reauth)
	}
	if _, ok := env[claudeConfigDirEnv]; ok {
		t.Fatalf("CLAUDE_CONFIG_DIR set with switch off, env=%v", env)
	}

	root := filepath.Join(config.RuntimeDir(), launchCredentialsSubdir)
	if entries, err := os.ReadDir(root); err == nil && len(entries) != 0 {
		t.Fatalf("scratch dirs planted with switch off: %d", len(entries))
	}
}

// TestLaunchReauthFromSelectErrorClassifiesNeedsReauth confirms a rotator
// NeedsReauthError maps to a NEEDS_REAUTH signal naming the affected account.
func TestLaunchReauthFromSelectErrorClassifiesNeedsReauth(t *testing.T) {
	err := oauthrotation.NeedsReauthError{Provider: "anthropic", Account: "acct-dead"}
	reauth := launchReauthFromSelectError(provider.Name("anthropic"), err)
	if reauth == nil {
		t.Fatalf("nil reauth for NeedsReauthError")
	}
	if reauth.GetKind() != clydev1.LaunchCredentialReauthKind_LAUNCH_CREDENTIAL_REAUTH_KIND_NEEDS_REAUTH {
		t.Fatalf("kind = %v, want NEEDS_REAUTH", reauth.GetKind())
	}
	if reauth.GetAccount() != "acct-dead" || reauth.GetProvider() != "anthropic" {
		t.Fatalf("reauth = %+v, want anthropic/acct-dead", reauth)
	}
	if reauth.GetThrottledUntilMs() != 0 {
		t.Fatalf("throttled_until_ms = %d, want 0 for needs-reauth", reauth.GetThrottledUntilMs())
	}
}

// TestLaunchReauthFromSelectErrorClassifiesAllThrottled confirms a rotator
// AllAccountsThrottledError maps to an ALL_THROTTLED signal carrying the
// soonest reset.
func TestLaunchReauthFromSelectErrorClassifiesAllThrottled(t *testing.T) {
	reset := time.UnixMilli(1_700_000_000_000).UTC()
	err := oauthrotation.AllAccountsThrottledError{Provider: "anthropic", SoonestReset: reset, Account: "acct-throttled"}
	reauth := launchReauthFromSelectError(provider.Name("anthropic"), err)
	if reauth == nil {
		t.Fatalf("nil reauth for AllAccountsThrottledError")
	}
	if reauth.GetKind() != clydev1.LaunchCredentialReauthKind_LAUNCH_CREDENTIAL_REAUTH_KIND_ALL_THROTTLED {
		t.Fatalf("kind = %v, want ALL_THROTTLED", reauth.GetKind())
	}
	if reauth.GetThrottledUntilMs() != reset.UnixMilli() {
		t.Fatalf("throttled_until_ms = %d, want %d", reauth.GetThrottledUntilMs(), reset.UnixMilli())
	}
}

// TestLaunchReauthFromSelectErrorIgnoresOtherErrors confirms an unrelated
// selection error yields nil so the caller treats it as no-credentials rather
// than prompting.
func TestLaunchReauthFromSelectErrorIgnoresOtherErrors(t *testing.T) {
	if reauth := launchReauthFromSelectError(provider.Name("anthropic"), errors.New("boom")); reauth != nil {
		t.Fatalf("reauth = %+v for unrelated error, want nil", reauth)
	}
}

// seedTestLogger returns a logger that discards output for the seed unit tests,
// which assert filesystem effects rather than log lines.
func seedTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeSeedFile writes content to path, creating parent dirs, failing the test
// on error.
func writeSeedFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSeedLaunchConfigDirFromSourceSymlinksAndCopies covers the common case: a
// populated config home is symlinked entry-by-entry into the scratch dir except
// the credential file, the symlinks resolve to the real sources, and an existing
// global config is copied verbatim.
func TestSeedLaunchConfigDirFromSourceSymlinksAndCopies(t *testing.T) {
	configHome := t.TempDir()
	scratchDir := t.TempDir()

	projectsDir := filepath.Join(configHome, "projects")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	settingsPath := filepath.Join(configHome, "settings.json")
	writeSeedFile(t, settingsPath, `{"theme":"dark"}`)
	// A credential file in the config home must never be symlinked.
	writeSeedFile(t, filepath.Join(configHome, launchCredentialsFileName), `{"secret":"do-not-link"}`)

	globalConfigPath := filepath.Join(filepath.Dir(configHome), claudeGlobalConfigName)
	globalConfigBytes := `{"hasCompletedOnboarding":true,"oauthAccount":{"accountUuid":"real-acct"}}`
	writeSeedFile(t, globalConfigPath, globalConfigBytes)

	// Plant the credential file in the scratch dir before seeding, mirroring the
	// production order, so the loop must not overwrite it.
	credInScratch := filepath.Join(scratchDir, launchCredentialsFileName)
	writeSeedFile(t, credInScratch, `{"planted":"creds"}`)

	linked, err := seedLaunchConfigDirFromSource(context.Background(), seedTestLogger(), configHome, globalConfigPath, scratchDir, provider.AccountID("acct-seed"))
	if err != nil {
		t.Fatalf("seedLaunchConfigDirFromSource: %v", err)
	}
	// projects/ and settings.json are linked; .credentials.json is skipped.
	if linked != 2 {
		t.Fatalf("symlinked count = %d, want 2", linked)
	}

	wantLinks := map[string]string{
		"projects":      projectsDir,
		"settings.json": settingsPath,
	}
	for name, source := range wantLinks {
		linkPath := filepath.Join(scratchDir, name)
		info, lerr := os.Lstat(linkPath)
		if lerr != nil {
			t.Fatalf("lstat %s: %v", linkPath, lerr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink", name)
		}
		resolved, rerr := filepath.EvalSymlinks(linkPath)
		if rerr != nil {
			t.Fatalf("eval symlink %s: %v", linkPath, rerr)
		}
		wantResolved, _ := filepath.EvalSymlinks(source)
		if resolved != wantResolved {
			t.Fatalf("%s resolves to %q, want %q", name, resolved, wantResolved)
		}
	}

	// The planted credential remains a regular file with its original content.
	if info, serr := os.Lstat(credInScratch); serr != nil {
		t.Fatalf("lstat planted cred: %v", serr)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf(".credentials.json became a symlink")
	}
	gotCred, _ := os.ReadFile(credInScratch)
	if string(gotCred) != `{"planted":"creds"}` {
		t.Fatalf("planted cred content = %q, want planted creds intact", string(gotCred))
	}

	// The global config is copied verbatim, byte-equal, to <scratch>/.claude.json.
	copied, cerr := os.ReadFile(filepath.Join(scratchDir, claudeGlobalConfigName))
	if cerr != nil {
		t.Fatalf("read copied global config: %v", cerr)
	}
	if string(copied) != globalConfigBytes {
		t.Fatalf("copied global config = %q, want byte-equal %q", string(copied), globalConfigBytes)
	}
}

// TestSeedLaunchConfigDirFromSourceWritesMinimalConfig covers the no-source-config
// case: when no global config exists, a minimal one with onboarding complete and
// the selected account UUID is written and parses back.
func TestSeedLaunchConfigDirFromSourceWritesMinimalConfig(t *testing.T) {
	configHome := t.TempDir()
	scratchDir := t.TempDir()
	// A global config path that does not exist forces the minimal-config branch.
	globalConfigPath := filepath.Join(filepath.Dir(configHome), claudeGlobalConfigName)

	if _, err := seedLaunchConfigDirFromSource(context.Background(), seedTestLogger(), configHome, globalConfigPath, scratchDir, provider.AccountID("acct-uuid-123")); err != nil {
		t.Fatalf("seedLaunchConfigDirFromSource: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(scratchDir, claudeGlobalConfigName))
	if err != nil {
		t.Fatalf("read minimal global config: %v", err)
	}
	var parsed claudeGlobalConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode minimal global config: %v", err)
	}
	if !parsed.HasCompletedOnboarding {
		t.Fatalf("hasCompletedOnboarding = false, want true")
	}
	if parsed.OAuthAccount.AccountUUID != "acct-uuid-123" {
		t.Fatalf("accountUuid = %q, want acct-uuid-123", parsed.OAuthAccount.AccountUUID)
	}
}

// TestSeedLaunchConfigDirFromSourceMissingConfigHome confirms an absent config
// home is a no-op symlink step that still seeds a global config and does not
// error, mirroring a brand-new install.
func TestSeedLaunchConfigDirFromSourceMissingConfigHome(t *testing.T) {
	scratchDir := t.TempDir()
	// configHome is a path under a temp dir that is never created.
	configHome := filepath.Join(t.TempDir(), "absent-config-home")
	globalConfigPath := filepath.Join(configHome, claudeGlobalConfigName)

	// Plant a credential so we can assert seeding leaves it intact.
	credInScratch := filepath.Join(scratchDir, launchCredentialsFileName)
	writeSeedFile(t, credInScratch, `{"planted":"creds"}`)

	linked, err := seedLaunchConfigDirFromSource(context.Background(), seedTestLogger(), configHome, globalConfigPath, scratchDir, provider.AccountID("acct-new"))
	if err != nil {
		t.Fatalf("seedLaunchConfigDirFromSource with absent config home: %v", err)
	}
	if linked != 0 {
		t.Fatalf("symlinked count = %d, want 0 for absent config home", linked)
	}
	if info, serr := os.Lstat(credInScratch); serr != nil {
		t.Fatalf("planted cred missing after seeding: %v", serr)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf(".credentials.json became a symlink")
	}
	// A minimal global config is still written since no source exists.
	if _, gerr := os.Stat(filepath.Join(scratchDir, claudeGlobalConfigName)); gerr != nil {
		t.Fatalf("minimal global config not written: %v", gerr)
	}
}

// TestSeedLaunchConfigDirFromSourceCopiesLegacyName confirms that when the
// resolved source is the legacy .config.json the copy lands under that same base
// name in the scratch dir, matching claude's legacy precedence.
func TestSeedLaunchConfigDirFromSourceCopiesLegacyName(t *testing.T) {
	configHome := t.TempDir()
	scratchDir := t.TempDir()
	legacyPath := filepath.Join(configHome, claudeLegacyGlobalConfigName)
	legacyBytes := `{"hasCompletedOnboarding":true,"oauthAccount":{"accountUuid":"legacy-acct"}}`
	writeSeedFile(t, legacyPath, legacyBytes)

	if _, err := seedLaunchConfigDirFromSource(context.Background(), seedTestLogger(), configHome, legacyPath, scratchDir, provider.AccountID("acct-legacy")); err != nil {
		t.Fatalf("seedLaunchConfigDirFromSource: %v", err)
	}
	copied, err := os.ReadFile(filepath.Join(scratchDir, claudeLegacyGlobalConfigName))
	if err != nil {
		t.Fatalf("read copied legacy config: %v", err)
	}
	if string(copied) != legacyBytes {
		t.Fatalf("copied legacy config = %q, want byte-equal %q", string(copied), legacyBytes)
	}
	// The legacy entry under config home is also symlinked into the scratch dir;
	// that is fine because the verbatim copy lands under the same name and the
	// symlink step skips an entry only when a same-named file already exists.
}

// TestApplyRotatorLaunchCredentialsNonClaudeProviderNoop confirms a non-claude
// launch provider never plants credentials, even with the switch on.
func TestApplyRotatorLaunchCredentialsNonClaudeProviderNoop(t *testing.T) {
	srv, _ := newLaunchCredentialsServer(t, "acct-launch")

	cfg := launchCredentialsConfig(true)

	env := map[string]string{}
	reauth, err := srv.applyRotatorLaunchCredentials(context.Background(), session.ProviderCodex, &cfg, &env)
	if err != nil {
		t.Fatalf("applyRotatorLaunchCredentials: %v", err)
	}
	if reauth != nil {
		t.Fatalf("unexpected reauth signal for non-claude provider: %+v", reauth)
	}
	if _, ok := env[claudeConfigDirEnv]; ok {
		t.Fatalf("CLAUDE_CONFIG_DIR set for non-claude provider, env=%v", env)
	}
}
