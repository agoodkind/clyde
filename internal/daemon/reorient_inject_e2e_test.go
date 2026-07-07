package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/reorientinject"
)

type e2eHookBody struct {
	data []byte
}

func (b e2eHookBody) Bytes() ([]byte, error) {
	return b.data, nil
}

// TestReorientInjectEndToEnd exercises the whole Tier 2 path with the real
// pieces: the daemon content provider resolves a session id to an on-disk Claude
// transcript, renders it, and the reorient hook appends it to a summary SSE
// response. It uses a temp HOME so it never reads the real projects directory or
// the shared daemon.
func TestReorientInjectEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessionID := "e2e-session-xyz"
	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	transcript := `{"sessionId":"` + sessionID + `","cwd":"/repo","type":"user","timestamp":"2026-07-06T19:00:00Z","message":{"role":"user","content":"REORIENT-E2E-MARKER hello world"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"acknowledged reply"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	provider := newReorientInjectContentProvider()
	content, err := provider(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("provider err = %v", err)
	}
	if !strings.Contains(content, "REORIENT-E2E-MARKER") {
		t.Fatalf("provider content missing the transcript marker; got %q", content)
	}

	hook := reorientinject.New(provider)
	requestBody := `{"messages":[` +
		`{"role":"user","content":"earlier turn"},` +
		`{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far."}]}` +
		`],"metadata":{"user_id":"{\"session_id\":\"` + sessionID + `\"}"}}`
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{},
		Body:   e2eHookBody{data: []byte(requestBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if !match.Matched || match.Transformer == nil {
		t.Fatalf("expected a match with a transformer; matched=%v", match.Matched)
	}

	sse := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"summary body</summary>"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	out, err := match.Transformer.TransformResponse(context.Background(), mitm.ResponseHookResponse{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          strings.NewReader(sse),
		ContentLength: int64(len(sse)),
	})
	if err != nil {
		t.Fatalf("TransformResponse err = %v", err)
	}
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read transformed body: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "REORIENT-E2E-MARKER") {
		t.Fatalf("transformed SSE missing the recovered transcript marker")
	}
	if !strings.Contains(body, "pre-compaction-transcript") {
		t.Fatal("transformed SSE missing the pre-compaction-transcript wrapper")
	}
	if !strings.Contains(body, "summary body") || !strings.Contains(body, "</summary>") {
		t.Fatal("transformed SSE dropped the original summary text")
	}
	// The transcript must be injected inside the summary span (before </summary>).
	if strings.Index(body, "REORIENT-E2E-MARKER") > strings.Index(body, "</summary>") {
		t.Fatal("transcript must be injected before </summary>, inside the summary span")
	}
}
