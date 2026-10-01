//go:build live

package live

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
)

func TestLiveEmbeddedConversationReload(t *testing.T) {
	harness, configuration := newEmbeddedLifecycleHarness(t)
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	harness.boot(t)
	if !harness.waitForDaemonLog("daemon.conversation_semantic_embedded.opened", 20*time.Second) {
		t.Fatalf("initial embedded store did not open; logs: %s", harness.dumpLogsOnFailure(t))
	}
	oldPID := harness.latestWorkerPid()
	configuration.Conversation.Semantic.IndexedRoles = []string{"user", "assistant"}
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	if !harness.waitForDaemonLog(workerReplacementStartedKey, 20*time.Second) {
		t.Fatalf("replacement supervisor did not start; logs: %s", harness.dumpLogsOnFailure(t))
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		replacementPID := harness.latestWorkerPid()
		if replacementPID != oldPID && countEmbeddedLifecycleEvents(t, harness, "daemon.conversation_semantic_embedded.opened", replacementPID) == 1 {
			contents, err := os.ReadFile(harness.configPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("replacement opened the same store; old_pid=%d replacement_pid=%d config_sha256=%x logs=%s", oldPID, replacementPID, sha256.Sum256(contents), harness.dumpLogsOnFailure(t))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("replacement did not open the same store within 15 seconds; outbox_owned=%t; logs: %s",
		harness.logContains("another embedded ingestion worker owns"), harness.dumpLogsOnFailure(t))
}

func newEmbeddedLifecycleHarness(t *testing.T) (*harness, config.Config) {
	t.Helper()
	home := writeLoadRulesFixtureHome(t)
	fixture := filepath.Join(home, ".claude", "projects", "-tmp-load-rules-live", loadRulesFixtureSession+".jsonl")
	aged := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fixture, aged, aged); err != nil {
		t.Fatal(err)
	}
	harness := newHarness(t)
	harness.extraEnv = []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(home, ".codex"),
		"COPILOT_HOME=" + filepath.Join(home, ".copilot"),
		"CLYDE_CURSOR_PROJECTS_DIRS=" + filepath.Join(home, "cursor-projects"),
		"CLYDE_CURSOR_DATA_DIRS=" + filepath.Join(home, "cursor-data"),
		"CLYDE_ZED_DATA_DIRS=" + filepath.Join(home, "zed-data"),
	}
	database := createEmbeddedLifecycleDatabase(t)
	harness.writeConfig(t, harness.cfg.MITMPort, []string{"anthropic"})
	contents, err := os.ReadFile(harness.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var configuration config.Config
	if err := toml.Unmarshal(contents, &configuration); err != nil {
		t.Fatal(err)
	}
	configuration.Conversation.Semantic = config.ConversationSemanticConfig{
		IngestionEnabled: true,
		ProjectionProfile: config.ConversationProjectionProfileSourceSpan,
		CollectionID:      "lifecycle", PoolID: "lifecycle",
		CatalogPath:   filepath.Join(harness.stateRoot, "catalog.sqlite"),
		LockPath:      filepath.Join(harness.stateRoot, "catalog.lock"),
		MilvusAddress: "localhost:39530", MilvusDatabase: database, MilvusCollection: "vectors",
		EmbeddingBaseURL: "http://[::1]:5400/v1", EmbeddingModel: "nvidia/NV-EmbedCode-7b-v1",
		EmbeddingRevision: "lifecycle", VectorDimension: 4096, Normalization: "l2",
	}
	return harness, configuration
}

func TestLiveEmbeddedFailedReplacementKeepsSearch(t *testing.T) {
	testEmbeddedFailedReplacement(t, false)
}

func TestLiveEmbeddedPendingReplacementStopsWithSupervisor(t *testing.T) {
	for _, stop := range []string{"supervisor_signal", "current_worker_exit"} {
		t.Run(stop, func(t *testing.T) { testEmbeddedPendingReplacementStop(t, stop) })
	}
}

func testEmbeddedPendingReplacementStop(t *testing.T, stop string) {
	t.Helper()
	home := writeLoadRulesFixtureHome(t)
	harness := newHarness(t)
	harness.extraEnv = []string{"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(home, ".codex"), "COPILOT_HOME=" + filepath.Join(home, ".copilot"),
		"CLYDE_CURSOR_PROJECTS_DIRS=" + filepath.Join(home, "cursor-projects"),
		"CLYDE_CURSOR_DATA_DIRS=" + filepath.Join(home, "cursor-data"), "CLYDE_ZED_DATA_DIRS=" + filepath.Join(home, "zed-data")}
	harness.writeConfig(t, harness.cfg.MITMPort, []string{"anthropic"})
	harness.boot(t)
	oldPID := harness.latestWorkerPid()
	contents, err := os.ReadFile(harness.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var configuration config.Config
	if err := toml.Unmarshal(contents, &configuration); err != nil {
		t.Fatal(err)
	}
	configuration.Conversation.Semantic = config.ConversationSemanticConfig{
		IngestionEnabled: true,
		ProjectionProfile: config.ConversationProjectionProfileSourceSpan,
		CollectionID:      "pending-stop", PoolID: "pending-stop",
		CatalogPath: filepath.Join(harness.stateRoot, "catalog.sqlite"), LockPath: filepath.Join(harness.stateRoot, "catalog.lock"),
		MilvusAddress: "127.0.0.1:1", MilvusDatabase: "clyde_lifecycle_pending_stop", MilvusCollection: "vectors",
		EmbeddingBaseURL: "http://127.0.0.1:1/v1", EmbeddingModel: "nvidia/NV-EmbedCode-7b-v1",
		EmbeddingRevision: "pending-stop", VectorDimension: 4096, Normalization: "l2",
	}
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	if !harness.waitForDaemonLog(workerReplacementStartedKey, 20*time.Second) {
		t.Fatalf("pending replacement did not start; logs=%s", harness.dumpLogsOnFailure(t))
	}
	replacementPID := harness.latestWorkerPid()
	if oldPID == replacementPID {
		t.Fatal("replacement PID equals the old worker PID")
	}
	t.Cleanup(func() { _ = syscall.Kill(replacementPID, syscall.SIGKILL) })
	if err := syscall.Kill(replacementPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if countEmbeddedLifecycleEvents(t, harness, "daemon.conversation_semantic_embedded.opened", replacementPID) != 0 {
		t.Fatal("replacement admitted before the shutdown overlap")
	}
	supervisorPID := harness.cmd.Process.Pid
	// A stopped orphan process group receives SIGHUP on Darwin. Resume the
	// replacement before terminating its supervisor to observe supervisor cleanup.
	if err := syscall.Kill(replacementPID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if stop == "current_worker_exit" {
		if err := syscall.Kill(oldPID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := harness.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	_ = harness.cmd.Wait()
	harness.cmd = nil
	if err := syscall.Kill(replacementPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("supervisor returned before pending replacement stopped; supervisor_pid=%d old_pid=%d replacement_pid=%d replacement_error=%v logs=%s",
			supervisorPID, oldPID, replacementPID, err, harness.dumpLogsOnFailure(t))
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		oldErr := syscall.Kill(oldPID, 0)
		replacementErr := syscall.Kill(replacementPID, 0)
		if errors.Is(oldErr, syscall.ESRCH) && errors.Is(replacementErr, syscall.ESRCH) {
			t.Logf("supervisor stopped current and pending workers; supervisor_pid=%d old_pid=%d replacement_pid=%d logs=%s", supervisorPID, oldPID, replacementPID, harness.dumpLogsOnFailure(t))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("supervisor shutdown left a worker alive; supervisor_pid=%d old_pid=%d old_error=%v replacement_pid=%d replacement_error=%v logs=%s",
		supervisorPID, oldPID, syscall.Kill(oldPID, 0), replacementPID, syscall.Kill(replacementPID, 0), harness.dumpLogsOnFailure(t))
}

func TestLiveEmbeddedDescriptorMismatchKeepsSearch(t *testing.T) {
	testEmbeddedFailedReplacement(t, true)
}

func testEmbeddedFailedReplacement(t *testing.T, mismatch bool) {
	t.Helper()
	harness, configuration := newEmbeddedLifecycleHarness(t)
	configuration.Conversation.Semantic.SearchEnabled = true
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	harness.boot(t)
	search := dialPagingSearch(t, harness)
	before := waitEmbeddedLifecycleSearch(t, search)
	oldPID := harness.latestWorkerPid()
	if mismatch {
		configuration.Conversation.Semantic.EmbeddingRevision = "incompatible-generation"
	} else {
		configuration.Conversation.Semantic.MilvusAddress = "127.0.0.1:1"
	}
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	if !harness.waitForDaemonLog("daemon.supervisor.reload_replacement_rejected", 20*time.Second) {
		t.Fatalf("unavailable replacement was not rejected; logs=%s", harness.dumpLogsOnFailure(t))
	}
	failedPID := harness.latestWorkerPid()
	if failedPID == oldPID || countEmbeddedLifecycleEvents(t, harness, "daemon.conversation_semantic_embedded.opened", failedPID) != 0 {
		t.Fatalf("failed replacement pid=%d old_pid=%d opened the unavailable store", failedPID, oldPID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(failedPID, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(failedPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("failed replacement pid=%d still exists: %v", failedPID, err)
	}
	if err := syscall.Kill(oldPID, 0); err != nil {
		t.Fatalf("old worker pid=%d stopped: %v", oldPID, err)
	}
	after := waitEmbeddedLifecycleSearch(t, search)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("failed replacement changed search rows: before=%v after=%v", before, after)
	}
	if mismatch && !harness.logContains("store descriptor mismatch") {
		t.Fatalf("mismatched descriptor did not report the configuration failure; logs=%s", harness.dumpLogsOnFailure(t))
	}
	t.Logf("unavailable replacement terminated; old_pid=%d failed_pid=%d rows=%v logs=%s", oldPID, failedPID, after, harness.dumpLogsOnFailure(t))
	if err := harness.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for syscall.Kill(oldPID, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(oldPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("supervisor did not stop the retained old worker pid=%d: %v", oldPID, err)
	}
}

func TestLiveEmbeddedSearchOnlyReplacement(t *testing.T) {
	harness, configuration := newEmbeddedLifecycleHarness(t)
	configuration.Conversation.Semantic.SearchEnabled = true
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	harness.boot(t)
	search := dialPagingSearch(t, harness)
	before := waitEmbeddedLifecycleSearch(t, search)
	oldPID := harness.latestWorkerPid()
	configuration.Conversation.Semantic.IngestionEnabled = false
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	if !harness.waitForDaemonLog(workerReplacementStartedKey, 20*time.Second) {
		t.Fatalf("search-only replacement did not start; logs=%s", harness.dumpLogsOnFailure(t))
	}
	replacementPID := harness.latestWorkerPid()
	deadline := time.Now().Add(15 * time.Second)
	for countEmbeddedLifecycleEvents(t, harness, "daemon.conversation_semantic_embedded.opened", replacementPID) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if oldPID == replacementPID || countEmbeddedLifecycleEvents(t, harness, "daemon.conversation_semantic_embedded.opened", replacementPID) != 1 {
		t.Fatalf("search-only replacement did not open once; old_pid=%d replacement_pid=%d logs=%s", oldPID, replacementPID, harness.dumpLogsOnFailure(t))
	}
	after := waitEmbeddedLifecycleSearch(t, dialPagingSearch(t, harness))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("search-only replacement changed rows: before=%v after=%v", before, after)
	}
	t.Logf("search-only replacement returned retained rows; old_pid=%d replacement_pid=%d rows=%v logs=%s", oldPID, replacementPID, after, harness.dumpLogsOnFailure(t))
}

func waitEmbeddedLifecycleSearch(t *testing.T, search clydev1.ClydeServiceClient) []string {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		response, err := search.SearchConversations(ctx, &clydev1.SearchConversationsRequest{Query: "user probe alpha", Limit: 50})
		cancel()
		if err == nil && len(response.GetMatches()) > 0 {
			keys := make([]string, 0, len(response.GetMatches()))
			for _, match := range response.GetMatches() {
				keys = append(keys, fmt.Sprintf("%s:%d", match.GetConversation().GetId(), match.GetMessageIndex()))
			}
			return keys
		}
		if time.Now().After(deadline) {
			t.Fatalf("embedded public search returned no fixture rows: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func countEmbeddedLifecycleEvents(t *testing.T, harness *harness, event string, pid int) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(harness.stateRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Base(path) != "clyde-daemon.jsonl" {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var record struct {
				Message string `json:"msg"`
				PID     int    `json:"pid"`
			}
			if json.Unmarshal(scanner.Bytes(), &record) == nil && record.Message == event && record.PID == pid {
				count++
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}
