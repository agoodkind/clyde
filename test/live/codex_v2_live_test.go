//go:build live

package live

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

const liveEncryptedContent = "live-encrypted-state"
const liveCodexVersion = "codex-cli 0.151.0"

type codexLiveRequest struct {
	path                        string
	inputTypes                  []string
	requestKind                 string
	compactionPhase             string
	compactionImplementation    string
	encryptedContentHash        [sha256.Size]byte
	hasEncryptedContent         bool
	transcriptTagCount          int
	v2Compaction                bool
	regularTurn                 bool
	regularFinalAnswer          bool
	v2LayoutAccepted            bool
	v2PlannerAccepted           bool
	v2PlannerAcceptedFullWindow bool
	v2PlannerAcceptedUnbounded  bool
	v2FunctionPairComplete      bool
	v2ObserverArmed             bool
	v2RecoveryInjectable        bool
	v2SessionIDPresent          bool
	sessionIDHash               [sha256.Size]byte
	decodedRequestBodyHash      [sha256.Size]byte
	encodedRequestBodyHash      [sha256.Size]byte
	contentEncoding             string
}

type codexLiveClientResponse struct {
	request  codexLiveRequest
	tagCount int
}

func codexLiveLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("allocate loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return port
}

func configureCodexLivePorts(t *testing.T, h *harness) {
	t.Helper()
	used := make(map[int]bool)
	nextPort := func() int {
		for {
			port := codexLiveLoopbackPort(t)
			if !used[port] {
				used[port] = true
				return port
			}
		}
	}
	h.cfg.MITMPort = nextPort()
	h.cfg.AdapterPort = nextPort()
	h.cfg.CursorPort = nextPort()
	h.cfg.TopologyPort = nextPort()
	h.cfg.MovedMITMPort = nextPort()
}

func codexLiveSessionID(output []byte) (string, bool) {
	for _, line := range bytes.Split(output, []byte("\n")) {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type != "thread.started" || event.ThreadID == "" {
			continue
		}
		return event.ThreadID, true
	}
	return "", false
}

