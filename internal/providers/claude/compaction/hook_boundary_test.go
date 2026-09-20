package compaction

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/reorientinject"
	"goodkind.io/clyde/internal/tokencount"
)

// hookBody is the request body the proxy hands a hook.
type hookBody struct {
	raw []byte
	err error
}

func (b hookBody) Bytes() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.raw, nil
}

// unreadableBody fails the test when anything reads it. A test that supplies it
// asserts the hook rejected the request before the decode.
type unreadableBody struct {
	t *testing.T
}

func (b unreadableBody) Bytes() ([]byte, error) {
	b.t.Helper()
	b.t.Fatal("the hook read the request body for a request it must reject first")
	return nil, nil
}

func liveHook(budget int) *reorientinject.Hook {
	return reorientinject.New(NewProvider(), reorientinject.Settings{
		DefaultBudget: budget,
		Counter: tokencount.LocalCounter(
			tokencount.FamilyClaude,
			"claude-opus-5",
			tokencount.Settings{},
		),
	})
}

// hookRequest builds a request Claude Code declared to be its own compaction
// turn. The client sets that declaration; a pasted transcript does not.
func hookRequest(body mitm.RequestResponseHookBody) mitm.RequestResponseHookRequest {
	return mitm.RequestResponseHookRequest{
		Provider: "claude",
		Host:     "api.anthropic.com",
		Method:   http.MethodPost,
		Path:     "/v1/messages",
		Header:   http.Header{},
		Body:     body,
		Purpose:  mitm.RequestPurposeCompaction,
	}
}

// compactionBody builds a real /v1/messages compaction request: a prior summary
// large enough to force a cut, several turns, then the compaction prompt.
func compactionBody(t *testing.T) []byte {
	t.Helper()
	messages := []map[string]any{
		{"role": "user", "content": "start the work"},
		{"role": "assistant", "content": []map[string]string{
			{"type": "text", "text": strings.TrimSpace(strings.Repeat("prior summary text ", 3000))},
		}},
		{"role": "user", "content": "run the tests"},
		{"role": "assistant", "content": []map[string]string{
			{"type": "text", "text": "every test passes"},
		}},
		{"role": "user", "content": []map[string]string{
			{"type": "text", "text": compactionPrompt("")},
		}},
	}
	body, err := json.Marshal(map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 8192,
		"messages":   messages,
		"metadata":   map[string]string{"user_id": `{"session_id":"abc-123"}`},
	})
	if err != nil {
		t.Fatalf("encode compaction body: %v", err)
	}
	return body
}

