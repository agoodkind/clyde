package mitmcontrib

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/agentgateaction"
	"goodkind.io/clyde/internal/mitm"
)

func TestResponseAdapterAppliesRealAgentGateDiagnostic(t *testing.T) {
	command := os.Getenv("AGENT_GATE_TEST_COMMAND")
	if command == "" {
		t.Skip("AGENT_GATE_TEST_COMMAND is unset")
	}
	prohibitedVerb := strings.Join([]string{"car", "ries"}, "")
	responseText := "The response " + prohibitedVerb + " the result."
	body := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + responseText + `"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	response, err := routeProvider{}.TransformResponse(
		context.Background(),
		mitm.RequestResponseHookRequest{Header: http.Header{"X-Claude-Code-Session-Id": {"session-test"}}},
		eventStreamResponse(body),
		agentgateaction.New(command),
	)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	for _, expected := range []string{responseText, "Agent Gate rejected", "no-vague-prose-relationships"} {
		if !bytes.Contains(got, []byte(expected)) {
			t.Fatalf("response missing %q: %s", expected, got)
		}
	}
}

func eventStreamResponse(body string) mitm.ResponseHookResponse {
	return mitm.ResponseHookResponse{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"text/event-stream"}},
		Body:          strings.NewReader(body),
		ContentLength: int64(len(body)),
	}
}
