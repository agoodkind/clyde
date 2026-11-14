package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/providers/claude/compaction"
	claudemitm "goodkind.io/clyde/internal/providers/claude/mitmcontrib"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
)

const (
	declaredSession = "headerless-compaction-session"
	declaredID      = "claude:" + declaredSession
	retainedMarker  = "CLYDE-499-RETAINED-TRANSCRIPT"
	upstreamSummary = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"<summary>upstream summary</summary>\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

type compactionProbeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type compactionProbeRequest struct {
	Model     string                   `json:"model"`
	MaxTokens int                      `json:"max_tokens"`
	Messages  []compactionProbeMessage `json:"messages"`
}

func TestHeaderlessClaudeCompactionRequiresDeclaration(t *testing.T) {
	configureInitialIndexTest(t, false)
	project := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "repo")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}

	transcript := fmt.Sprintf(
		`{"type":"user","sessionId":%q,"cwd":"/repo","uuid":"first","timestamp":"2026-11-13T00:00:00Z","message":{"role":"user","content":%q}}
{"type":"assistant","sessionId":%q,"cwd":"/repo","uuid":"last","timestamp":"2026-11-13T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}
`,
		declaredSession,
		strings.Repeat("older transcript text ", 200),
		declaredSession,
		retainedMarker,
	)
	if err := os.WriteFile(
		filepath.Join(project, "session.jsonl"),
		[]byte(transcript),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	cfg := config.NewConfigWithDefaults()
	cfg.Adapter.Enabled = false
	cfg.MITM.EnabledDefault = false
	cfg.Conversation.Semantic.IngestionEnabled = false
	cfg.Conversation.Semantic.SearchEnabled = false
	cfg.MITM.ReorientSummaryInjection = true
	cfg.MITM.ReorientInjectMaxTokens = 1024
	cfg.MITM.CA.CertPath = filepath.Join(t.TempDir(), "ca.pem")
	cfg.MITM.CA.KeyPath = filepath.Join(t.TempDir(), "ca.key")
	cfg.Daemon.GRPCAddress = "unix://" + conversationSearchSocketPath(t)
	index := conversation.NewIndex(registry, cfg.Conversation)
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	controlListener, err := daemonListener(t.Context(), cfg.Daemon.GRPCAddress, nil)
	if err != nil {
		t.Fatal(err)
	}

	var elapsed atomic.Int64
	runtime := &runtimeServices{
		listener: controlListener,
		group:    newLifecycleGroup(semanticTestLogger()),
	}

	runtime.declarations = startCompactionDeclarations(
		runtime.group,
		func() time.Time { return time.Unix(0, elapsed.Load()) },
	)
	t.Cleanup(func() { runtime.shutdown(context.Background()) })
	upstreamBodies := make(chan []byte, 32)
	upstream := httptest.NewUnstartedServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				t.Error(readErr)
			}

			upstreamBodies <- body
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, upstreamSummary)
		}),
	)
	if err := upstream.Listener.Close(); err != nil {
		t.Fatal(err)
	}

	upstream.Listener, err = net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}

	upstream.Start()
	t.Cleanup(upstream.Close)
	previousTransport := http.DefaultClient.Transport
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "api.anthropic.com:80" {
				return nil, fmt.Errorf("unexpected upstream address %q", address)
			}

			return new(net.Dialer).DialContext(ctx, network, upstream.Listener.Addr().String())
		},
	}

	http.DefaultClient.Transport = transport
	t.Cleanup(func() {
		http.DefaultClient.Transport = previousTransport
		transport.CloseIdleConnections()
	})
	t.Cleanup(func() { mitm.RegisterProvider(claudemitm.NewProvider(nil)) })
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}

	group := newLifecycleGroup(semanticTestLogger())
	proxy, err := mitm.NewProxy(
		cfg.MITM,
		cfg.Logging.Request,
		semanticTestLogger(),
		[]net.Listener{listener},
		nil,
		nil,
		"compaction-test",
		group,
	)
	if err != nil {
		t.Fatal(err)
	}

	proxy.SetRequestResponseHooks(mitmRequestResponseHooks(cfg))
	go func() { _ = proxy.Serve(t.Context()) }()
	t.Cleanup(func() {
		_ = proxy.ShutdownHTTP(context.Background())
		group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: time.Second})
	})
	grpcServer := grpc.NewServer()
	control := newControlServer(
		cfg,
		semanticTestLogger(),
		nil,
		index,
		nil,
		grpcServer,
		runtime,
		exportTokenConfig{},
	)
	clydev1.RegisterClydeServiceServer(grpcServer, control)
	go func() { _ = grpcServer.Serve(runtime.listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient(
		cfg.Daemon.GRPCAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = connection.Close() })
	client := clydev1.NewClydeServiceClient(connection)
	record, err := index.Resolve(t.Context(), declaredID)
	if err != nil {
		t.Fatal(err)
	}

	messages, err := index.LoadMessagesWithOptions(record, conversation.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	payload := compactionProbeRequest{Model: "claude-test", MaxTokens: 1024}

	for _, message := range messages {
		payload.Messages = append(
			payload.Messages,
			compactionProbeMessage{Role: message.Role, Content: message.Text},
		)
	}

	payload.Messages = append(
		payload.Messages,
		compactionProbeMessage{
			Role:    "user",
			Content: compaction.PromptSignature + " this conversation.",
		},
	)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	declare := func(selector string, synthetic bool) {
		t.Helper()
		request := &clydev1.ReorientConversationRequest{
			ConversationId:      selector,
			Workspace:           "/repo",
			SyntheticPrecompact: synthetic,
			PageBytes:           1024,
		}

		var recovered strings.Builder

		for {
			page, pageErr := client.ReorientConversation(t.Context(), request)
			if pageErr != nil {
				t.Fatal(pageErr)
			}

			recovered.Write(page.GetPageBody())
			if page.GetNextCursor() == "" {
				break
			}

			if runtime.declarations.Pending(declaredID) {
				t.Fatal("declaration preceded final page")
			}

			elapsed.Add(int64(time.Minute))
			request.Cursor = page.GetNextCursor()
		}

		if !strings.Contains(recovered.String(), retainedMarker) {
			t.Fatal("PreCompact RPC omitted real transcript marker")
		}
	}

	assertRequest := func(session string, class string, requestBody []byte, injected bool) {
		t.Helper()
		response, forwarded := sendCompactionProbe(
			t,
			"http://"+listener.Addr().String(),
			session,
			class,
			requestBody,
			upstreamBodies,
		)
		if injected {
			if !strings.Contains(string(response), "<pre-compaction-transcript>") ||
				!strings.Contains(string(response), retainedMarker) {
				t.Fatalf("summary omitted retained transcript: %s", response)
			}

			if bytes.Contains(forwarded, []byte(retainedMarker)) {
				t.Fatal("upstream received retained transcript marker")
			}

			return
		}

		if string(response) != upstreamSummary || !bytes.Equal(forwarded, requestBody) {
			t.Fatal("unmatched request or summary changed")
		}
	}

	assertRequest(declaredSession, "", body, false)
	declare("", true)
	assertRequest(declaredSession, "", body, false)
	declare(declaredID, false)
	assertRequest(declaredSession, "", body, false)
	declare(declaredID, true)

	for _, probe := range []struct {
		session, class string
		body           []byte
	}{
		{declaredSession, "", []byte(`{"messages":[{"role":"user","content":"ordinary turn"}]}`)},
		{"another-session", "", body},
		{"", "", body},
		{declaredSession, "main", body},
		{declaredSession, "", []byte(`{`)},
	} {
		assertRequest(probe.session, probe.class, probe.body, false)
		if !runtime.declarations.Pending(declaredID) {
			t.Fatal("unrelated request consumed declaration")
		}
	}

	assertRequest(declaredSession, "", body, true)
	assertRequest(declaredSession, "", body, false)
	assertRequest(declaredSession, "compaction", body, true)
	declare(declaredID, true)
	elapsed.Add(int64(60 * time.Second))
	assertRequest(declaredSession, "", body, false)
	declare(declaredID, true)
	var wait sync.WaitGroup
	var injectedCount atomic.Int64

	for i := 0; i < 2; i++ {
		wait.Go(func() {
			response, _ := sendCompactionProbe(
				t,
				"http://"+listener.Addr().String(),
				declaredSession,
				"",
				body,
				upstreamBodies,
			)
			if strings.Contains(string(response), retainedMarker) {
				injectedCount.Add(1)
			}
		})
	}

	wait.Wait()
	if injectedCount.Load() != 1 {
		t.Fatalf("concurrent injections = %d, want 1", injectedCount.Load())
	}

	cfg.MITM.ReorientSummaryInjection = false
	control.declareCompaction = newControlServer(
		cfg,
		semanticTestLogger(),
		nil,
		index,
		nil,
		grpcServer,
		runtime,
		exportTokenConfig{},
	).declareCompaction
	proxy.SetRequestResponseHooks(mitmRequestResponseHooks(cfg))
	assertRequest(declaredSession, "", body, false)
	declare(declaredID, true)
	if runtime.declarations.Pending(declaredID) {
		t.Fatal("disabled injection authorized compaction")
	}

	assertRequest(declaredSession, "compaction", body, false)
	cfg.MITM.ReorientSummaryInjection = true
	control.declareCompaction = runtime.declarations.Declare
	proxy.SetRequestResponseHooks(mitmRequestResponseHooks(cfg))
	declare(declaredID, true)
	runtime.group.Quiesce(t.Context(), "reload", budgetShutdown)
	assertRequest(declaredSession, "", body, false)
	declare(declaredID, true)
	assertRequest(declaredSession, "", body, false)
}

func sendCompactionProbe(
	t *testing.T,
	baseURL string,
	session string,
	class string,
	body []byte,
	upstreamBodies <-chan []byte,
) ([]byte, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"http://api.anthropic.com/v1/messages",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json")
	if session != "" {
		request.Header.Set("x-claude-code-session-id", session)
	}

	if class != "" {
		request.Header.Set("x-claude-code-request-class", class)
	}

	proxyURL, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}

	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != http.StatusOK {
		t.Fatalf("proxy HTTP status = %d: %s", response.StatusCode, result)
	}

	return result, <-upstreamBodies
}
