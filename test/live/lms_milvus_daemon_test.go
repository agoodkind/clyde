//go:build live

package live

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	lmclient "goodkind.io/lm-semantic-search/client"
	lmsemanticsearchv1 "goodkind.io/lm-semantic-search/gen/go/lmsemanticsearch/v1"

	"goodkind.io/clyde/internal/sandbox"
)

const (
	lmsModulePath    = "goodkind.io/lm-semantic-search"
	lmsDaemonPackage = lmsModulePath + "/cmd/lm-semantic-search-daemon"
	// lmsBinaryEnvVar is the variable that stores the path of the
	// lm-semantic-search daemon binary a live test starts.
	lmsBinaryEnvVar      = "CLYDE_TEST_LMS_DAEMON_BINARY"
	liveEmbeddingBaseURL = "http://localhost:5400/v1"
	liveEmbeddingPath    = "/v1"
	liveEmbeddingModel   = "nvidia/NV-EmbedCode-7b-v1"
	// liveEmbeddingDimension is the vector width of liveEmbeddingModel.
	liveEmbeddingDimension = 4096
	// liveEmbeddingAuthEnv is the variable that stores the embedding endpoint
	// credential. Tests pass it to the engine and never print it.
	liveEmbeddingAuthEnv = "OPENAI_API_KEY"
	// lmsUpdateDisabledURL is a discard port. The engine sends its
	// self-update check there instead of to the release API.
	lmsUpdateDisabledURL = "http://[::1]:9"
	lmsReadyTimeout      = 60 * time.Second
	lmsReadyPollInterval = 200 * time.Millisecond
	lmsStopTimeout       = 15 * time.Second
	lmsLogTailBytes      = 4096
)

// lmsMilvusDaemon is one lm-semantic-search daemon on the standard profile. It
// stores vectors in a Milvus database that the test created and embeds through
// the embedding counting proxy.
type lmsMilvusDaemon struct {
	socketPath string
	logPath    string
}

// startLMSMilvusDaemon starts the lm-semantic-search daemon binary at the
// commit Clyde's go.mod requires with every state path, the socket,
// and the config root inside throwaway sandbox roots. The daemon environment
// starts empty and sets HOME inside the roots. The daemon reads no operator
// setting and no operator ~/.context/.env file. Cleanup stops the process
// group and removes the roots.
func startLMSMilvusDaemon(t *testing.T, milvusDatabase string, embeddingBaseURL string) *lmsMilvusDaemon {
	t.Helper()

	apiKey := strings.TrimSpace(os.Getenv(liveEmbeddingAuthEnv))
	if apiKey == "" {
		t.Fatalf("environment variable %s is required for the embedding endpoint", liveEmbeddingAuthEnv)
	}
	roots, err := sandbox.NewRoots()
	if err != nil {
		t.Fatalf("create lm-semantic-search sandbox roots: %v", err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(roots.Base); removeErr != nil {
			t.Errorf("remove lm-semantic-search sandbox roots: %v", removeErr)
		}
	})
	binaryPath := pinnedLMSDaemonBinary(t)

	homeDir := filepath.Join(roots.Runtime, "home")
	contextRoot := filepath.Join(roots.Runtime, "context")
	socketPath := filepath.Join(roots.Runtime, "lms.sock")
	logPath := filepath.Join(roots.Base, "lms-daemon.out")
	for _, directory := range []string{homeDir, contextRoot, roots.State, roots.Config, roots.Cache} {
		if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr != nil {
			t.Fatalf("create lm-semantic-search sandbox directory %q: %v", directory, mkdirErr)
		}
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create lm-semantic-search daemon log: %v", err)
	}
	command := exec.CommandContext(context.Background(), binaryPath, "-profile", "standard", "-state-root", roots.State, "-socket", socketPath)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
		"HOME=" + homeDir,
		"XDG_STATE_HOME=" + filepath.Join(homeDir, "state"),
		"XDG_CONFIG_HOME=" + filepath.Join(homeDir, "config"),
		"CLAUDE_CONTEXTD_STATE_ROOT=" + roots.State,
		"CLAUDE_CONTEXTD_CONFIG_ROOT=" + roots.Config,
		"CLAUDE_CONTEXTD_CONTEXT_ROOT=" + contextRoot,
		"CLAUDE_CONTEXTD_SOCKET_PATH=" + socketPath,
		"CLAUDE_CONTEXTD_MODEL_CACHE_ROOT=" + roots.Cache,
		"CLAUDE_CONTEXT_PROFILE=standard",
		"MILVUS_ADDRESS=" + liveMilvusAddress,
		"MILVUS_DATABASE=" + milvusDatabase,
		"EMBEDDING_PROVIDER=OpenAI",
		"EMBEDDING_MODEL=" + liveEmbeddingModel,
		"EMBEDDING_DIMENSION=" + strconv.Itoa(liveEmbeddingDimension),
		"OPENAI_BASE_URL=" + embeddingBaseURL,
		liveEmbeddingAuthEnv + "=" + apiKey,
		"CLAUDE_CONTEXT_BACKGROUND_SYNC=false",
		"CLAUDE_CONTEXT_TRIGGER_WATCHER=false",
		"CLAUDE_CONTEXT_FILE_WATCHER=false",
		"CLAUDE_CONTEXT_DEBUG_LISTENER=false",
		"CLAUDE_CONTEXT_RESUME_ON_BOOT=false",
		"LM_SEMANTIC_SEARCH_UPDATE_API_BASE_URL=" + lmsUpdateDisabledURL,
	}
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if startErr := command.Start(); startErr != nil {
		_ = logFile.Close()
		t.Fatalf("start lm-semantic-search daemon %q: %v", binaryPath, startErr)
	}
	// The daemon writes its service log under the state root after boot and
	// writes only boot records to stdout and stderr.
	daemon := &lmsMilvusDaemon{socketPath: socketPath, logPath: filepath.Join(roots.State, "logs", "lm-semantic-search-daemon.log")}
	t.Cleanup(func() {
		stopLMSMilvusDaemon(t, command)
		if closeErr := logFile.Close(); closeErr != nil {
			t.Errorf("close lm-semantic-search daemon log: %v", closeErr)
		}
		if t.Failed() {
			t.Logf("lm-semantic-search daemon log tail:\n%s", daemon.logTail())
		}
	})
	daemon.waitForVersion(t)
	return daemon
}

