package sentinelinject

import (
	"context"
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
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   staticHookBody{body: []byte(sentinelRequestBody)},
	})
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
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   staticHookBody{body: []byte(normalRequestBody)},
	})
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
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   staticHookBody{body: []byte(body)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("keyword only in an older user message must not match")
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
	text := string(body)
	if strings.Contains(text, `"text":"hello"`) {
		t.Fatalf("upstream model text leaked into rewritten SSE: %s", text)
	}
	if !strings.Contains(text, `"text":"\nforced reply"`) && !strings.Contains(text, `"text":"\\nforced reply"`) {
		t.Fatalf("rewritten SSE missing forced suffix: %s", text)
	}
}

func TestTransformPassesThroughNonStreamingResponse(t *testing.T) {
	t.Parallel()
	resp := mitm.ResponseHookResponse{
		StatusCode:    http.StatusBadRequest,
		Status:        "400 Bad Request",
		Proto:         "HTTP/1.1",
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          strings.NewReader(`{"type":"error"}`),
		ContentLength: 16,
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
	if string(body) != `{"type":"error"}` {
		t.Fatalf("body = %q, want unchanged error body", string(body))
	}
}

func TestNewEmptySentinelNeverMatches(t *testing.T) {
	t.Parallel()
	hook := New("")
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   staticHookBody{body: []byte(sentinelRequestBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("empty sentinel must not match")
	}
}
