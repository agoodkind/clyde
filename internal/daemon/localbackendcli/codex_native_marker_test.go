package localbackendcli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/gklog/correlation"
)

const (
	nativeMarkerBinaryEnv     = "CLYDE_NATIVE_CODEX_BINARY"
	nativeMarkerTimeout       = 3 * time.Minute
	nativeMarkerWait          = 60 * time.Second
	nativeMarkerCompactCipher = "clyde-native-compaction-state"
	nativeMarkerAuth          = `{"auth_mode":"apikey","OPENAI_API_KEY":"clyde-native-marker-test"}`
	nativeMarkerConfig        = "model_provider = \"openai\"\nmodel = \"gpt-6.1-sol\"\nmodel_context_window = 2000\nopenai_base_url = \"%s/v1\"\n"
	nativeMarkerResponsesPath = "/v1/responses"
	nativeMarkerOutputError   = "OutputTextDelta without active item"
	nativeMarkerFirstPrompt   = "first native question"
	nativeMarkerToolPrompt    = "second native question that requires a tool"
	nativeMarkerReloadPrompt  = "third native question after reload"
	nativeMarkerToolText      = "NATIVE_MARKER_TOOL_SENTINEL"

	nativeMarkerRetainedCommand   = `{"cmd":"echo ` + nativeMarkerToolText + `"}`
	nativeMarkerCompactionCommand = `{"cmd":"seq 1 1500"}`
	nativeMarkerRetainedCallID    = "call-native-retained"
	nativeMarkerCompactionCallID  = "call-native-compaction"

	nativeMarkerAllFile     = "native-all-export.txt"
	nativeMarkerRecoveredID = "msg-native-recovered"
	nativeMarkerRecovered   = "native marker recovered final answer"
	nativeMarkerSmallUsage  = `{"input_tokens":10,"output_tokens":5,"total_tokens":15}`
	nativeMarkerLargeUsage  = `{"input_tokens":1900,"output_tokens":50,"total_tokens":1950}`
)

type nativeMarkerUpstream struct {
	t             *testing.T
	mutex         sync.Mutex
	registry      *adaptercodex.RawResponsesCompactionV2Registry
	requests      []nativeMarkerRequest
	upgradeCount  int
	injection     string
	recoveryCount int

	retainedCallSent   bool
	compactionCallSent bool
}

type nativeMarkerRequest struct {
	metadata     nativeMarkerMetadata
	metadataText string
	input        []json.RawMessage
	text         string
}

type nativeMarkerMetadata struct {
	RequestKind string `json:"request_kind"`
	Compaction  struct {
		Implementation string `json:"implementation"`
		Phase          string `json:"phase"`
	} `json:"compaction"`
}

type nativeMarkerItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (u *nativeMarkerUpstream) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		if request.URL.Path == nativeMarkerResponsesPath {
			u.mutex.Lock()
			u.upgradeCount++
			u.mutex.Unlock()
			writer.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		http.NotFound(writer, request)
		return
	}
	body, err := nativeMarkerRequestBody(request)
	if err != nil {
		u.t.Errorf("The test could not decode the native request: %v", err)
		http.Error(writer, "decode", http.StatusBadRequest)
		return
	}
	header := request.Header.Clone()
	metadataText := header.Get(adaptercodex.CodexTurnMetadataHeader)
	var metadata nativeMarkerMetadata
	if err := json.Unmarshal([]byte(metadataText), &metadata); err != nil {
		u.t.Errorf("The test could not decode native turn metadata %q: %v", metadataText, err)
	}
	var decoded struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		u.t.Errorf("The test could not decode the native input: %v", err)
	}
	u.mutex.Lock()
	u.requests = append(u.requests, nativeMarkerRequest{metadata: metadata, metadataText: metadataText, input: decoded.Input, text: string(body)})
	u.mutex.Unlock()

	raw := adaptercodex.RawResponsesRequest{Body: body, Header: header, RequestID: "", Correlation: correlation.Context{}, Stream: true}
	payload := u.responseBody(raw, metadata)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write(payload); err != nil {
		u.t.Errorf("The test could not write the native response: %v", err)
	}
}

