//go:build live

package live

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveHarnessKeepsInheritedConversationCacheUntouched(t *testing.T) {
	inheritedRoot := t.TempDir()
	inheritedCache := filepath.Join(inheritedRoot, "clyde", "conversation-index.json")
	if err := os.MkdirAll(filepath.Dir(inheritedCache), 0o700); err != nil {
		t.Fatal(err)
	}
	const sentinel = "inherited temporary cache must remain unchanged\n"
	if err := os.WriteFile(inheritedCache, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", inheritedRoot)
	home := writeLoadRulesFixtureHome(t)
	harness := newHarness(t)
	harness.writeConversationOnlyConfig(t, nil, "")
	harness.extraEnv = []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(home, ".codex"),
		"COPILOT_HOME=" + filepath.Join(home, ".copilot"),
		"CLYDE_CURSOR_PROJECTS_DIRS=" + filepath.Join(home, "cursor-projects"),
		"CLYDE_CURSOR_DATA_DIRS=" + filepath.Join(home, "cursor-data"),
		"CLYDE_ZED_DATA_DIRS=" + filepath.Join(home, "zed-data"),
	}
	harness.boot(t)
	harness.waitForConversationDiscovery(t, home, 10*time.Second)
	cachePath := filepath.Join(harness.cacheRoot, "clyde", "conversation-index.json")
	cache, err := os.ReadFile(cachePath)
	if err != nil || !strings.Contains(string(cache), loadRulesConversationID) {
		t.Errorf("child did not persist the fixture inside its cache: %s, %v", cachePath, err)
	}
	inherited, err := os.ReadFile(inheritedCache)
	if err != nil || string(inherited) != sentinel {
		t.Errorf("child changed inherited cache %s: sentinel differs, err=%v", inheritedCache, err)
	}
	t.Logf("worker cache path=%s; inherited cache path=%s", cachePath, inheritedCache)
}