func TestLiveCodexV2ForegroundFunctionCall(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the logged-in Codex CLI")
	}
	versionCommand := exec.Command("codex", "--version")
	versionOutput, err := versionCommand.Output()
	if err != nil {
		t.Fatalf("read Codex version: %v", err)
	}
	if strings.TrimSpace(string(versionOutput)) != liveCodexVersion {
		t.Fatalf("Codex version=%q, want %q", strings.TrimSpace(string(versionOutput)), liveCodexVersion)
	}
	var requests []codexLiveRequest
	var responses []codexLiveRequest
	var clientResponses []codexLiveClientResponse
	var upstreamTagCounts []int
	var upstreamBranches []string
	v2Triggered := false
	var mutex sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		decodedBody := body
		if request.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(nil)
			if err == nil {
				decodedBody, err = decoder.DecodeAll(body, nil)
				decoder.Close()
			}
			if err != nil {
				http.Error(writer, "decode", http.StatusBadRequest)
				return
			}
		}
		observed := summarizeCodexLiveRequest(request, body, decodedBody)
		mutex.Lock()
		requests = append(requests, observed)
		if request.Method == http.MethodPost && request.URL.Path == "/backend-api/codex/responses" {
			responses = append(responses, observed)
		}
		responseCount := len(responses)
		mutex.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		responseBody := ""
		responseBranch := "final"
		if bytes.Contains(decodedBody, []byte(`"type":"compaction_trigger"`)) {
			responseBranch = "compaction"
			mutex.Lock()
			v2Triggered = true
			mutex.Unlock()
			responseBody = "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"" + liveEncryptedContent + "\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"live-compact\",\"status\":\"completed\",\"output\":[]}}\n\n"
		} else if responseCount == 1 || responseCount == 3 {
			responseBranch = "tool"
			callID := "live-call-1"
			if responseCount == 3 {
				callID = "live-call-2"
			}
			responseBody = "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"" + callID + "\",\"name\":\"exec_command\",\"arguments\":\"{\\\"cmd\\\":\\\"seq 1 1500\\\"}\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"live-" + fmt.Sprint(responseCount) + "\",\"status\":\"completed\",\"output\":[]}}\n\n"
		} else {
			responseBody = "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"live-2\",\"status\":\"completed\",\"output\":[]}}\n\n"
		}
		mutex.Lock()
		upstreamTagCounts = append(upstreamTagCounts, codexLiveSSETranscriptTagCount([]byte(responseBody)))
		upstreamBranches = append(upstreamBranches, responseBranch)
		mutex.Unlock()
		_, _ = io.WriteString(writer, responseBody)
	}))
	t.Cleanup(server.Close)
	authFile := filepath.Join(t.TempDir(), "auth.json")
	testAccessToken := strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`)),
		"test-signature",
	}, ".")
	testRefreshToken := strings.Join([]string{"test", "refresh", "token"}, "-")
	authJSON := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"account_id":"live-test-account"}}`, testAccessToken, testRefreshToken)
	if err := os.WriteFile(authFile, []byte(authJSON), 0o600); err != nil {
		t.Fatalf("write isolated Codex auth file: %v", err)
	}
	h := newHarness(t)
	configureCodexLivePorts(t, h)
	h.writeCodexAdapterConfig(t, server.URL+"/backend-api/codex/responses", authFile)
	h.boot(t)
	adapterURL := "http://[::1]:" + fmt.Sprint(h.cfg.AdapterPort)
	clientProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		decodedBody := body
		if request.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(nil)
			if err == nil {
				decodedBody, err = decoder.DecodeAll(body, nil)
				decoder.Close()
			}
			if err != nil {
				http.Error(writer, "decode", http.StatusBadRequest)
				return
			}
		}
		forwarded, err := http.NewRequestWithContext(request.Context(), request.Method, adapterURL+request.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(writer, "forward", http.StatusBadGateway)
			return
		}
		forwarded.Header = request.Header.Clone()
		response, err := http.DefaultClient.Do(forwarded)
		if err != nil {
			http.Error(writer, "adapter", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		responseBody, _ := io.ReadAll(response.Body)
		for key, values := range response.Header {
			writer.Header()[key] = append([]string(nil), values...)
		}
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(responseBody)
		if request.Method == http.MethodPost && request.URL.Path == "/v1/responses" {
			mutex.Lock()
			clientResponses = append(clientResponses, codexLiveClientResponse{request: summarizeCodexLiveRequest(request, body, decodedBody), tagCount: codexLiveSSETranscriptTagCount(responseBody)})
			mutex.Unlock()
		}
	}))
	t.Cleanup(clientProxy.Close)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("redacted daemon diagnostics: %s", h.dumpLogsOnFailure(t))
		}
	})

	commandContext, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	commandArgs := []string{"exec", "--json", "--skip-git-repo-check", "-C", t.TempDir(), "-c", `model_provider="openai"`, "-c", `model_context_window=2000`, "-c", `openai_base_url="` + clientProxy.URL + `/v1"`}
	commandArgs = append(commandArgs, "Return after the shell result.")
	command := exec.CommandContext(commandContext, "codex", commandArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex exit=%v output_bytes=%d logs=%s", err, len(output), h.dumpLogsOnFailure(t))
	}
	sessionID, ok := codexLiveSessionID(output)
	if !ok {
		t.Fatalf("Codex emitted no persisted session identifier; output_bytes=%d", len(output))
	}
	resumeArgs := []string{"exec", "--json", "--skip-git-repo-check", "-C", t.TempDir(), "-c", `model_provider="openai"`, "-c", `model_context_window=2000`, "-c", `openai_base_url="` + clientProxy.URL + `/v1"`, "resume", sessionID, "Return a final answer after the prior shell result."}
	resume := exec.CommandContext(commandContext, "codex", resumeArgs...)
	if output, err := resume.CombinedOutput(); err != nil {
		safeOutput := strings.ReplaceAll(string(output), sessionID, "<session>")
		t.Fatalf("Codex resume exit=%v output=%q logs=%s", err, safeOutput, h.dumpLogsOnFailure(t))
	}
	resendArgs := []string{"exec", "--json", "--skip-git-repo-check", "-C", t.TempDir(), "-c", `model_provider="openai"`, "-c", `model_context_window=2000`, "-c", `openai_base_url="` + clientProxy.URL + `/v1"`, "resume", sessionID, "Return after the prior final answer."}
	resend := exec.CommandContext(commandContext, "codex", resendArgs...)
	if output, err := resend.CombinedOutput(); err != nil {
		safeOutput := strings.ReplaceAll(string(output), sessionID, "<session>")
		t.Fatalf("Codex resend exit=%v output=%q logs=%s", err, safeOutput, h.dumpLogsOnFailure(t))
	}
	mutex.Lock()
	requestCount := len(responses)
	mutex.Unlock()
	if requestCount < 4 {
		t.Fatalf("Codex sent %d requests, want compaction, recovery, and resend", requestCount)
	}
	mutex.Lock()
	triggered := v2Triggered
	observedResponses := append([]codexLiveRequest(nil), responses...)
	observedClientResponses := append([]codexLiveClientResponse(nil), clientResponses...)
	observedUpstreamTagCounts := append([]int(nil), upstreamTagCounts...)
	observedUpstreamBranches := append([]string(nil), upstreamBranches...)
	mutex.Unlock()
	if !triggered {
		t.Fatalf("Codex emitted no v2 compaction trigger; requests=%d", len(observedResponses))
	}
	assertCodexLiveV2ClientResponses(t, observedClientResponses, observedUpstreamTagCounts, observedUpstreamBranches)
	assertCodexLiveV2Compaction(t, observedResponses)
}