// responseBody returns a compaction stream for v2 compaction requests and a
// recovery stream when recovery injection changes another request. Otherwise,
// it returns a tool call for the first request with nativeMarkerToolPrompt. It
// returns an answer stream for later or unmatched requests.
func (u *nativeMarkerUpstream) responseBody(raw adaptercodex.RawResponsesRequest, metadata nativeMarkerMetadata) []byte {
	if adaptercodex.DetectRawResponsesCompactionProtocol(raw.Header) == adaptercodex.RawResponsesCompactionV2 {
		return u.compactionBody(raw)
	}
	injected, recovery, changed := adaptercodex.InjectRawResponsesCompactionV2Recovery(raw, u.registry)
	if changed {
		return u.recoveryBody(injected, recovery)
	}
	if bytes.Contains(raw.Body, []byte(nativeMarkerToolPrompt)) {
		if !u.takeToolCall(&u.compactionCallSent) {
			return []byte(nativeMarkerToolCallStream(nativeMarkerCompactionCallID, nativeMarkerCompactionCommand, nativeMarkerLargeUsage))
		}
	} else if !u.takeToolCall(&u.retainedCallSent) {
		return []byte(nativeMarkerToolCallStream(nativeMarkerRetainedCallID, nativeMarkerRetainedCommand, nativeMarkerSmallUsage))
	}
	return []byte(nativeMarkerAnswerStream(u.t, fmt.Sprintf("msg-native-answer-%d", u.requestCount()), nativeMarkerAnswerText(raw.Body), nativeMarkerSmallUsage))
}

func nativeMarkerAnswerText(body []byte) string {
	if bytes.Contains(body, []byte(nativeMarkerReloadPrompt)) {
		return codexProbeFramedQuotation
	}
	return "native marker earlier answer"
}

func (u *nativeMarkerUpstream) takeToolCall(sent *bool) bool {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	alreadySent := *sent
	*sent = true
	return alreadySent
}

func (u *nativeMarkerUpstream) requestCount() int {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	return len(u.requests)
}

func (u *nativeMarkerUpstream) compactionBody(raw adaptercodex.RawResponsesRequest) []byte {
	plan, ok := adaptercodex.PlanRawResponsesCompactionV2(raw, codexProbeSplitter())
	if !ok {
		u.t.Errorf("PlanRawResponsesCompactionV2 rejected the native request: %s", raw.Header.Get(adaptercodex.CodexTurnMetadataHeader))
		return []byte(nativeMarkerCompactionStream())
	}
	observed := adaptercodex.ObserveRawResponsesCompactionV2Response(nativeMarkerStreamResponse(nativeMarkerCompactionStream()), plan, u.registry)
	body, err := io.ReadAll(observed.Body)
	if err != nil {
		u.t.Errorf("The test could not read the observed compaction response: %v", err)
	}
	adaptercodex.ArmRawResponsesCompactionV2Response(observed)
	u.mutex.Lock()
	u.injection = plan.Injection
	u.mutex.Unlock()
	return body
}

func (u *nativeMarkerUpstream) recoveryBody(injected adaptercodex.RawResponsesRequest, recovery *adaptercodex.RawResponsesCompactionV2Recovery) []byte {
	transformer := adaptercodex.NewRawResponsesCompactionV2FinalAnswerTransformer(injected, recovery)
	stream := nativeMarkerAnswerStream(u.t, nativeMarkerRecoveredID, nativeMarkerRecovered, nativeMarkerSmallUsage)
	if transformer == nil {
		recovery.ReleaseRecovery()
		u.t.Errorf("NewRawResponsesCompactionV2FinalAnswerTransformer rejected the recovery request: %s", injected.Header.Get(adaptercodex.CodexTurnMetadataHeader))
		return []byte(stream)
	}
	response := transformer.TransformResponse(nativeMarkerStreamResponse(stream))
	body, err := io.ReadAll(response.Body)
	if err != nil {
		u.t.Errorf("The test could not read the recovered response: %v", err)
	}
	if !transformer.DidMutateResponse() {
		recovery.ReleaseRecovery()
		u.t.Errorf("The final answer transformer did not insert the transcript:\n%s", body)
		return body
	}
	recovery.CompleteRecovery()
	u.mutex.Lock()
	u.recoveryCount++
	u.mutex.Unlock()
	return body
}

func (u *nativeMarkerUpstream) lastRequest(t *testing.T, label string) nativeMarkerRequest {
	t.Helper()
	u.mutex.Lock()
	defer u.mutex.Unlock()
	if len(u.requests) == 0 {
		t.Fatalf("The native client did not send a request for %s", label)
	}
	return u.requests[len(u.requests)-1]
}

