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