// pinnedLMSDaemonBinary returns the daemon binary path from lmsBinaryEnvVar.
// The daemon links native libraries that this repository does not build, so
// the test cannot build it from the Clyde module graph. It fails unless the
// binary build info reports the package lmsDaemonPackage, the commit in the
// go.mod version, and an unmodified source tree.
func pinnedLMSDaemonBinary(t *testing.T) string {
	t.Helper()

	binaryPath := strings.TrimSpace(os.Getenv(lmsBinaryEnvVar))
	if binaryPath == "" {
		t.Fatalf("%s is unset; set it to an %s binary built at the lm-semantic-search commit go.mod requires", lmsBinaryEnvVar, lmsDaemonPackage)
	}
	pinnedCommit := lmsCommitOfThisBinary(t)
	info, err := buildinfo.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read build info of %s: %v", binaryPath, err)
	}
	if info.Path != lmsDaemonPackage {
		t.Fatalf("%s is package %q, want %q", binaryPath, info.Path, lmsDaemonPackage)
	}
	revision := ""
	modified := ""
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			revision = setting.Value
		}
		if setting.Key == "vcs.modified" {
			modified = setting.Value
		}
	}
	if !strings.HasPrefix(revision, pinnedCommit) || modified != "false" {
		t.Fatalf("%s was built at revision %q with vcs.modified %q, want the go.mod commit %s unmodified", binaryPath, revision, modified, pinnedCommit)
	}
	t.Logf("lm-semantic-search daemon %s built at revision %s", binaryPath, revision)
	return binaryPath
}

// lmsCommitOfThisBinary returns the commit hash in the lm-semantic-search
// pseudo-version this test binary was built against.
func lmsCommitOfThisBinary(t *testing.T) string {
	t.Helper()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("read test binary build info")
	}
	for _, dependency := range info.Deps {
		if dependency.Path != lmsModulePath {
			continue
		}
		separator := strings.LastIndex(dependency.Version, "-")
		if separator < 0 {
			t.Fatalf("%s version %q has no commit hash", lmsModulePath, dependency.Version)
		}
		return dependency.Version[separator+1:]
	}
	t.Fatalf("%s is not a dependency of this test binary", lmsModulePath)
	return ""
}

// waitForVersion polls the Version RPC until the daemon answers.
func (daemon *lmsMilvusDaemon) waitForVersion(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(lmsReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = daemon.version()
		if lastErr == nil {
			return
		}
		time.Sleep(lmsReadyPollInterval)
	}
	t.Fatalf("lm-semantic-search daemon did not answer Version within %s: %v\ndaemon log tail:\n%s", lmsReadyTimeout, lastErr, daemon.logTail())
}

func (daemon *lmsMilvusDaemon) version() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, client, err := lmclient.DialDaemon(ctx, daemon.socketPath)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	_, versionErr := client.Version(ctx, &lmsemanticsearchv1.VersionRequest{})
	closeErr := connection.Close()
	if versionErr != nil {
		return errors.Join(fmt.Errorf("version: %w", versionErr), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close: %w", closeErr)
	}
	return nil
}

// logTail returns the last lmsLogTailBytes of the daemon stdout and stderr.
func (daemon *lmsMilvusDaemon) logTail() string {
	contents, err := os.ReadFile(daemon.logPath)
	if err != nil {
		return fmt.Sprintf("(read lm-semantic-search daemon log: %v)", err)
	}
	if len(contents) > lmsLogTailBytes {
		contents = contents[len(contents)-lmsLogTailBytes:]
	}
	return string(contents)
}

// stopLMSMilvusDaemon sends SIGTERM to the daemon process group and sends
// SIGKILL after lmsStopTimeout.
func stopLMSMilvusDaemon(t *testing.T, command *exec.Cmd) {
	t.Helper()

	if command.Process == nil {
		return
	}
	processGroup := -command.Process.Pid
	if err := syscall.Kill(processGroup, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("stop lm-semantic-search daemon: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(lmsStopTimeout):
		if err := syscall.Kill(processGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill lm-semantic-search daemon: %v", err)
		}
		<-done
	}
}