func (u *nativeMarkerUpstream) requestShapes(t *testing.T) string {
	t.Helper()
	u.mutex.Lock()
	defer u.mutex.Unlock()
	var shapes strings.Builder
	for index, request := range u.requests {
		fmt.Fprintf(&shapes, "request %d metadata=%s\n", index+1, request.metadataText)
		for _, raw := range request.input {
			var item nativeMarkerItem
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatalf("The test could not decode the request item: %v", err)
			}
			fmt.Fprintf(&shapes, "  %s %s parts=%d\n", item.Type, item.Role, len(item.Content))
		}
	}
	return shapes.String()
}

func nativeMarkerRequestBody(request *http.Request) ([]byte, error) {
	encoded, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, fmt.Errorf("the test could not read the body: %w", err)
	}
	if request.Header.Get("Content-Encoding") != "zstd" {
		return encoded, nil
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("the test could not create the decoder: %w", err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(encoded, nil)
	if err != nil {
		return nil, fmt.Errorf("the decoder could not decode the body: %w", err)
	}
	return decoded, nil
}

func nativeMarkerFrame(eventType string, sequence int, fields string) string {
	return "event: " + eventType + "\ndata: " +
		fmt.Sprintf(`{"type":%q,"sequence_number":%d,%s}`, eventType, sequence, fields) + "\n\n"
}

func nativeMarkerAnswerStream(t *testing.T, itemID string, text string, usage string) string {
	t.Helper()
	encodedText := injectedProbeJSON(t, text)
	target := fmt.Sprintf(`"item_id":%q,"output_index":0,"content_index":0`, itemID)
	empty := `{"type":"output_text","annotations":[],"logprobs":[],"text":""}`
	part := `{"type":"output_text","annotations":[],"logprobs":[],"text":` + encodedText + `}`
	pending := `{"id":"` + itemID + `","type":"message","role":"assistant","status":"in_progress","phase":"final_answer","content":[]}`
	done := `{"id":"` + itemID + `","type":"message","role":"assistant","status":"completed","phase":"final_answer","content":[` + part + `]}`
	response := `"id":"resp-` + itemID + `","status":"completed","output":[` + done + `],"usage":` + usage
	return strings.Join([]string{
		nativeMarkerFrame("response.created", 1, `"response":{"id":"resp-`+itemID+`","status":"in_progress","output":[]}`),
		nativeMarkerFrame("response.output_item.added", 2, `"output_index":0,"item":`+pending),
		nativeMarkerFrame("response.content_part.added", 3, target+`,"part":`+empty),
		nativeMarkerFrame("response.output_text.delta", 4, target+`,"logprobs":[],"delta":`+encodedText),
		nativeMarkerFrame("response.output_text.done", 5, target+`,"logprobs":[],"text":`+encodedText),
		nativeMarkerFrame("response.content_part.done", 6, target+`,"part":`+part),
		nativeMarkerFrame("response.output_item.done", 7, `"output_index":0,"item":`+done),
		nativeMarkerFrame("response.completed", 8, `"response":{`+response+`}`),
	}, "")
}

func nativeMarkerToolCallStream(callID string, command string, usage string) string {
	arguments, _ := json.Marshal(command)
	item := `{"id":"fc-` + callID + `","type":"function_call","status":"completed","call_id":"` + callID + `","name":"exec_command","arguments":` + string(arguments) + `}`
	return nativeMarkerFrame("response.output_item.done", 1, `"output_index":0,"item":`+item) +
		nativeMarkerFrame("response.completed", 2, `"response":{"id":"resp-`+callID+`","status":"completed","output":[`+item+`],"usage":`+usage+`}`)
}

func nativeMarkerCompactionStream() string {
	item := `{"type":"compaction","encrypted_content":"` + nativeMarkerCompactCipher + `"}`
	return nativeMarkerFrame("response.output_item.done", 1, `"output_index":0,"item":`+item) +
		nativeMarkerFrame("response.completed", 2, `"response":{"id":"resp-native-compaction","status":"completed","output":[],"usage":`+nativeMarkerSmallUsage+`}`)
}

func nativeMarkerStreamResponse(stream string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
}

type nativeMarkerServer struct {
	t             *testing.T
	command       *exec.Cmd
	stdin         io.WriteCloser
	stderr        *localProofBuffer
	mutex         sync.Mutex
	nextID        int
	pending       map[int]chan json.RawMessage
	notifications chan json.RawMessage
	stopOnce      sync.Once
}

func startNativeMarkerServer(ctx context.Context, t *testing.T, binary string, home string) *nativeMarkerServer {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
	command.Env = append(localProofProviderEnvironment(t, home), "PATH="+os.Getenv("PATH"))
	command.Dir = home
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("The test could not open native stdin: %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("The test could not open native stdout: %v", err)
	}
	stderr := &localProofBuffer{mu: sync.Mutex{}, buffer: bytes.Buffer{}}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("The test could not start the native app server: %v", err)
	}
	server := &nativeMarkerServer{
		t:             t,
		command:       command,
		stdin:         stdin,
		stderr:        stderr,
		mutex:         sync.Mutex{},
		nextID:        0,
		pending:       make(map[int]chan json.RawMessage),
		notifications: make(chan json.RawMessage, 4096),
		stopOnce:      sync.Once{},
	}
	t.Cleanup(server.stop)
	go server.read(stdout)
	server.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "clyde_native_marker_test", "title": "Clyde marker test", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if _, err := stdin.Write([]byte(`{"method":"initialized","params":{}}` + "\n")); err != nil {
		t.Fatalf("The test could not write the initialized notification: %v", err)
	}
	return server
}