func summarizeCodexLiveRequest(request *http.Request, encodedBody, decodedBody []byte) codexLiveRequest {
	observed := codexLiveRequest{
		path:                   request.URL.Path,
		inputTypes:             nil,
		decodedRequestBodyHash: sha256.Sum256(decodedBody),
		encodedRequestBodyHash: sha256.Sum256(encodedBody),
		contentEncoding:        request.Header.Get("Content-Encoding"),
	}
	var envelope struct {
		Input []struct {
			Type             string          `json:"type"`
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			EncryptedContent string          `json:"encrypted_content"`
			CallID           string          `json:"call_id"`
			Name             string          `json:"name"`
		} `json:"input"`
	}
	encryptedContent := ""
	if json.Unmarshal(decodedBody, &envelope) == nil {
		functionCalls := make(map[string]bool)
		functionOutputs := make(map[string]bool)
		functionNamesPresent := true
		for _, item := range envelope.Input {
			observed.inputTypes = append(observed.inputTypes, codexLiveInputShape(item.Type, item.Role, item.Content))
			observed.transcriptTagCount += codexLiveTranscriptTagCount(item.Type, item.Content)
			if item.EncryptedContent != "" {
				observed.hasEncryptedContent = true
				observed.encryptedContentHash = sha256.Sum256([]byte(item.EncryptedContent))
				encryptedContent = item.EncryptedContent
			}
			if item.Type == "function_call" {
				functionCalls[item.CallID] = true
				if item.Name == "" || item.CallID == "" {
					functionNamesPresent = false
				}
			}
			if item.Type == "function_call_output" {
				functionOutputs[item.CallID] = true
				if item.CallID == "" {
					functionNamesPresent = false
				}
			}
		}
		observed.v2FunctionPairComplete = len(functionCalls) == len(functionOutputs) && functionNamesPresent
		for callID := range functionCalls {
			if !functionOutputs[callID] {
				observed.v2FunctionPairComplete = false
			}
		}
	}
	var metadata struct {
		RequestKind string `json:"request_kind"`
		SessionID   string `json:"session_id"`
		Compaction  struct {
			Implementation string `json:"implementation"`
			Phase          string `json:"phase"`
		} `json:"compaction"`
	}
	if json.Unmarshal([]byte(request.Header.Get("X-Codex-Turn-Metadata")), &metadata) == nil {
		observed.requestKind = metadata.RequestKind
		observed.compactionPhase = metadata.Compaction.Phase
		observed.compactionImplementation = metadata.Compaction.Implementation
		observed.v2Compaction = metadata.RequestKind == "compaction" && metadata.Compaction.Implementation == "responses_compaction_v2" && metadata.Compaction.Phase == "mid_turn"
		observed.regularTurn = metadata.RequestKind == "turn" && metadata.Compaction.Implementation == ""
		observed.regularFinalAnswer = metadata.RequestKind == "turn" && metadata.Compaction.Phase == "final_answer" && metadata.Compaction.Implementation == ""
		observed.v2SessionIDPresent = metadata.SessionID != ""
		observed.sessionIDHash = sha256.Sum256([]byte(metadata.SessionID))
		if observed.regularFinalAnswer && encryptedContent != "" {
			registry := adaptercodex.NewRawResponsesCompactionV2Registry(nil)
			if registry.Arm(metadata.SessionID, encryptedContent, "test-transcript") {
				_, _, observed.v2RecoveryInjectable = adaptercodex.InjectRawResponsesCompactionV2Recovery(
					adaptercodex.RawResponsesRequest{Body: decodedBody, Header: request.Header.Clone()}, registry,
				)
			}
		}
	}
	if observed.v2Compaction {
		_, observed.v2LayoutAccepted = adaptercodex.ParseRawResponsesCompactionV2(
			adaptercodex.RawResponsesRequest{Body: decodedBody, Header: request.Header.Clone()},
		)
		plan, accepted := adaptercodex.PlanRawResponsesCompactionV2(
			adaptercodex.RawResponsesRequest{Body: decodedBody, Header: request.Header.Clone()},
			adaptercodex.RawResponsesCompactionSettings{Enabled: true, ContextWindowTokens: 2000},
		)
		observed.v2PlannerAccepted = accepted
		_, observed.v2PlannerAcceptedFullWindow = adaptercodex.PlanRawResponsesCompactionV2(
			adaptercodex.RawResponsesRequest{Body: decodedBody, Header: request.Header.Clone()},
			adaptercodex.RawResponsesCompactionSettings{Enabled: true, ContextWindowTokens: 2000, ContextWindowFraction: 1},
		)
		_, observed.v2PlannerAcceptedUnbounded = adaptercodex.PlanRawResponsesCompactionV2(
			adaptercodex.RawResponsesRequest{Body: decodedBody, Header: request.Header.Clone()},
			adaptercodex.RawResponsesCompactionSettings{Enabled: true, ContextWindowTokens: 1_000_000, MaxTokens: 1_000_000, ContextWindowFraction: 1},
		)
		if accepted {
			registry := adaptercodex.NewRawResponsesCompactionV2Registry(nil)
			sourceResponse := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"" + liveEncryptedContent + "\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"live-compact\",\"status\":\"completed\",\"output\":[]}}\n\n")
			response := adaptercodex.ObserveRawResponsesCompactionV2Response(&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(sourceResponse)),
			}, plan, registry)
			_, _ = io.Copy(io.Discard, response.Body)
			adaptercodex.ArmRawResponsesCompactionV2Response(response)
			_, observed.v2ObserverArmed = registry.Match(plan.SessionID, liveEncryptedContent)
		}
	}
	return observed
}

