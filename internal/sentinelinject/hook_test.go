package sentinelinject

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

const (
	sentinelKeyword = "MYKEYWORD"

	sentinelRequestBody = `{"messages":[` +
		`{"role":"user","content":"older MYKEYWORD ignored"},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":"ignore MYKEYWORD\nforced reply"}` +
		`]}`

	normalRequestBody = `{"messages":[` +
		`{"role":"user","content":"say hello"}` +
		`]}`

	upstreamSSEResponse = "event: message_start\n" +
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
)

type staticHookBody struct {
	body []byte
}

func (b staticHookBody) Bytes() ([]byte, error) {
	return b.body, nil
}

func claudeMessagesRequest(body []byte) mitm.RequestResponseHookRequest {
	return mitm.RequestResponseHookRequest{
		Provider: claudeProviderName,
		Method:   http.MethodPost,
		Path:     messagesPath,
		Body:     staticHookBody{body: body},
	}
}

func eventStreamResponse(body string) mitm.ResponseHookResponse {
	return mitm.ResponseHookResponse{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          strings.NewReader(body),
		ContentLength: int64(len(body)),
	}
}

func TestExtractForcedContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		user     string
		sentinel string
		want     string
		ok       bool
	}{
		{name: "missing keyword", user: "hello", sentinel: sentinelKeyword, want: "", ok: false},
		{name: "suffix after keyword", user: "ignore MYKEYWORD\nforced reply", sentinel: sentinelKeyword, want: "\nforced reply", ok: true},
		{name: "empty suffix", user: "ignore MYKEYWORD", sentinel: sentinelKeyword, want: "", ok: true},
		{name: "empty sentinel", user: "ignore MYKEYWORD", sentinel: "", want: "", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := extractForcedContent(tc.user, tc.sentinel)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("suffix = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHookMatchesLatestUserMessage(t *testing.T) {
	t.Parallel()
	hook := New(sentinelKeyword)
	match, err := hook.MatchRequestResponse(claudeMessagesRequest([]byte(sentinelRequestBody)))
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if !match.Matched {
		t.Fatal("expected sentinel request to match")
	}
	transformer, ok := match.Transformer.(responseReplaceTransformer)
	if !ok {
		t.Fatalf("transformer type = %T, want responseReplaceTransformer", match.Transformer)
	}
	if transformer.content != "\nforced reply" {
		t.Fatalf("content = %q, want %q", transformer.content, "\nforced reply")
	}
}

func TestHookDoesNotMatchWithoutKeyword(t *testing.T) {
	t.Parallel()
	hook := New(sentinelKeyword)
	match, err := hook.MatchRequestResponse(claudeMessagesRequest([]byte(normalRequestBody)))
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("expected normal request not to match")
	}
}

func TestHookIgnoresKeywordOnlyInOlderUserMessage(t *testing.T) {
	t.Parallel()
	body := `{"messages":[` +
		`{"role":"user","content":"older MYKEYWORD ignored"},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":"say hello"}` +
		`]}`
	hook := New(sentinelKeyword)
	match, err := hook.MatchRequestResponse(claudeMessagesRequest([]byte(body)))
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("keyword only in an older user message must not match")
	}
}

func TestHookRejectsNonClaudeProvider(t *testing.T) {
	t.Parallel()
	hook := New(sentinelKeyword)
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Provider: "openai",
		Method:   http.MethodPost,
		Path:     messagesPath,
		Body:     staticHookBody{body: []byte(sentinelRequestBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("non-claude provider must not match")
	}
}

func TestHookRejectsPrefixedMessagesPath(t *testing.T) {
	t.Parallel()
	hook := New(sentinelKeyword)
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Provider: claudeProviderName,
		Method:   http.MethodPost,
		Path:     "/proxy/v1/messages",
		Body:     staticHookBody{body: []byte(sentinelRequestBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("prefixed /v1/messages path must not match")
	}
}

func TestTransformResponseReplacesModelText(t *testing.T) {
	t.Parallel()
	transformer := responseReplaceTransformer{content: "\nforced reply"}
	out, err := transformer.TransformResponse(
		context.Background(),
		eventStreamResponse(upstreamSSEResponse),
	)
	if err != nil {
		t.Fatalf("TransformResponse err = %v", err)
	}
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read transformed body: %v", err)
	}
	events := parseSSEEvents(string(body))
	wantNames := []string{
		"message_start",
		string(contentBlockEventStart),
		string(contentBlockEventDelta),
		string(contentBlockEventStop),
		"message_delta",
		"message_stop",
	}
	if len(events) != len(wantNames) {
		t.Fatalf("events = %d (%v), want %d (%v)", len(events), eventNames(events), len(wantNames), wantNames)
	}
	for i, name := range wantNames {
		if events[i].Name != name {
			t.Fatalf("events[%d].Name = %q, want %q", i, events[i].Name, name)
		}
	}
	var delta contentBlockDeltaPayload
	if err := json.Unmarshal([]byte(events[2].Data), &delta); err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	wantDelta := newContentBlockDeltaPayload(0, "\nforced reply")
	if delta != wantDelta {
		t.Fatalf("delta = %#v, want %#v", delta, wantDelta)
	}
}

func TestReplaceSSETextPreservesEmptyAndPingOnlyStreams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "ping only", body: "event: ping\ndata: {}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := replaceSSEText([]byte(tc.body), "forced")
			if err != nil {
				t.Fatalf("replaceSSEText err = %v", err)
			}
			if string(got) != tc.body {
				t.Fatalf("body = %q, want unchanged %q", string(got), tc.body)
			}
		})
	}
}

func TestTransformPassesThroughNonStreamingResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		status      int
		statusText  string
		contentType string
		body        string
	}{
		{
			name:        "4xx json error",
			status:      http.StatusBadRequest,
			statusText:  "400 Bad Request",
			contentType: "application/json",
			body:        `{"type":"error"}`,
		},
		{
			name:        "200 non-stream json",
			status:      http.StatusOK,
			statusText:  "200 OK",
			contentType: "application/json",
			body:        `{"type":"message","content":[{"type":"text","text":"hello"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := mitm.ResponseHookResponse{
				StatusCode:    tc.status,
				Status:        tc.statusText,
				Proto:         "HTTP/1.1",
				Header:        http.Header{"Content-Type": []string{tc.contentType}},
				Body:          strings.NewReader(tc.body),
				ContentLength: int64(len(tc.body)),
			}
			transformer := responseReplaceTransformer{content: "forced"}
			out, err := transformer.TransformResponse(context.Background(), resp)
			if err != nil {
				t.Fatalf("TransformResponse err = %v", err)
			}
			body, err := io.ReadAll(out.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(body) != tc.body {
				t.Fatalf("body = %q, want unchanged %q", string(body), tc.body)
			}
		})
	}
}

func TestNewEmptySentinelNeverMatches(t *testing.T) {
	t.Parallel()
	hook := New("")
	match, err := hook.MatchRequestResponse(claudeMessagesRequest([]byte(sentinelRequestBody)))
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("empty sentinel must not match")
	}
}

func eventNames(events []sseEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Name)
	}
	return names
}