func (s *nativeMarkerServer) stop() {
	s.stopOnce.Do(func() {
		if err := s.stdin.Close(); err != nil {
			s.t.Logf("The test could not close native stdin: %v", err)
		}
		if err := s.command.Process.Kill(); err != nil {
			s.t.Logf("The test could not stop the native app server: %v", err)
		}
		if err := s.command.Wait(); err != nil {
			s.t.Logf("The native app server exited: %v", err)
		}
		if strings.Contains(s.stderr.String(), nativeMarkerOutputError) {
			s.t.Errorf("The native client rejected the stream event order:\n%s", s.stderr.String())
		}
	})
}

func (s *nativeMarkerServer) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		var message struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &message) != nil {
			continue
		}
		if message.ID == nil {
			select {
			case s.notifications <- line:
			default:
			}
			continue
		}
		if message.Method != "" {
			continue
		}
		s.mutex.Lock()
		reply, ok := s.pending[*message.ID]
		delete(s.pending, *message.ID)
		s.mutex.Unlock()
		if ok {
			reply <- line
		}
	}
}

func (s *nativeMarkerServer) call(method string, params map[string]any) json.RawMessage {
	s.t.Helper()
	s.mutex.Lock()
	s.nextID++
	id := s.nextID
	reply := make(chan json.RawMessage, 1)
	s.pending[id] = reply
	s.mutex.Unlock()
	encoded, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		s.t.Fatalf("The test could not encode %s: %v", method, err)
	}
	if _, err := s.stdin.Write(append(encoded, '\n')); err != nil {
		s.t.Fatalf("The test could not write %s: %v", method, err)
	}
	select {
	case line := <-reply:
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil || len(response.Error) > 0 {
			s.t.Fatalf("The native client could not complete %s: %v %s", method, err, line)
		}
		return response.Result
	case <-time.After(nativeMarkerWait):
		s.t.Fatalf("The %s call did not return within %s", method, nativeMarkerWait)
	}
	return nil
}

func (s *nativeMarkerServer) waitTurn(label string) {
	s.t.Helper()
	deadline := time.After(nativeMarkerWait)
	for {
		select {
		case line := <-s.notifications:
			var notification struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(line, &notification); err != nil {
				s.t.Fatalf("The test could not decode the notification: %v", err)
			}
			if notification.Method == "error" {
				s.t.Errorf("The native client reported an error for %s: %s", label, line)
			}
			if notification.Method == "turn/completed" {
				return
			}
		case <-deadline:
			s.t.Fatalf("The %s call did not finish within %s.\nThe native app server wrote to stderr:\n%s", label, nativeMarkerWait, s.stderr.String())
		}
	}
}

func (s *nativeMarkerServer) turn(threadID string, text string) {
	s.t.Helper()
	s.call("turn/start", map[string]any{"threadId": threadID, "input": []map[string]any{{"type": "text", "text": text}}})
	s.waitTurn(text)
}