func codexLiveTranscriptTagCount(itemType string, content json.RawMessage) int {
	if itemType != "message" {
		return 0
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return 0
	}
	tagCount := 0
	for _, block := range blocks {
		tagCount += strings.Count(block.Text, "<pre-compaction-transcript>")
	}
	return tagCount
}

func codexLiveSSETranscriptTagCount(body []byte) int {
	tagCount := 0
	for _, frame := range bytes.Split(body, []byte("\n\n")) {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var event struct {
				Item struct {
					Type    string          `json:"type"`
					Content json.RawMessage `json:"content"`
				} `json:"item"`
			}
			if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &event) == nil {
				tagCount += codexLiveTranscriptTagCount(event.Item.Type, event.Item.Content)
			}
		}
	}
	return tagCount
}

func codexLiveInputShape(itemType, role string, content json.RawMessage) string {
	if itemType != "message" {
		return itemType + ":" + role
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return itemType + ":" + role + "[invalid]"
	}
	types := make([]string, 0, len(blocks))
	for _, block := range blocks {
		textState := "empty"
		if strings.TrimSpace(block.Text) != "" {
			textState = "text"
		}
		types = append(types, block.Type+":"+textState)
	}
	return itemType + ":" + role + "[" + strings.Join(types, ",") + "]"
}

func containsCodexLiveInputType(types []string, want string) bool {
	for _, itemType := range types {
		if strings.HasPrefix(itemType, want+":") {
			return true
		}
	}
	return false
}

