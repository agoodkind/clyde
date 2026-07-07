package reorientinject

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

// compactRequestBody is a summarization request: its final user message carries
// Claude Code's compaction prompt, and metadata.user_id is the double-encoded
// JSON string that names the session.
const compactRequestBody = `{"messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","content":"ok"},` +
	`{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far."}]}` +
	`],"metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"sess-abc\"}"}}`

// normalRequestBody is an ordinary turn: same structure, no compaction prompt in
// the final message.
const normalRequestBody = `{"messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","content":"ok"},` +
	`{"role":"user","content":"say a color"}` +
	`],"metadata":{"user_id":"{\"session_id\":\"sess-abc\"}"}}`

const summarySSEResponse = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","content":[]}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the summary</summary>"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

type staticHookBody struct {
	body       []byte
	failIfRead bool
	t          *testing.T
}

func (b staticHookBody) Bytes() ([]byte, error) {
	if b.failIfRead {
		b.t.Fatalf("request body read on the cheap-reject path")
	}
	return b.body, nil
}

func fixedContentProvider(content string) ContentProvider {
	return func(context.Context, string) (string, error) {
		return content, nil
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

func TestHookMatchesCompactionSummaryRequest(t *testing.T) {
	t.Parallel()
	hook := New(fixedContentProvider("recovered"))
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{},
		Body:   staticHookBody{body: []byte(compactRequestBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if !match.Matched {
		t.Fatal("expected the compaction summary request to match")
	}
	if match.Transformer == nil {
		t.Fatal("expected a transformer on match")
	}
	appender, ok := match.Transformer.(responseAppendTransformer)
	if !ok {
		t.Fatalf("transformer type = %T, want responseAppendTransformer", match.Transformer)
	}
	if appender.sessionID != "sess-abc" {
		t.Fatalf("sessionID = %q, want %q", appender.sessionID, "sess-abc")
	}
}

func TestHookIgnoresNormalTurn(t *testing.T) {
	t.Parallel()
	hook := New(fixedContentProvider("recovered"))
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{},
		Body:   staticHookBody{body: []byte(normalRequestBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("a normal turn must not match")
	}
}

func TestHookIgnoresCompactionWithoutSessionID(t *testing.T) {
	t.Parallel()
	// A compaction summary request with no metadata.user_id.session_id cannot be
	// correlated to a transcript, so it must not match (no misleading "matched").
	const noSession = `{"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far."}]}` +
		`]}`
	hook := New(fixedContentProvider("recovered"))
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{},
		Body:   staticHookBody{body: []byte(noSession)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if match.Matched {
		t.Fatal("a compaction request without a session id must not match")
	}
}

func TestHookCheapRejectDoesNotReadBody(t *testing.T) {
	t.Parallel()
	hook := New(fixedContentProvider("recovered"))
	cases := []mitm.RequestResponseHookRequest{
		{Method: http.MethodGet, Path: "/v1/messages", Header: http.Header{}, Body: staticHookBody{failIfRead: true, t: t}},
		{Method: http.MethodPost, Path: "/v1/models", Header: http.Header{}, Body: staticHookBody{failIfRead: true, t: t}},
	}
	for _, request := range cases {
		match, err := hook.MatchRequestResponse(request)
		if err != nil {
			t.Fatalf("MatchRequestResponse err = %v", err)
		}
		if match.Matched {
			t.Fatalf("request %s %s must not match", request.Method, request.Path)
		}
	}
}

func TestHookMatchesQueryParameterizedPath(t *testing.T) {
	t.Parallel()
	// The seam strips the query, so Path is already query-free here; assert the
	// suffix match still holds for the bare messages path.
	hook := New(fixedContentProvider("recovered"))
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{},
		Body:   staticHookBody{body: []byte(compactRequestBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if !match.Matched {
		t.Fatal("expected a match on /v1/messages")
	}
}

func transformSummary(t *testing.T, provider ContentProvider, resp mitm.ResponseHookResponse) mitm.ResponseHookResponse {
	t.Helper()
	transformer := responseAppendTransformer{provider: provider, sessionID: "sess-abc"}
	out, err := transformer.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse err = %v", err)
	}
	return out
}

func readBody(t *testing.T, resp mitm.ResponseHookResponse) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read transformed body: %v", err)
	}
	return string(data)
}

func TestTransformInjectsInsideSummarySpan(t *testing.T) {
	t.Parallel()
	out := transformSummary(t, fixedContentProvider("RECOVERED-TRANSCRIPT"), eventStreamResponse(summarySSEResponse))
	body := readBody(t, out)
	if !strings.Contains(body, preCompactionTranscriptOpen) || !strings.Contains(body, preCompactionTranscriptClose) {
		t.Fatal("transformed body missing the pre-compaction-transcript markers")
	}
	if !strings.Contains(body, "RECOVERED-TRANSCRIPT") {
		t.Fatal("transformed body missing the recovered content")
	}
	// The model's summary text must survive.
	if !strings.Contains(body, "the summary") {
		t.Fatal("original summary text was dropped")
	}
	// The transcript must land INSIDE the summary span: before </summary>, so the
	// client keeps it as part of the extracted summary. It must be written into
	// the model's own text block (index 0), not a new trailing block.
	recoveredAt := strings.Index(body, "RECOVERED-TRANSCRIPT")
	summaryCloseAt := strings.Index(body, "</summary>")
	if recoveredAt < 0 || summaryCloseAt < 0 || recoveredAt > summaryCloseAt {
		t.Fatal("recovered transcript must be injected before </summary> (inside the summary span)")
	}
	if strings.Contains(body, `"index":1`) {
		t.Fatal("injection must modify the summary block, not add a new content block")
	}
	if out.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1", out.ContentLength)
	}
	if out.Header.Get("Content-Length") != "" {
		t.Fatal("Content-Length header should be cleared after rewrite")
	}
}

// malformedSummarySSEResponse has no </summary> (an empty/malformed summary), so
// the client keeps the whole assistant text and a trailing appended block
// survives.
const malformedSummarySSEResponse = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","content":[]}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestTransformFallsBackToAppendedBlockWhenNoSummaryTag(t *testing.T) {
	t.Parallel()
	out := transformSummary(t, fixedContentProvider("RECOVERED-TRANSCRIPT"), eventStreamResponse(malformedSummarySSEResponse))
	body := readBody(t, out)
	if !strings.Contains(body, "RECOVERED-TRANSCRIPT") || !strings.Contains(body, preCompactionTranscriptOpen) {
		t.Fatal("malformed-summary response must still receive the transcript via a trailing block")
	}
	if !strings.Contains(body, `"index":1`) {
		t.Fatal("with no </summary>, the transcript should be appended as a new content block (index 1)")
	}
	recoveredAt := strings.Index(body, "RECOVERED-TRANSCRIPT")
	stopAt := strings.LastIndex(body, "message_stop")
	if recoveredAt < 0 || stopAt < 0 || recoveredAt > stopAt {
		t.Fatal("appended block must come before the terminal message_stop")
	}
}

func TestTransformPassesThroughEmptyContent(t *testing.T) {
	t.Parallel()
	out := transformSummary(t, fixedContentProvider(""), eventStreamResponse(summarySSEResponse))
	if got := readBody(t, out); got != summarySSEResponse {
		t.Fatal("empty content must pass the response through unchanged")
	}
}

func TestTransformPassesThroughProviderError(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	out := transformSummary(t, failing, eventStreamResponse(summarySSEResponse))
	if got := readBody(t, out); got != summarySSEResponse {
		t.Fatal("a provider error must pass the response through unchanged")
	}
}

type erroringReader struct{}

func (erroringReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestTransformFailsOpenOnBodyReadError(t *testing.T) {
	t.Parallel()
	// A read failure on the summary stream must not return an error (the seam
	// would turn it into a 502 and break /compact); it must fail open.
	resp := mitm.ResponseHookResponse{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          erroringReader{},
		ContentLength: -1,
	}
	transformer := responseAppendTransformer{provider: fixedContentProvider("RECOVERED"), sessionID: "sess-abc"}
	out, err := transformer.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse must fail open on a read error, got err=%v", err)
	}
	if out.Body == nil {
		t.Fatal("fail-open response must carry a non-nil body")
	}
}

func TestTransformPassesThroughNonStreamingResponse(t *testing.T) {
	t.Parallel()
	type respCase struct {
		name        string
		status      int
		contentType string
		body        string
	}
	cases := []respCase{
		{name: "4xx json error", status: http.StatusBadRequest, contentType: "application/json", body: `{"type":"error","error":{"message":"boom"}}`},
		{name: "200 non-stream json", status: http.StatusOK, contentType: "application/json", body: `{"type":"message"}`},
	}
	for _, tc := range cases {
		resp := mitm.ResponseHookResponse{
			StatusCode:    tc.status,
			Status:        "",
			Proto:         "HTTP/1.1",
			Header:        http.Header{"Content-Type": []string{tc.contentType}},
			Body:          strings.NewReader(tc.body),
			ContentLength: int64(len(tc.body)),
		}
		out := transformSummary(t, fixedContentProvider("RECOVERED"), resp)
		if got := readBody(t, out); got != tc.body {
			t.Fatalf("%s must pass through unchanged: got %q", tc.name, got)
		}
	}
}