// nativeMarkerCopies collects assistant messages with multiple content parts
// that contain the exact injection text. It checks that each collected message
// has the recovered answer at index 0 and the transcript at index 1, with only
// index 1 marked.
func nativeMarkerCopies(t *testing.T, label string, items []json.RawMessage, injection string) []nativeMarkerItem {
	t.Helper()
	copies := make([]nativeMarkerItem, 0, 1)
	for _, raw := range items {
		var item nativeMarkerItem
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("The test could not decode the item for %s: %v", label, err)
		}
		if item.Type != "message" || !nativeMarkerItemHasText(item, injection) {
			continue
		}
		if item.Role != "assistant" {
			continue
		}
		if len(item.Content) == 1 {
			continue
		}
		if indexes := codexstore.InjectedTranscriptIndexes(raw); !slices.Equal(indexes, []int{1}) {
			t.Errorf("The test found unexpected marked indexes for %s: %v in %s", label, indexes, raw)
		}
		if len(item.Content) != 2 || item.Content[0].Text != nativeMarkerRecovered || item.Content[1].Text != injection {
			t.Errorf("The content parts for %s changed: %s", label, raw)
		}
		copies = append(copies, item)
	}
	return copies
}

func nativeMarkerItemHasText(item nativeMarkerItem, text string) bool {
	for _, part := range item.Content {
		if part.Text == text {
			return true
		}
	}
	return false
}

func readNativeMarkerResponseItems(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("The test could not read the native rollout: %v", err)
	}
	items := make([]json.RawMessage, 0)
	for line := range bytes.SplitSeq(bytes.TrimSpace(body), []byte("\n")) {
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("The test could not decode the native rollout line: %v", err)
		}
		if record.Type == "response_item" {
			items = append(items, record.Payload)
		}
	}
	return items
}

func requireNativeMarkerCopies(t *testing.T, label string, items []json.RawMessage, injection string, want int) []nativeMarkerItem {
	t.Helper()
	copies := nativeMarkerCopies(t, label, items, injection)
	if len(copies) != want {
		t.Fatalf("The test counted marked transcript copies for %s: %d instead of %d", label, len(copies), want)
	}
	return copies
}