// TestHookSplitsARealCompactionRequest drives the whole hook: the Claude
// provider decodes the request, the planner cuts it by token budget, and the
// request transformer returns a smaller body the API still accepts.
func TestHookSplitsARealCompactionRequest(t *testing.T) {
	t.Parallel()
	body := compactionBody(t)
	match, err := liveHook(2000).MatchRequestResponse(hookRequest(hookBody{raw: body, err: nil}))
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	if !match.Matched {
		t.Fatal("a compaction request did not match")
	}
	if match.ContinueMatching {
		t.Fatal("a compaction match continued to later response hooks")
	}
	if match.RequestTransformer == nil || match.Transformer == nil {
		t.Fatal("a matched compaction needs both transformers")
	}
	truncated, changed, err := match.RequestTransformer.TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	if !changed {
		t.Fatal("the forwarded request is unchanged")
	}
	if len(truncated) >= len(body) {
		t.Fatalf("truncated body is %d bytes, original is %d", len(truncated), len(body))
	}
	var forwarded struct {
		Model     string            `json:"model"`
		MaxTokens int               `json:"max_tokens"`
		Messages  []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(truncated, &forwarded); err != nil {
		t.Fatalf("the forwarded request is not valid JSON: %v", err)
	}
	if forwarded.Model != "claude-opus-5" || forwarded.MaxTokens != 8192 {
		t.Fatalf("the forwarded request lost a top-level field: %+v", forwarded)
	}
	if len(forwarded.Messages) == 0 {
		t.Fatal("the forwarded request has no messages")
	}
}

// TestHookIgnoresANormalTurn asserts an ordinary request never matches, so the
// proxy forwards it untouched.
func TestHookIgnoresANormalTurn(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(map[string]any{
		"model": "claude-opus-5",
		"messages": []map[string]string{
			{"role": "user", "content": "write the release notes"},
		},
	})
	if err != nil {
		t.Fatalf("encode turn body: %v", err)
	}
	match, matchErr := liveHook(2000).MatchRequestResponse(hookRequest(hookBody{raw: body, err: nil}))
	if matchErr != nil {
		t.Fatalf("MatchRequestResponse: %v", matchErr)
	}
	if match.Matched {
		t.Fatal("an ordinary turn matched the compaction split")
	}
}

// TestHookIgnoresAnUndecodableBody asserts a malformed body forwards unmodified
// rather than failing the request.
func TestHookIgnoresAnUndecodableBody(t *testing.T) {
	t.Parallel()
	body := hookBody{raw: []byte("{not json"), err: nil}
	match, err := liveHook(2000).MatchRequestResponse(hookRequest(body))
	if err != nil {
		t.Fatalf("MatchRequestResponse returned an error for a malformed body: %v", err)
	}
	if match.Matched {
		t.Fatal("a malformed body matched the compaction split")
	}
}

// TestHookIgnoresAnUndeclaredCompactionBody is the regression test for the
// pasted-transcript injection, capture row 364776: an ordinary turn whose last
// user message reproduced the compaction prompt received an injected transcript.
//
// The body here is a complete compaction request, prompt included, and the
// client declared no compaction. The body reader fails the test when anything
// reads it, which pins the declaration check above the decode. Without that
// ordering a pasted transcript is decoded on every ordinary turn.
func TestHookIgnoresAnUndeclaredCompactionBody(t *testing.T) {
	t.Parallel()
	compactionBody(t)
	request := hookRequest(unreadableBody{t: t})
	request.Purpose = mitm.RequestPurposeUnspecified
	match, err := liveHook(2000).MatchRequestResponse(request)
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	if match.Matched {
		t.Fatal("an undeclared request matched the compaction split")
	}
}

// TestHookIgnoresANonMessagesPath asserts the path check runs before the body
// read, so an unrelated request is never decoded.
func TestHookIgnoresANonMessagesPath(t *testing.T) {
	t.Parallel()
	request := hookRequest(unreadableBody{t: t})
	request.Path = "/v1/messages/count_tokens"
	match, err := liveHook(2000).MatchRequestResponse(request)
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	if match.Matched {
		t.Fatal("a count_tokens request matched the compaction split")
	}
}

// TestHookLeavesANonStreamingResponseUnchanged asserts the injection runs only
// on a 200 event stream, so an upstream error body reaches the client intact.
func TestHookLeavesANonStreamingResponseUnchanged(t *testing.T) {
	t.Parallel()
	body := compactionBody(t)
	match, err := liveHook(2000).MatchRequestResponse(hookRequest(hookBody{raw: body, err: nil}))
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	if !match.Matched {
		t.Fatal("a compaction request did not match")
	}
	upstream := []byte(`{"type":"error","error":{"type":"overloaded_error"}}`)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	out, transformErr := match.Transformer.TransformResponse(
		context.Background(),
		mitm.ResponseHookResponse{
			StatusCode:    http.StatusServiceUnavailable,
			Status:        "503 Service Unavailable",
			Proto:         "HTTP/1.1",
			Header:        header,
			Body:          bytes.NewReader(upstream),
			ContentLength: int64(len(upstream)),
		},
	)
	if transformErr != nil {
		t.Fatalf("TransformResponse: %v", transformErr)
	}
	got, readErr := io.ReadAll(out.Body)
	if readErr != nil {
		t.Fatalf("read transformed body: %v", readErr)
	}
	if !bytes.Equal(got, upstream) {
		t.Fatalf("the error body changed: %q", got)
	}
	if out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", out.StatusCode)
	}
}

// recompactionBody builds the request a second /compact sends in a coding
// session: message 0 stores the prior summary plus the prior injection and is
// the largest message, and the later turns are mostly tool call input. The
// markers appear only in message 0's tail and only inside a tool_use input.
func recompactionBody(t *testing.T, arguments string) []byte {
	t.Helper()
	priorInjection := strings.TrimSpace(strings.Repeat("prior injected line ", 6000))
	toolInput := strings.TrimSpace(strings.Repeat("patched line of source ", 1500))
	messages := []map[string]any{
		{"role": "user", "content": priorInjection + " prior-tail-marker"},
		{"role": "assistant", "content": []map[string]any{
			{"type": "text", "text": "editing the file"},
			{"type": "tool_use", "id": "toolu_01AA", "name": "Write", "input": map[string]string{
				"path":    "main.go",
				"content": "tool-call-marker " + toolInput,
			}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_01AA", "content": "wrote main.go"},
		}},
		{"role": "assistant", "content": []map[string]string{
			{"type": "text", "text": "the edit is done"},
		}},
		{"role": "user", "content": []map[string]string{
			{"type": "text", "text": compactionPrompt(arguments)},
		}},
	}
	body, err := json.Marshal(map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 8192,
		"messages":   messages,
		"metadata":   map[string]string{"user_id": `{"session_id":"abc-123"}`},
	})
	if err != nil {
		t.Fatalf("encode recompaction body: %v", err)
	}
	return body
}

