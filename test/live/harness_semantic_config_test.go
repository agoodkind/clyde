//go:build live

package live

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	lmsemanticsearchv1 "goodkind.io/lm-semantic-search/gen/go/lmsemanticsearch/v1"
)

type semanticMethodCounter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (counter *semanticMethodCounter) record(method string) {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counter.calls[method]++
}

func (counter *semanticMethodCounter) count(method string) int {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	return counter.calls[method]
}

func (counter *semanticMethodCounter) total() int {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	total := 0
	for _, count := range counter.calls {
		total += count
	}
	return total
}

func (counter *semanticMethodCounter) unaryInterceptor(
	ctx context.Context,
	request interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	counter.record(info.FullMethod)
	return handler(ctx, request)
}

func (counter *semanticMethodCounter) streamInterceptor(
	server interface{},
	stream grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	counter.record(info.FullMethod)
	return handler(server, stream)
}

type countingSemanticService struct {
	lmsemanticsearchv1.UnimplementedSemanticSearchDaemonServiceServer
	results []*lmsemanticsearchv1.ConversationSearchResult
}

func (service *countingSemanticService) RegisterConversationCollection(
	context.Context,
	*lmsemanticsearchv1.RegisterConversationCollectionRequest,
) (*lmsemanticsearchv1.RegisterConversationCollectionResponse, error) {
	return &lmsemanticsearchv1.RegisterConversationCollectionResponse{}, nil
}

func (service *countingSemanticService) SearchConversations(
	context.Context,
	*lmsemanticsearchv1.SearchConversationsRequest,
) (*lmsemanticsearchv1.SearchConversationsResponse, error) {
	return &lmsemanticsearchv1.SearchConversationsResponse{Results: service.results}, nil
}

func startCountingSemanticService(
	t *testing.T,
	results []*lmsemanticsearchv1.ConversationSearchResult,
) (string, *semanticMethodCounter) {
	t.Helper()

	socketFile, err := os.CreateTemp("/tmp", "clyde-semantic-*.sock")
	if err != nil {
		t.Fatalf("create fake semantic socket path: %v", err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatalf("close fake semantic socket placeholder: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove fake semantic socket placeholder: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove fake semantic socket: %v", err)
		}
	})
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on fake semantic socket: %v", err)
	}
	counter := &semanticMethodCounter{mu: sync.Mutex{}, calls: make(map[string]int)}
	server := grpc.NewServer(
		grpc.UnaryInterceptor(counter.unaryInterceptor),
		grpc.StreamInterceptor(counter.streamInterceptor),
	)
	lmsemanticsearchv1.RegisterSemanticSearchDaemonServiceServer(server, &countingSemanticService{
		results: results,
	})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		serveErr := <-serveDone
		if serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			t.Errorf("serve fake semantic service: %v", serveErr)
		}
	})
	return socketPath, counter
}

func TestResolveFakeConversationSemanticConfigDefaults(t *testing.T) {
	t.Setenv("CLYDE_TEST_CONVERSATION_INGESTION", "")
	t.Setenv("CLYDE_TEST_CONVERSATION_SEARCH", "")
	t.Setenv("CLYDE_TEST_COLLECTION_ID", "")

	first := resolveFakeConversationSemanticConfig(t)
	second := resolveFakeConversationSemanticConfig(t)

	if first.IngestionEnabled {
		t.Fatal("IngestionEnabled = true, want false")
	}
	if first.SearchEnabled {
		t.Fatal("SearchEnabled = true, want false")
	}
	if first.CollectionID == "" {
		t.Fatal("CollectionID = empty, want random id")
	}
	if second.CollectionID == "" {
		t.Fatal("second CollectionID = empty, want random id")
	}
	if first.CollectionID == second.CollectionID {
		t.Fatalf("CollectionID = %q twice, want a fresh random id per resolution", first.CollectionID)
	}
}

func TestResolveFakeConversationSemanticConfigHonorsEnv(t *testing.T) {
	t.Setenv("CLYDE_TEST_CONVERSATION_INGESTION", "true")
	t.Setenv("CLYDE_TEST_CONVERSATION_SEARCH", "true")
	t.Setenv("CLYDE_TEST_COLLECTION_ID", "test-collection")

	cfg := resolveFakeConversationSemanticConfig(t)
	if !cfg.IngestionEnabled {
		t.Fatal("IngestionEnabled = false, want true")
	}
	if !cfg.SearchEnabled {
		t.Fatal("SearchEnabled = false, want true")
	}
	if cfg.CollectionID != "test-collection" {
		t.Fatalf("CollectionID = %q, want %q", cfg.CollectionID, "test-collection")
	}
}