// TestNativeCodexKeepsMarkedTranscriptPart runs when CLYDE_NATIVE_CODEX_BINARY
// has a value. It checks that native Codex preserves separate answer and
// marked transcript parts in its rollout and request after restarting and
// resuming the thread. It also checks CLI and MCP content filtering and
// quotation preservation without changing the rollout.
func TestNativeCodexKeepsMarkedTranscriptPart(t *testing.T) {
	binary := os.Getenv(nativeMarkerBinaryEnv)
	if binary == "" {
		t.Skipf("The native Codex marker test requires a value for %s", nativeMarkerBinaryEnv)
	}
	upstream := &nativeMarkerUpstream{
		t:             t,
		mutex:         sync.Mutex{},
		registry:      adaptercodex.NewRawResponsesCompactionV2Registry(nil),
		requests:      nil,
		upgradeCount:  0,
		injection:     "",
		recoveryCount: 0,

		retainedCallSent:   false,
		compactionCallSent: false,
	}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("The test could not resolve the native home directory: %v", err)
	}
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("The test could not create the native home directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(nativeMarkerAuth), 0o600); err != nil {
		t.Fatalf("The test could not write the native auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), fmt.Appendf(nil, nativeMarkerConfig, server.URL), 0o600); err != nil {
		t.Fatalf("The test could not write the native config: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), nativeMarkerTimeout)
	t.Cleanup(cancel)

	native := startNativeMarkerServer(ctx, t, binary, home)
	started := native.call("thread/start", map[string]any{
		"cwd": home, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": false, "historyMode": "legacy",
	})
	var thread struct {
		Thread struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(started, &thread); err != nil {
		t.Fatalf("The test could not decode thread/start: %v", err)
	}
	threadID := thread.Thread.ID
	native.turn(threadID, nativeMarkerFirstPrompt)
	native.turn(threadID, nativeMarkerToolPrompt)
	native.stop()

	upstream.mutex.Lock()
	injection := upstream.injection
	recoveryCount := upstream.recoveryCount
	upgradeCount := upstream.upgradeCount
	upstream.mutex.Unlock()
	if recoveryCount != 1 || injection == "" {
		t.Fatalf("The test counted %d recoveries and %d injection bytes but expected one recovery:\n%s", recoveryCount, len(injection), upstream.requestShapes(t))
	}
	if upgradeCount == 0 {
		t.Fatal("The native client did not attempt the WebSocket upgrade that the test server rejects with HTTP status 426 (Upgrade Required).")
	}
	if len(reorienttag.ParseTranscriptSpans(injection)) == 0 {
		t.Fatalf("PlanRawResponsesCompactionV2 did not produce a framed transcript:\n%s", injection)
	}
	savedCopies := requireNativeMarkerCopies(t, "saved response item", readNativeMarkerResponseItems(t, thread.Thread.Path), injection, 1)
	if savedCopies[0].ID != nativeMarkerRecoveredID {
		t.Fatalf("The test read saved item ID %q instead of %q", savedCopies[0].ID, nativeMarkerRecoveredID)
	}

	reloaded := startNativeMarkerServer(ctx, t, binary, home)
	reloaded.call("thread/resume", map[string]any{"threadId": threadID, "approvalPolicy": "never", "sandbox": "read-only"})
	reloaded.turn(threadID, nativeMarkerReloadPrompt)
	reloaded.stop()
	nextRequest := upstream.lastRequest(t, "reload")
	if !strings.Contains(nextRequest.text, nativeMarkerCompactCipher) {
		t.Fatal("The request after reload does not contain the saved compaction item.")
	}
	requireNativeMarkerCopies(t, "request after reload", nextRequest.input, injection, 1)
	requireNativeMarkerCopies(t, "rollout after reload", readNativeMarkerResponseItems(t, thread.Thread.Path), injection, 1)

	assertNativeMarkerExports(t, home, thread.Thread.Path, threadID, injection)
}

// nativeMarkerExportBodies exports to a file through the CLI and
// clyde_export_transcript and returns both file bodies.
func nativeMarkerExportBodies(t *testing.T, binaryPath string, cliEnvironment []string, mcpEnvironment []string, export injectedProbeExport) map[string]string {
	t.Helper()
	bodies := make(map[string]string, 2)
	injectedProbeCLI(t, binaryPath, cliEnvironment, export)
	cliBody, err := os.ReadFile(export.output)
	if err != nil {
		t.Fatalf("The test could not read the CLI export: %v", err)
	}
	bodies["cli"] = string(cliBody)
	if err := os.Remove(export.output); err != nil {
		t.Fatalf("The test could not remove the CLI export: %v", err)
	}
	injectedProbeMCPBody(t, binaryPath, mcpEnvironment, export)
	mcpBody, err := os.ReadFile(export.output)
	if err != nil {
		t.Fatalf("The test could not read the MCP export: %v", err)
	}
	bodies["mcp"] = string(mcpBody)
	return bodies
}

func assertNativeMarkerExports(t *testing.T, home string, rolloutPath string, threadID string, injection string) {
	t.Helper()
	binaryPath := buildClydeCLI(t)
	before := injectedProbeDigest(t, rolloutPath)
	environment := environmentWith(localProofProviderEnvironment(t, home)...)
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, threadID)

	quotation := strings.TrimSpace(codexProbeFramedQuotation)
	for _, only := range []string{injectedProbeChatOnly, codexProbeToolOutputsOnly, injectedProbeAll} {
		export := injectedProbeExport{name: only, provider: codexProbeProvider, session: threadID, only: only, maxTokens: "", output: filepath.Join(t.TempDir(), nativeMarkerAllFile), lastN: codexProbeFullLastN}
		bodies := nativeMarkerExportBodies(t, binaryPath, environment, sandboxDaemon.environment, export)
		for surface, body := range bodies {
			label := "native " + surface + " " + only
			switch only {
			case injectedProbeChatOnly:
				assertCodexProbeContains(t, label, body, nativeMarkerRecovered, nativeMarkerToolPrompt, quotation)
				assertCodexProbeOmits(t, label, body, nativeMarkerToolText)
			case codexProbeToolOutputsOnly:
				assertCodexProbeContains(t, label, body, nativeMarkerToolText)
				assertCodexProbeOmits(t, label, body, nativeMarkerRecovered, nativeMarkerFirstPrompt)
			default:
				assertCodexProbeContains(t, label, body, nativeMarkerRecovered, injection, quotation)
			}
		}
	}
	if after := injectedProbeDigest(t, rolloutPath); after != before {
		t.Errorf("The export changed the native rollout file (before digest: %s, after digest: %s).", before, after)
	}
}
