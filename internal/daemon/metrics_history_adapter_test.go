package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
)

func TestAdapterHandlerIOLogFeedsMetricsHistory(t *testing.T) {
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	cfg := config.NewConfigWithDefaults()
	cfg.Adapter.ClientIdentity = config.AdapterClientIdentity{
		SystemPromptPrefix: "test", StainlessPackageVersion: "test", StainlessRuntime: "go",
		StainlessRuntimeVersion: "test", CCVersion: "test", CCEntrypoint: "test",
	}
	server, err := adapter.New(context.Background(), cfg.Adapter, cfg.Logging, adapter.Deps{}, logger)
	if err != nil {
		t.Fatalf("adapter.New: %v", err)
	}
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.StartOnListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case serveErr := <-serveDone:
			if serveErr != nil {
				t.Errorf("serve: %v", serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("adapter server did not stop")
		}
	})

	request, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/healthz", bytes.NewReader([]byte("unread")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set(clydeingress.HeaderRequestID, "history-io")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}
	executionID := adapterIOExecutionID(t, logBuffer.Bytes())

	now := time.Now().UTC()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open history log: %v", err)
	}
	if _, err := file.Write(logBuffer.Bytes()); err != nil {
		t.Fatalf("write adapter log: %v", err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range []metricsHistoryRecord{
		{Time: now.Add(-time.Second).Format(time.RFC3339Nano), Message: "adapter.request.started", RequestID: "history-io", ExecutionID: executionID},
		{Time: now.Format(time.RFC3339Nano), Message: "adapter.request.completed", RequestID: "history-io", ExecutionID: executionID},
	} {
		if err := encoder.Encode(record); err != nil {
			t.Fatalf("append lifecycle record: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close history log: %v", err)
	}

	report := BuildMetricsHistory(MetricsHistoryInput{
		Since: now.Add(-time.Minute), Now: now.Add(time.Minute), LogPath: logPath,
	})
	if metricInt(report.Metrics.BytesIn.Delta) != 0 || metricInt(report.Metrics.BytesOut.Delta) != int64(len(body)) {
		t.Fatalf("reported bytes = in %v out %v, want 0 and %d", report.Metrics.BytesIn.Delta, report.Metrics.BytesOut.Delta, len(body))
	}
}

func adapterIOExecutionID(t *testing.T, logs []byte) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(logs))
	for {
		var record struct {
			Message     string `json:"msg"`
			ExecutionID string `json:"execution_id"`
		}
		err := decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode adapter log: %v", err)
		}
		if record.Message == "adapter.request.io" && record.ExecutionID != "" {
			return record.ExecutionID
		}
	}
	t.Fatal("adapter.request.io execution_id missing")
	return ""
}

func TestAdapterHandlerHistorySeparatesRepeatedExternalRequestID(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-upstream","object":"chat.completion","model":"local-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	upstreamServer := &http.Server{Handler: upstream}
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	go func() { _ = upstreamServer.Serve(upstreamListener) }()
	t.Cleanup(func() { _ = upstreamServer.Close() })

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	cfg := config.NewConfigWithDefaults()
	cfg.Adapter.Enabled = true
	cfg.Adapter.ClientIdentity = config.AdapterClientIdentity{
		SystemPromptPrefix: "test", StainlessPackageVersion: "test", StainlessRuntime: "go",
		StainlessRuntimeVersion: "test", CCVersion: "test", CCEntrypoint: "test",
	}
	cfg.Adapter.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{
		BaseURL: "http://" + upstreamListener.Addr().String() + "/v1",
	}
	server, err := adapter.New(context.Background(), cfg.Adapter, cfg.Logging, adapter.Deps{}, logger)
	if err != nil {
		t.Fatalf("adapter.New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen adapter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.StartOnListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		<-serveDone
	})

	windowStart := time.Now().UTC().Add(-time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	for range 2 {
		request, requestErr := http.NewRequest(
			http.MethodPost,
			"http://"+listener.Addr().String()+"/v1/chat/completions",
			bytes.NewBufferString(`{"model":"local-model","messages":[{"role":"user","content":"hello"}]}`),
		)
		if requestErr != nil {
			t.Fatalf("NewRequest: %v", requestErr)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(clydeingress.HeaderRequestID, "reused-caller-id")
		response, requestErr := client.Do(request)
		if requestErr != nil {
			t.Fatalf("chat request: %v", requestErr)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
	}
	windowEnd := time.Now().UTC().Add(time.Second)
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	file, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create history log: %v", err)
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(metricsHistoryRecord{Time: windowStart.Format(time.RFC3339Nano), Message: "daemon.health"}); err != nil {
		t.Fatalf("encode history boundary: %v", err)
	}
	if _, err := file.Write(logBuffer.Bytes()); err != nil {
		t.Fatalf("write adapter log: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close history log: %v", err)
	}

	report := BuildMetricsHistory(MetricsHistoryInput{Since: windowStart, Now: windowEnd, LogPath: logPath})
	ApplyCurrentProviderSnapshot(&report, nil, windowStart.Add(-time.Second))
	if !report.Coverage.Complete {
		t.Fatalf("coverage = incomplete warnings=%q, want complete", report.Warnings)
	}
	if metricInt(report.Metrics.Requests.Delta) != 2 {
		t.Fatalf("request delta = %v, want 2", report.Metrics.Requests.Delta)
	}
}