func assertCodexLiveV2Compaction(t *testing.T, requests []codexLiveRequest) {
	t.Helper()
	expectedEncryptedHash := sha256.Sum256([]byte(liveEncryptedContent))
	foundCompaction := false
	foundNPlusOne := false
	foundNPlusTwo := false
	for requestIndex, request := range requests {
		if request.v2Compaction {
			foundCompaction = true
			if request.transcriptTagCount != 0 {
				t.Fatalf("v2 compaction request transcript tags=%d, want 0", request.transcriptTagCount)
			}
			if !request.v2SessionIDPresent {
				t.Fatal("v2 compaction request has no session ID")
			}
		}
		if request.regularTurn && request.hasEncryptedContent {
			if request.encryptedContentHash != expectedEncryptedHash {
				t.Fatal("N encrypted content hash changed before the N+1 request")
			}
			if !foundNPlusOne && request.transcriptTagCount != 1 {
				t.Fatalf("normal recovery request index=%d final=%t encrypted_hash_match=%t transcript_tags=%d, want 1", requestIndex+1, request.regularFinalAnswer, request.encryptedContentHash == expectedEncryptedHash, request.transcriptTagCount)
			}
			if !foundNPlusOne {
				foundNPlusOne = true
				continue
			}
			foundNPlusTwo = true
		}
	}
	if !foundCompaction {
		t.Fatalf("upstream received no v2 compaction request; request_count=%d", len(requests))
	}
	if !foundNPlusOne || !foundNPlusTwo {
		metadata := make([]string, 0, len(requests))
		for _, request := range requests {
			metadata = append(metadata, fmt.Sprintf("%s/%s/%s/%t/%d/layout=%t/planner=%t/full_window=%t/unbounded=%t/pairs=%t/observer=%t/injectable=%t/session=%t/session_hash=%x/%q", request.requestKind, request.compactionImplementation, request.compactionPhase, request.hasEncryptedContent, request.transcriptTagCount, request.v2LayoutAccepted, request.v2PlannerAccepted, request.v2PlannerAcceptedFullWindow, request.v2PlannerAcceptedUnbounded, request.v2FunctionPairComplete, request.v2ObserverArmed, request.v2RecoveryInjectable, request.v2SessionIDPresent, request.sessionIDHash, request.inputTypes))
		}
		t.Fatalf("v2 recovery stages n_plus_one=%t n_plus_two=%t; request_metadata=%q", foundNPlusOne, foundNPlusTwo, metadata)
	}
}

func assertCodexLiveV2ClientResponses(t *testing.T, responses []codexLiveClientResponse, upstreamTagCounts []int, upstreamBranches []string) {
	t.Helper()
	for _, tagCount := range upstreamTagCounts {
		if tagCount != 0 {
			t.Fatalf("upstream response transcript tags=%d, want 0", tagCount)
		}
	}
	foundNPlusTwoFinal := false
	foundNPlusThreeResend := false
	for index, response := range responses {
		if index >= len(upstreamBranches) || upstreamBranches[index] != "final" || !response.request.regularTurn || !response.request.hasEncryptedContent {
			continue
		}
		foundNPlusTwoFinal = true
		if response.tagCount != 1 {
			t.Fatalf("N+2 final client response transcript tags=%d, want 1", response.tagCount)
		}
		if index+1 < len(responses) && responses[index+1].request.transcriptTagCount == 1 {
			foundNPlusThreeResend = true
		}
		break
	}
	if !foundNPlusTwoFinal || !foundNPlusThreeResend {
		if len(responses) != len(upstreamBranches) {
			t.Fatalf("v2 client response stages n_plus_two_final=%t n_plus_three_resend=%t client_responses=%d fixture_responses=%d", foundNPlusTwoFinal, foundNPlusThreeResend, len(responses), len(upstreamBranches))
		}
		expectedSessionHash := [sha256.Size]byte{}
		for _, response := range responses {
			if response.request.v2Compaction {
				expectedSessionHash = response.request.sessionIDHash
				break
			}
		}
		trace := make([]string, 0, len(responses))
		for index, response := range responses {
			trace = append(trace, fmt.Sprintf("%d/%s/turn=%t/final=%t/encrypted=%t/tag_count=%d/session_match=%t/fixture=%s", index+1, response.request.path, response.request.regularTurn, response.request.regularFinalAnswer, response.request.hasEncryptedContent, response.request.transcriptTagCount, response.request.sessionIDHash == expectedSessionHash, upstreamBranches[index]))
		}
		t.Fatalf("v2 client response stages n_plus_two_final=%t n_plus_three_resend=%t trace=%q", foundNPlusTwoFinal, foundNPlusThreeResend, trace)
	}
}