func TestResolveFakeConversationSemanticConfigDirectionsAreIndependent(t *testing.T) {
	t.Setenv("CLYDE_TEST_CONVERSATION_INGESTION", "false")
	t.Setenv("CLYDE_TEST_CONVERSATION_SEARCH", "true")

	cfg := resolveFakeConversationSemanticConfig(t)
	if cfg.IngestionEnabled {
		t.Fatal("IngestionEnabled = true, want false")
	}
	if !cfg.SearchEnabled {
		t.Fatal("SearchEnabled = false, want true")
	}
}

func TestWriteConfigCarriesConversationSemanticSettings(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		write func(*testing.T, *harness)
	}{
		{
			name: "mitm only",
			write: func(t *testing.T, h *harness) {
				t.Helper()
				h.writeConfig(t, h.cfg.MITMPort, []string{"anthropic"})
			},
		},
		{
			name: "adapter",
			write: func(t *testing.T, h *harness) {
				t.Helper()
				h.writeAdapterConfig(t, h.cfg.AdapterPort, "http://[::1]:18080", nil)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			configRoot := t.TempDir()
			h := &harness{
				stateRoot:   root,
				configRoot:  configRoot,
				runtimeRoot: t.TempDir(),
				cfg: fakePorts{
					MITMPort:      fakeMITMPort,
					AdapterPort:   fakeAdapterPort,
					CursorPort:    fakeCursorPort,
					TopologyPort:  fakeTopologyPort,
					MovedMITMPort: fakeMITMPort + 1,
				},
				conversationSemantic: fakeConversationSemanticConfig{
					IngestionEnabled: true,
					SearchEnabled:    true,
					CollectionID:     "test-collection",
				},
				binPath:      "",
				configPath:   filepath.Join(configRoot, "clyde", "config.toml"),
				daemonLog:    "",
				prodPidsPre:  nil,
				extraEnv:     nil,
				requireToken: "",
				cmd:          nil,
			}
			if err := os.MkdirAll(filepath.Dir(h.configPath), 0o700); err != nil {
				t.Fatalf("mkdir config dir: %v", err)
			}

			testCase.write(t, h)

			body, err := os.ReadFile(h.configPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}

			// Decode the written file the way the daemon will rather than
			// searching it for strings. A substring is satisfied by a value in
			// the wrong section, and by a file whose syntax the daemon would
			// reject, so it can pass for a config the daemon cannot boot on.
			var written config.Config
			if err := toml.Unmarshal(body, &written); err != nil {
				t.Fatalf("parse config: %v\n%s", err, body)
			}
			semantic := written.Conversation.Semantic
			if !semantic.FeedsEngine() {
				t.Fatalf("FeedsEngine() = false, want true:\n%s", body)
			}
			if !semantic.AnswersSearch() {
				t.Fatalf("AnswersSearch() = false, want true:\n%s", body)
			}
			if semantic.CollectionID != "test-collection" {
				t.Fatalf("CollectionID = %q, want %q:\n%s", semantic.CollectionID, "test-collection", body)
			}
		})
	}
}

