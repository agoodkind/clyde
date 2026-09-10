//go:build live

package live

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveConversationIndexSurvivesOrdinaryReload(t *testing.T) {
	home := writeLoadRulesFixtureHome(t)
	upstream := startSlowUpstream(t)
	upstream.releaseAll()
	buildStarted := time.Now()
	harness := newHarness(t)
	binaryInfo, err := os.Stat(harness.binPath)
	if err != nil || binaryInfo.ModTime().Before(buildStarted) {
		t.Fatalf("fresh binary was not built: %v", err)
	}
	harness.extraEnv = []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(home, ".codex"),
		"COPILOT_HOME=" + filepath.Join(home, ".copilot"),
		"CLYDE_CURSOR_PROJECTS_DIRS=" + filepath.Join(home, "cursor-projects"),
		"CLYDE_CURSOR_DATA_DIRS=" + filepath.Join(home, "cursor-data"),
		"CLYDE_ZED_DATA_DIRS=" + filepath.Join(home, "zed-data"),
	}
	harness.writeAdapterConfig(t, harness.cfg.AdapterPort, upstream.baseURL(), nil)
	harness.boot(t)
	harness.waitForConversationDiscovery(t, home, 10*time.Second)
	before, err := harness.runCLI(t, home, "conversation", "info", loadRulesConversationID)
	if err != nil || !strings.Contains(before, loadRulesFixtureSession) {
		t.Fatalf("initial conversation: %q, %v", before, err)
	}
	cachePath := filepath.Join(harness.cacheRoot, "clyde", "conversation-index.json")
	marker := time.Unix(100, 0)
	if err := os.Chtimes(cachePath, marker, marker); err != nil {
		t.Fatal(err)
	}
	oldPID := harness.latestWorkerPid()
	if oldPID == 0 {
		t.Fatal("missing initial worker pid")
	}
	harness.writeReloadEdit(t, upstream.baseURL())
	if !harness.waitForDaemonLog(workerReplacementStartedKey, 20*time.Second) {
		t.Fatalf("reload replacement did not start; logs: %s", harness.dumpLogsOnFailure(t))
	}
	if newPID := harness.latestWorkerPid(); newPID == 0 || newPID == oldPID {
		t.Fatalf("worker pid after reload = %d, before = %d", newPID, oldPID)
	}
	harness.waitForConversationDiscovery(t, home, 10*time.Second)
	after, err := harness.runCLI(t, home, "conversation", "info", loadRulesConversationID)
	if err != nil || before != after {
		t.Fatalf("conversation changed across reload: before=%q after=%q err=%v", before, after, err)
	}
	info, err := os.Stat(cachePath)
	if err != nil || !info.ModTime().Equal(marker) {
		t.Fatalf("unchanged reload rewrote cache: %v", err)
	}
	t.Logf("ordinary reload retained conversation and cache; worker %d -> %d", oldPID, harness.latestWorkerPid())
}
