//go:build live

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

func TestEmbeddedRuntimeTimedOutStopJoinsBeforeStorageClose(t *testing.T) {
	requireLiveLocalEmbeddingModel(t)
	stores := isolateEmbeddedProjectionStores(t)
	runtimeRoot, err := os.MkdirTemp("/tmp", "c4-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(runtimeRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), runtimeRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("CLYDE_DAEMON_RELOAD_CHILD", "")
	t.Setenv("CLYDE_DAEMON_INHERITED_LISTENERS", "")
	t.Setenv("CLYDE_DAEMON_READY_FD", "")
	rolloutPath := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, rolloutPath)
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	requireLiveOwner(t, index)
	contents, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rolloutPath, rolloutPath+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(rolloutPath, 0o600); err != nil {
		t.Fatal(err)
	}
	ageLiveArtifact(t, rolloutPath)
	database := createLiveMilvusDatabase(t)
	configuration := config.NewConfigWithDefaults()
	configuration.Adapter.Enabled = false
	configuration.MITM.EnabledDefault = false
	configuration.Conversation.Semantic = config.ConversationSemanticConfig{
		IngestionEnabled:  true,
		SearchEnabled:     true,
		ProjectionProfile: config.ConversationProjectionProfileSourceSpan,
		CollectionID:      "fifo-stop", PoolID: "fifo-stop",
		CatalogPath: filepath.Join(t.TempDir(), "catalog.sqlite"), LockPath: filepath.Join(t.TempDir(), "catalog.lock"),
		MilvusAddress: liveMilvusAddress, MilvusDatabase: database, MilvusCollection: "vectors",
		EmbeddingBaseURL: liveEmbeddingBaseURL, EmbeddingModel: liveEmbeddingModel,
		EmbeddingRevision: "fifo-stop", VectorDimension: liveEmbeddingDimension, Normalization: "l2",
		QueryTimeout: config.Duration(30 * time.Second),
	}
	encoded, err := toml.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.GlobalConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.GlobalConfigPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	proofRoot, err := os.MkdirTemp("", "c4-fifo-proof-")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(proofRoot, "runtime.jsonl")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	logger := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug}))
	previousLogger := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	runtimeCtx, cancel := context.WithCancel(t.Context())
	returned := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				returned <- fmt.Errorf("runtime panic: %v", recovered)
			}
		}()
		returned <- RunContext(runtimeCtx, logger)
	}()
	var writer *os.File
	t.Cleanup(func() {
		cancel()
		if writer == nil {
			writer, err = os.OpenFile(rolloutPath, os.O_RDWR, 0o600)
			if err != nil {
				t.Error(err)
				return
			}
		}
		if writer != nil {
			_, _ = writer.Write(contents)
			_ = writer.Close()
		}
	})
	waitEmbeddedRuntimeEvent(t, logPath, "daemon.conversation_semantic_sync.pass_started", 20*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		writer, err = os.OpenFile(rolloutPath, os.O_WRONLY|syscall.O_NONBLOCK, 0o600)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if writer == nil {
		t.Fatalf("real source reader did not open FIFO: %v; logs=%s", err, logPath)
	}
	if embeddedRuntimeEventPosition(t, logPath, "daemon.conversation_semantic_sync.pass_completed") >= 0 {
		t.Fatalf("ingestion completed before FIFO release; logs=%s", logPath)
	}
	queryCtx, queryCancel := context.WithTimeout(t.Context(), 30*time.Second)
	page, queryErr := SearchConversations(queryCtx, conversation.SearchConversationsOptions{Query: "run the test suite", Limit: 1})
	queryCancel()
	if queryErr != nil || page.Source != conversation.SearchSourceSemantic || page.ReturnedCount != 0 {
		t.Fatalf("public search during blocked ingestion = %+v, %v, want an empty semantic page; logs=%s", page, queryErr, logPath)
	}
	if embeddedRuntimeEventPosition(t, logPath, "daemon.conversation_semantic_sync.pass_completed") >= 0 {
		t.Fatalf("ingestion completed before the concurrent search response; logs=%s", logPath)
	}
	cancel()
	waitEmbeddedRuntimeEvent(t, logPath, "daemon.conversation_semantic_sync.stop_timeout", 10*time.Second)
	waitEmbeddedRuntimeEvent(t, logPath, "daemon.conversation_semantic_embedded.close_pending", 5*time.Second)
	if embeddedRuntimeEventPosition(t, logPath, "daemon.conversation_semantic_embedded.closed") >= 0 {
		t.Fatalf("storage closed before the blocked source read completed; logs=%s", logPath)
	}
	assertEmbeddedCatalogDescriptor(t, configuration.Conversation.Semantic.CatalogPath, true)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("public runtime shutdown: %v; logs=%s", err, logPath)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("public runtime shutdown did not return after its stop deadline; logs=%s", logPath)
	}
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writer = nil
	waitEmbeddedRuntimeEvent(t, logPath, "daemon.conversation_semantic_embedded.closed", 10*time.Second)
	stopTimedOut := embeddedRuntimeEventPosition(t, logPath, "daemon.conversation_semantic_sync.stop_timeout")
	workerExited := embeddedRuntimeEventPosition(t, logPath, "daemon.conversation_semantic_embedded.worker_exited")
	storageClosed := embeddedRuntimeEventPosition(t, logPath, "daemon.conversation_semantic_embedded.closed")
	if stopTimedOut < 0 || workerExited <= stopTimedOut || storageClosed <= workerExited {
		t.Fatalf("stop/worker/storage event positions=%d/%d/%d; logs=%s", stopTimedOut, workerExited, storageClosed, logPath)
	}
	assertEmbeddedCatalogDescriptor(t, configuration.Conversation.Semantic.CatalogPath, false)
	t.Logf("public RunContext stop timed out before real FIFO release; pid=%d stop_timeout=%d worker_exited=%d storage_closed=%d logs=%s", os.Getpid(), stopTimedOut, workerExited, storageClosed, logPath)
}

func waitEmbeddedRuntimeEvent(t *testing.T, path, event string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if embeddedRuntimeEventPosition(t, path, event) >= 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runtime event %s is absent; logs=%s", event, path)
}

func embeddedRuntimeEventPosition(t *testing.T, path, event string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	position := 0
	for scanner.Scan() {
		var record struct {
			Message string `json:"msg"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record.Message == event {
			return position
		}
		position++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return -1
}

func assertEmbeddedCatalogDescriptor(t *testing.T, catalogPath string, opened bool) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "lsof", "-p", strconv.Itoa(os.Getpid()), "-Fn")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect real runtime descriptors: %v: %s", err, output)
	}
	canonical, err := filepath.EvalSymlinks(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	actual := strings.Contains(string(output), "\nn"+catalogPath+"\n") || strings.Contains(string(output), "\nn"+canonical+"\n")
	if actual != opened {
		t.Fatalf("catalog descriptor opened=%t, want %t for %s", actual, opened, catalogPath)
	}
}
