package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

type statusCountingParser struct {
	path        string
	discoveries atomic.Int64
	loads       atomic.Int64
}

func (*statusCountingParser) Provider() providerid.Provider { return providerid.ProviderCodex }

func (p *statusCountingParser) Discover(context.Context, map[string]conversation.Record) ([]conversation.ScanCandidate, error) {
	p.discoveries.Add(1)
	return []conversation.ScanCandidate{{Path: p.path, Stamp: conversation.FileStamp{Size: 1, Mtime: time.Unix(1, 0)}}}, nil
}

func (p *statusCountingParser) ScanRecord(string, conversation.FileStamp) (conversation.Record, bool) {
	return conversation.Record{ID: "codex:status-fixture", NativeID: "status-fixture", Provider: p.Provider(), ArtifactPath: p.path, UpdatedAt: time.Unix(1, 0)}, true
}

func (p *statusCountingParser) Stream(string, conversation.LoadOptions) iter.Seq2[transcript.Message, error] {
	p.loads.Add(1)
	return func(yield func(transcript.Message, error) bool) {
		yield(transcript.Message{Role: "user", Text: "status fixture"}, nil)
	}
}

func TestDaemonStatusCommandReadsPassiveRuntime(t *testing.T) {
	buildStarted := time.Now()
	binary := buildConversationSearchCLI(t)
	info, err := os.Stat(binary)
	if err != nil || info.ModTime().Before(buildStarted.Truncate(time.Second)) {
		t.Fatalf("fresh CLI build: %v, %v", info, err)
	}
	for _, directions := range []struct{ ingestion, search bool }{{false, false}, {true, true}, {true, false}, {false, true}} {
		t.Run(fmt.Sprintf("ingestion_%t_search_%t", directions.ingestion, directions.search), func(t *testing.T) {
			enabled := directions.ingestion || directions.search
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			cfg := config.NewConfig()
			cfg.Conversation.Semantic.IngestionEnabled = directions.ingestion
			cfg.Conversation.Semantic.SearchEnabled = directions.search
			artifact := filepath.Join(t.TempDir(), "conversation.jsonl")
			if err := os.WriteFile(artifact, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			parser := &statusCountingParser{path: artifact}
			registry := conversation.NewRegistry()
			registry.Register(parser)
			index := conversation.NewIndex(registry, cfg.Conversation)
			if err := index.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			beforeDiscovery := parser.discoveries.Load()
			if beforeDiscovery != 1 {
				t.Fatalf("discovery counter did not observe initial scan: %d", beforeDiscovery)
			}
			record, _ := parser.ScanRecord(artifact, conversation.FileStamp{})
			if _, err := index.LoadMessagesWithOptions(record, conversation.LoadOptions{}); err != nil {
				t.Fatal(err)
			}
			if parser.loads.Load() != 1 {
				t.Fatal("transcript counter did not observe fixture load")
			}
			socket := conversationSearchSocketPath(t)
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			runtime := &runtimeServices{listener: listener}
			runtime.currentConfig.Store(cfg)
			profilingText := "profiling: disabled"
			if directions.search && !directions.ingestion {
				profiling, err := net.Listen("tcp", "[::1]:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = profiling.Close() })
				runtime.pprofListener = profiling
				profilingText = "profiling: address=" + profiling.Addr().String()
			}
			var attempts atomic.Int64
			if enabled {
				connector := semsearchConnector(filepath.Join(t.TempDir(), "missing.sock"), "status-test")
				semantic, group := newTestSemanticRuntime(t, func(ctx context.Context) (semanticConnection, error) {
					attempts.Add(1)
					return connector(ctx)
				})
				semantic.retryDelay = func(uint32) time.Duration { return time.Hour }
				if err := semantic.attemptRegister(t.Context()); err == nil {
					t.Fatal("missing engine registered")
				}
				semantic.startRetryWorker(t.Context(), group)
				runtime.semantic = semantic
				t.Cleanup(func() { group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: time.Second}) })
			}
			grpcServer := grpc.NewServer()
			server := newControlServer(cfg, semanticTestLogger(), nil, index, nil, grpcServer, runtime, exportTokenConfig{})
			clydev1.RegisterClydeServiceServer(grpcServer, server)
			go func() { _ = grpcServer.Serve(listener) }()
			t.Cleanup(grpcServer.Stop)
			configRoot := t.TempDir()
			configDir := filepath.Join(configRoot, "clyde")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			// Deliberately disagree with the daemon's effective configuration.
			body := fmt.Sprintf("[daemon]\ngrpc_address = %q\n[conversation.semantic]\ningestion_enabled = %t\nsearch_enabled = %t\n", "unix://"+socket, !directions.ingestion, !directions.search)
			if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				command := exec.CommandContext(t.Context(), binary, "daemon", "status")
				command.Env = environmentWithOverrides("HOME="+t.TempDir(), "XDG_CONFIG_HOME="+configRoot, "XDG_RUNTIME_DIR="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir(), "CLYDE_DEBUG_PPROF_ADDR=[::1]:9999")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("status: %v\n%s", err, output)
				}
				state := "disabled"
				if enabled {
					state = "unavailable"
				}
				for _, want := range []string{fmt.Sprintf("ingestion_enabled=%t search_enabled=%t", directions.ingestion, directions.search), "connection=" + state, profilingText, "address=" + socket} {
					if !strings.Contains(string(output), want) {
						t.Errorf("status missing %q:\n%s", want, output)
					}
				}
				// JSON must carry the same daemon-owned flags as the text view.
				command = exec.CommandContext(t.Context(), binary, "daemon", "status", "--output-format", "json")
				command.Env = environmentWithOverrides("HOME="+t.TempDir(), "XDG_CONFIG_HOME="+configRoot, "XDG_RUNTIME_DIR="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
				output, err = command.CombinedOutput()
				if err != nil {
					t.Fatalf("JSON status: %v\n%s", err, output)
				}
				var decoded struct {
					Runtime *RuntimeStatus `json:"runtime"`
				}
				if err := json.Unmarshal(output, &decoded); err != nil {
					t.Fatalf("decode status: %v\n%s", err, output)
				}
				if decoded.Runtime == nil || decoded.Runtime.Semantic.IngestionEnabled != directions.ingestion || decoded.Runtime.Semantic.SearchEnabled != directions.search || string(decoded.Runtime.Semantic.Connection) != state {
					t.Fatalf("JSON status disagrees: %s", output)
				}
			}
			historyCommand := exec.CommandContext(t.Context(), binary, "daemon", "status", "--since", "1h", "--output-format", "json")
			historyCommand.Env = environmentWithOverrides("HOME="+t.TempDir(), "XDG_CONFIG_HOME="+configRoot, "XDG_RUNTIME_DIR="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
			history, err := historyCommand.CombinedOutput()
			if err != nil {
				t.Fatalf("historical status: %v\n%s", err, history)
			}
			var historical struct {
				Metadata struct {
					TraceID string `json:"trace_id"`
					SpanID  string `json:"span_id"`
				} `json:"_meta"`
				Window                 MetricsWindow        `json:"window"`
				Coverage               MetricsCoverage      `json:"coverage"`
				Metrics                MetricsValues        `json:"metrics"`
				TimeBreakdown          MetricsTimeBreakdown `json:"time_breakdown"`
				UnattributedDurationMS *int64               `json:"unattributed_duration_ms"`
				Warnings               []string             `json:"warnings"`
			}
			decoder := json.NewDecoder(bytes.NewReader(history))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&historical); err != nil {
				t.Fatalf("historical contract changed: %v\n%s", err, history)
			}
			if historical.Window.Until.Sub(historical.Window.Since) != time.Hour {
				t.Fatalf("historical window: %v", historical.Window)
			}
			conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			client := clydev1.NewClydeServiceClient(conn)
			var nextRetry int64
			for i := 0; i < 10; i++ {
				response, err := client.GetDaemonStatus(t.Context(), &emptypb.Empty{})
				if err != nil {
					t.Fatal(err)
				}
				semantic := response.GetSemantic()
				if i == 0 {
					nextRetry = semantic.GetNextRetryUnix()
				}
				if semantic.GetNextRetryUnix() != nextRetry || semantic.GetAttempts() != uint64(attempts.Load()) {
					t.Fatalf("control status changed retry state: %v", semantic)
				}
				if enabled && nextRetry <= time.Now().Unix() {
					t.Fatalf("missing engine has no future retry: %v", semantic)
				}
				if !enabled && nextRetry != 0 {
					t.Fatalf("disabled engine has a retry: %v", semantic)
				}
			}
			wantAttempts := int64(0)
			if enabled {
				wantAttempts = 1
			}
			if attempts.Load() != wantAttempts || parser.discoveries.Load() != beforeDiscovery || parser.loads.Load() != 1 {
				t.Fatalf("status caused work: attempts=%d discovery=%d loads=%d", attempts.Load(), parser.discoveries.Load(), parser.loads.Load())
			}
		})
	}
}