// summaryStream is the SSE summary response Claude returns for a compaction.
func summaryStream(t *testing.T) []byte {
	t.Helper()
	delta, err := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]string{"type": "text_delta", "text": "<summary>the recap</summary>"},
	})
	if err != nil {
		t.Fatalf("encode summary delta: %v", err)
	}
	return []byte("event: content_block_delta\ndata: " + string(delta) +
		"\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

// splitOnce runs the whole hook over one compaction body and returns the
// forwarded request and the client-visible summary response, which stores the
// retained content.
func splitOnce(t *testing.T, body []byte, budget int) (forwarded []byte, summary string) {
	t.Helper()
	match, err := liveHook(budget).MatchRequestResponse(hookRequest(hookBody{raw: body, err: nil}))
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	if !match.Matched {
		t.Fatal("a compaction request did not match")
	}
	forwarded, changed, transformErr := match.RequestTransformer.TransformRequest(context.Background(), body)
	if transformErr != nil {
		t.Fatalf("TransformRequest: %v", transformErr)
	}
	if !changed {
		t.Fatal("the split forwarded the request unmodified")
	}
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	stream := summaryStream(t)
	out, responseErr := match.Transformer.TransformResponse(context.Background(), mitm.ResponseHookResponse{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		Header:        header,
		Body:          bytes.NewReader(stream),
		ContentLength: int64(len(stream)),
	})
	if responseErr != nil {
		t.Fatalf("TransformResponse: %v", responseErr)
	}
	got, readErr := io.ReadAll(out.Body)
	if readErr != nil {
		t.Fatalf("read transformed body: %v", readErr)
	}
	return forwarded, string(got)
}

// TestSplitRetainsToolCallsWhenOnlyABudgetIsTyped is the regression test for
// session 0f864a72: `/compact --max-tokens 700k` at 943k retained 180,312
// tokens. The typed budget made the argument reader select `all`, and `all`
// excludes tool calls. A budget argument alone must change no content
// selection.
func TestSplitRetainsToolCallsWhenOnlyABudgetIsTyped(t *testing.T) {
	t.Parallel()
	forwarded, summary := splitOnce(t, recompactionBody(t, "--max-tokens 500k"), 500_000)
	if bytes.Contains(forwarded, []byte("tool-call-marker")) {
		t.Error("the forwarded request still stores the tool call the split retained")
	}
	if !strings.Contains(summary, "tool-call-marker") {
		t.Fatalf("the retained content dropped the tool call input (%d bytes)", len(summary))
	}
}

// TestSplitRetainsTheTailOfMessageZero is the regression test for the second
// defect in the same session: the planner stopped at message index 1 and sent
// the 1.58 MB message 0 to the model whole while 520k of budget sat unused.
func TestSplitRetainsTheTailOfMessageZero(t *testing.T) {
	t.Parallel()
	// Messages 1 through 3 measure about 6,500 tokens and message 0 about
	// 18,000. A 10,000 budget retains every later message and then the tail of
	// message 0.
	forwarded, summary := splitOnce(t, recompactionBody(t, ""), 10_000)
	if !strings.Contains(summary, "prior-tail-marker") {
		t.Fatal("the retained content omits the tail of message 0")
	}
	if !strings.Contains(summary, "tool-call-marker") {
		t.Fatal("the retained content omits the tool call after message 0")
	}
	if bytes.Contains(forwarded, []byte("prior-tail-marker")) {
		t.Error("the forwarded request still stores the tail the split retained")
	}
	if !bytes.Contains(forwarded, []byte("prior injected line")) {
		t.Error("the forwarded request dropped the head of message 0")
	}
}

// TestSplitHonorsAnExplicitContentSelector asserts a typed selector still
// narrows the retained kinds after the default changed to every kind.
func TestSplitHonorsAnExplicitContentSelector(t *testing.T) {
	t.Parallel()
	_, summary := splitOnce(t, recompactionBody(t, "--only chat"), 500_000)
	if strings.Contains(summary, "tool-call-marker") {
		t.Error("--only chat retained a tool call")
	}
	if !strings.Contains(summary, "the edit is done") {
		t.Error("--only chat dropped the chat text")
	}
}
