package agentgateresponse_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/agentgateresponse"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/sentinelinject"
)

const responseSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","content":[]}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

type requestBody struct {
	value []byte
}

func (b requestBody) Bytes() ([]byte, error) {
	return b.value, nil
}

func TestHookAppendsRealAgentGateDiagnosticAfterSentinelRewrite(t *testing.T) {
	command := agentGateCommand(t)
	prohibitedVerb := strings.Join([]string{"car", "ries"}, "")
	forced := "The response " + prohibitedVerb + " the result."
	requestJSON := `{"messages":[{"role":"user","content":"MYKEYWORD ` + forced + `"}]}`
	hook := agentgateresponse.New(command, []mitm.RequestResponseHook{
		sentinelinject.New("MYKEYWORD", ""),
	})
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Provider: "claude",
		Method:   http.MethodPost,
		Path:     "/v1/messages",
		Header:   http.Header{"X-Claude-Code-Session-Id": {"session-test"}},
		Body:     requestBody{value: []byte(requestJSON)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	if !match.Matched || match.Transformer == nil {
		t.Fatal("Anthropic response check did not match")
	}
	response, err := match.Transformer.TransformResponse(
		context.Background(),
		eventStreamResponse(responseSSE),
	)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read transformed body: %v", err)
	}
	for _, expected := range []string{
		forced,
		"Agent-gate detected a rule violation in the preceding response.",
		"no-vague-prose-relationships",
	} {
		if !bytes.Contains(body, []byte(expected)) {
			t.Fatalf("response missing %q: %s", expected, body)
		}
	}
	if bytes.Contains(body, []byte(`"text":"hello"`)) {
		t.Fatalf("sentinel response replacement did not run first: %s", body)
	}
}

func TestHookLeavesCompliantResponseUnchanged(t *testing.T) {
	command := agentGateCommand(t)
	hook := agentgateresponse.New(command, nil)
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Provider: "claude",
		Method:   http.MethodPost,
		Path:     "/v1/messages",
		Header:   http.Header{},
		Body:     requestBody{value: []byte(`{"messages":[]}`)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	response, err := match.Transformer.TransformResponse(
		context.Background(),
		eventStreamResponse(responseSSE),
	)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read transformed body: %v", err)
	}
	if !bytes.Equal(body, []byte(responseSSE)) {
		t.Fatalf("compliant response changed: %s", body)
	}
}

func agentGateCommand(t *testing.T) string {
	t.Helper()
	if command, err := exec.LookPath("agent-gate"); err == nil {
		return command
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("resolve home directory: %v", err)
	}
	command := filepath.Join(home, ".local", "bin", "agent-gate")
	if _, err := os.Stat(command); err != nil {
		t.Skipf("agent-gate is unavailable: %v", err)
	}
	return command
}

func eventStreamResponse(body string) mitm.ResponseHookResponse {
	return mitm.ResponseHookResponse{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		Header:        http.Header{"Content-Type": {"text/event-stream"}},
		Body:          strings.NewReader(body),
		ContentLength: int64(len(body)),
	}
}
