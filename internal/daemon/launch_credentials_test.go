package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fsnotify/fsnotify"

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
// launch-credentials path is the route-launched-claude switch.
func launchCredentialsConfig(routeLaunchedClaude bool) config.Config {
	return config.Config{
		Adapter: config.AdapterConfig{
			OAuth: config.AdapterOAuth{
				Rotation: config.AdapterOAuthRotation{
					RouteLaunchedClaude: routeLaunchedClaude,
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
	if err := srv.applyRotatorLaunchCredentials(context.Background(), session.ProviderClaude, &cfg, &env); err != nil {
		t.Fatalf("applyRotatorLaunchCredentials: %v", err)
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
	if err := srv.applyRotatorLaunchCredentials(context.Background(), session.ProviderClaude, &cfg, &env); err != nil {
		t.Fatalf("applyRotatorLaunchCredentials: %v", err)
	}
	if _, ok := env[claudeConfigDirEnv]; ok {
		t.Fatalf("CLAUDE_CONFIG_DIR set with switch off, env=%v", env)
	}

	root := filepath.Join(config.RuntimeDir(), launchCredentialsSubdir)
	if entries, err := os.ReadDir(root); err == nil && len(entries) != 0 {
		t.Fatalf("scratch dirs planted with switch off: %d", len(entries))
	}
}

// TestApplyRotatorLaunchCredentialsNonClaudeProviderNoop confirms a non-claude
// launch provider never plants credentials, even with the switch on.
func TestApplyRotatorLaunchCredentialsNonClaudeProviderNoop(t *testing.T) {
	srv, _ := newLaunchCredentialsServer(t, "acct-launch")

	cfg := launchCredentialsConfig(true)

	env := map[string]string{}
	if err := srv.applyRotatorLaunchCredentials(context.Background(), session.ProviderCodex, &cfg, &env); err != nil {
		t.Fatalf("applyRotatorLaunchCredentials: %v", err)
	}
	if _, ok := env[claudeConfigDirEnv]; ok {
		t.Fatalf("CLAUDE_CONFIG_DIR set for non-claude provider, env=%v", env)
	}
}