func TestSemanticDirectionsKeepRawPathsEngineFreeAndSearchReadOnly(t *testing.T) {
	home := writeLoadRulesFixtureHome(t)
	results := []*lmsemanticsearchv1.ConversationSearchResult{{
		ConversationId: loadRulesConversationID,
		MessageIndex:   1,
		Role:           "user",
		TimestampUnix:  1_710_000_000,
		Score:          0.9,
		Content:        "stored semantic result",
		LoadRules:      loadRulesDefaultTag,
	}}
	semanticSocket, counter := startCountingSemanticService(t, results)
	h := newHarness(t)
	h.extraEnv = []string{"HOME=" + home}
	h.writeConversationOnlyConfig(t, nil, semanticSocket)
	h.boot(t)
	h.waitForConversationDiscovery(t, home, 60*time.Second)

	rawCommands := [][]string{
		{"conversation", "search"},
		{"conversation", "search", loadRulesConversationID},
		{"conversation", "search", loadRulesConversationID, "--around", "1", "--window", "0"},
		{"conversation", "export", loadRulesConversationID, "--only", "chat", "--stdout"},
		{"daemon", "status"},
	}
	for _, arguments := range rawCommands {
		if _, err := h.runCLI(t, home, arguments...); err != nil {
			t.Fatalf("run %q: %v", strings.Join(arguments, " "), err)
		}
	}
	if _, err := h.runCLI(t, home, "conversation", "search", "--query", "probe"); err == nil {
		t.Fatal("semantic search succeeded while search was disabled")
	}
	if got := counter.total(); got != 0 {
		t.Fatalf("engine calls with both directions disabled = %d, want 0", got)
	}

	workerBefore := h.latestWorkerPid()
	if workerBefore == 0 {
		t.Fatal("could not read worker pid before semantic config reload")
	}
	h.conversationSemantic.SearchEnabled = true
	h.writeConversationOnlyConfig(t, nil, semanticSocket)
	if !h.waitForDaemonLog(reloadTriggeredKey, 15*time.Second) {
		t.Fatalf("semantic config change did not trigger a reload")
	}
	workerAfter := waitForWorkerPIDChange(t, h, workerBefore)

	searchOutput, err := h.runCLI(t, home, "conversation", "search", "--query", "stored semantic")
	if err != nil {
		t.Fatalf("run search-only query: %v", err)
	}
	if !strings.Contains(searchOutput, "stored semantic result") {
		t.Fatalf("search output = %q, want stored semantic result", searchOutput)
	}
	if calls := counter.count(lmsemanticsearchv1.SemanticSearchDaemonService_RegisterConversationCollection_FullMethodName); calls != 1 {
		t.Fatalf("register calls = %d, want 1", calls)
	}
	if calls := counter.count(lmsemanticsearchv1.SemanticSearchDaemonService_SearchConversations_FullMethodName); calls != 1 {
		t.Fatalf("search calls = %d, want 1", calls)
	}
	assertNoSemanticWrites(t, counter)

	invalidConfig := fmt.Sprintf(`[logging]
level = "debug"

[conversation.semantic]
enabled = false
search_enabled = true
socket_path = %q
collection_id = %q

[adapter]
enabled = false

[mitm]
enabled_default = false
`, semanticSocket, h.conversationSemantic.CollectionID)
	if err := os.WriteFile(h.configPath, []byte(invalidConfig), 0o600); err != nil {
		t.Fatalf("write removed-key config: %v", err)
	}
	if !h.waitForDaemonLog("daemon.config_watch.parse_failed", 10*time.Second) {
		t.Fatal("removed semantic key was not rejected by the running daemon")
	}
	if currentWorker := h.latestWorkerPid(); currentWorker != workerAfter {
		t.Fatalf("worker pid changed after rejected config: %d to %d", workerAfter, currentWorker)
	}
	searchResponse := searchRunningDaemon(t, h, "still stored")
	if len(searchResponse.GetMatches()) != 1 {
		t.Fatalf("running search returned %d matches after config rejection, want 1", len(searchResponse.GetMatches()))
	}
	assertNoSemanticWrites(t, counter)
}

func searchRunningDaemon(t *testing.T, h *harness, query string) *clydev1.SearchConversationsResponse {
	t.Helper()

	target := "unix://" + filepath.Join(h.runtimeRoot, "clyde", "daemon.sock")
	connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("connect to running daemon: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response, err := clydev1.NewClydeServiceClient(connection).SearchConversations(ctx, &clydev1.SearchConversationsRequest{
		Query: query,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("search running daemon: %v", err)
	}
	return response
}

func waitForWorkerPIDChange(t *testing.T, h *harness, previous int) int {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current := h.latestWorkerPid()
		if current != 0 && current != previous {
			return current
		}
		time.Sleep(50 * time.Millisecond)
	}
	dump := h.dumpLogsOnFailure(t)
	t.Fatalf("worker pid did not change from %d after reload; logs dumped to %s", previous, dump)
	return 0
}

func assertNoSemanticWrites(t *testing.T, counter *semanticMethodCounter) {
	t.Helper()

	for _, method := range []string{
		lmsemanticsearchv1.SemanticSearchDaemonService_SyncConversationManifest_FullMethodName,
		lmsemanticsearchv1.SemanticSearchDaemonService_UpsertConversationDocumentsStream_FullMethodName,
	} {
		if calls := counter.count(method); calls != 0 {
			t.Fatalf("%s calls = %d, want 0", method, calls)
		}
	}
}
