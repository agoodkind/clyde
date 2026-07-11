package reorientinject

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/reorienttag"
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

// compactWithTrailingSystemBody is an interactive summarization request: the
// compaction prompt is the last user message, but a trailing system-reminder
// message follows it, so the final message is not the prompt. Detection must
// still match by scanning back to the last user message.
const compactWithTrailingSystemBody = `{"messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","content":"ok"},` +
	`{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far."}]},` +
	`{"role":"system","content":"The task tools haven't been used recently. Consider using TaskCreate."}` +
	`],"metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"sess-abc\"}"}}`

// compactWithMultipleTrailingBody has the compaction prompt as the last user
// message followed by several trailing non-user messages (a system reminder and
// an assistant message). Detection must scan back past all of them.
const compactWithMultipleTrailingBody = `{"messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","content":"ok"},` +
	`{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far."}]},` +
	`{"role":"system","content":"The task tools haven't been used recently."},` +
	`{"role":"assistant","content":"noted"}` +
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
	return func(context.Context, string, int) (string, error) {
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
	hook := New(fixedContentProvider("recovered"), 0)
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

func TestHookSetsWindowAwareMaxBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		beta string
		want int
	}{
		{
			name: "1m context uses default 500k token cap",
			beta: "context-1m-2025-08-07",
			want: 500_000 * 4,
		},
		{
			name: "standard context uses half the 200k window",
			beta: "",
			want: 100_000 * 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := New(fixedContentProvider("recovered"), 0)
			match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
				Method: http.MethodPost,
				Path:   "/v1/messages",
				Header: http.Header{"anthropic-beta": []string{tc.beta}},
				Body:   staticHookBody{body: []byte(compactRequestBody)},
			})
			if err != nil {
				t.Fatalf("MatchRequestResponse err = %v", err)
			}
			appender, ok := match.Transformer.(responseAppendTransformer)
			if !ok {
				t.Fatalf("transformer type = %T, want responseAppendTransformer", match.Transformer)
			}
			if appender.maxBytes != tc.want {
				t.Fatalf("maxBytes = %d, want %d", appender.maxBytes, tc.want)
			}
		})
	}
}

// compactSplitBody is a summarization request whose conversation is four messages
// (m1..m4) before the compaction prompt, large enough for a midpoint split.
const compactSplitBody = `{"model":"claude-opus-4-8","system":"sys",` +
	`"messages":[` +
	`{"role":"user","content":"m1-oldest"},` +
	`{"role":"assistant","content":"m2-old"},` +
	`{"role":"user","content":"m3-recent"},` +
	`{"role":"assistant","content":"m4-newest"},` +
	`{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far."}]}` +
	`],"metadata":{"user_id":"{\"session_id\":\"sess-abc\"}"}}`

// compactTinyBody has a single conversation message before the prompt, too small
// to split, so the hook falls back to the disk provider path.
const compactTinyBody = `{"messages":[` +
	`{"role":"user","content":"only"},` +
	`{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far."}]}` +
	`],"metadata":{"user_id":"{\"session_id\":\"sess-abc\"}"}}`

func TestHookSplitsConversationAtMidpoint(t *testing.T) {
	t.Parallel()
	hook := New(nil, 0)
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{"anthropic-beta": []string{"context-1m-2025-08-07"}},
		Body:   staticHookBody{body: []byte(compactSplitBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if !match.Matched {
		t.Fatal("expected a match")
	}
	if match.RequestTransformer == nil {
		t.Fatal("expected a request transformer on a splittable request")
	}
	appender, ok := match.Transformer.(responseAppendTransformer)
	if !ok {
		t.Fatalf("transformer type = %T", match.Transformer)
	}
	// Recent half (m3, m4) is injected verbatim; older half (m1, m2) is not.
	if !strings.Contains(appender.content, "m3-recent") || !strings.Contains(appender.content, "m4-newest") {
		t.Fatalf("injected content missing the recent half: %q", appender.content)
	}
	if strings.Contains(appender.content, "m1-oldest") || strings.Contains(appender.content, "m2-old") {
		t.Fatalf("injected content leaked the older half: %q", appender.content)
	}

	trimmed, changed, err := match.RequestTransformer.TransformRequest(context.Background(), []byte(compactSplitBody))
	if err != nil {
		t.Fatalf("TransformRequest err = %v", err)
	}
	if !changed {
		t.Fatal("expected the request to change")
	}
	var got struct {
		Model    string            `json:"model"`
		System   string            `json:"system"`
		Messages []json.RawMessage `json:"messages"`
		Metadata json.RawMessage   `json:"metadata"`
	}
	if err := json.Unmarshal(trimmed, &got); err != nil {
		t.Fatalf("decode trimmed body: %v", err)
	}
	if got.Model != "claude-opus-4-8" || got.System != "sys" {
		t.Fatalf("non-message fields not preserved: model=%q system=%q", got.Model, got.System)
	}
	if len(got.Metadata) == 0 {
		t.Fatal("metadata dropped from trimmed request")
	}
	if len(got.Messages) != 3 {
		t.Fatalf("trimmed messages = %d, want 3 (m1, m2, prompt)", len(got.Messages))
	}
	trimmedStr := string(trimmed)
	if strings.Contains(trimmedStr, "m3-recent") || strings.Contains(trimmedStr, "m4-newest") {
		t.Fatalf("trimmed request still carries the recent half: %s", trimmedStr)
	}
	if !strings.Contains(trimmedStr, "m1-oldest") || !strings.Contains(trimmedStr, "m2-old") {
		t.Fatalf("trimmed request dropped the older half: %s", trimmedStr)
	}
	if !strings.Contains(trimmedStr, "create a detailed summary") {
		t.Fatalf("trimmed request dropped the compaction prompt: %s", trimmedStr)
	}
}

func TestSplitRequestTransformerFailsOpen(t *testing.T) {
	t.Parallel()
	transformer := messageTrimTransformer{keep: []int{0, 1}}
	body := []byte(`{not valid json`)
	out, changed, err := transformer.TransformRequest(context.Background(), body)
	if err == nil {
		t.Fatal("expected an error on an undecodable body")
	}
	if changed {
		t.Fatal("changed = true, want false on failure (fail-open)")
	}
	if string(out) != string(body) {
		t.Fatalf("out = %q, want the original body unchanged", out)
	}
}

func TestSplitConversationClampedByCap(t *testing.T) {
	t.Parallel()
	assistant := anthropicMessage{Role: "assistant", Content: json.RawMessage(`"` + strings.Repeat("x", 100) + `"`)}
	user := anthropicMessage{Role: "user", Content: json.RawMessage(`"u"`)}
	// Eight conversation messages alternating user/assistant, each assistant ~100
	// bytes, then the prompt at index 8. A 150-byte cap fits far fewer than the
	// count midpoint (4), so the recent half is clamped, and after the boundary snap
	// the older half still ends on an assistant.
	messages := []anthropicMessage{user, assistant, user, assistant, user, assistant, user, assistant, user}
	promptIndex := 8
	recentStart, ok := splitConversation(messages, promptIndex, 150)
	if !ok {
		t.Fatal("expected a split")
	}
	if messages[recentStart-1].Role != "assistant" {
		t.Fatalf("older half ends on %q, want an assistant", messages[recentStart-1].Role)
	}
	if recentStart <= 4 {
		t.Fatalf("recentStart = %d, want > 4 (the cap clamped the recent half below the midpoint)", recentStart)
	}
}

func TestHookSmallConversationFallsBackToProvider(t *testing.T) {
	t.Parallel()
	hook := New(fixedContentProvider("disk-recovered"), 0)
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{},
		Body:   staticHookBody{body: []byte(compactTinyBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if !match.Matched {
		t.Fatal("expected a match")
	}
	if match.RequestTransformer != nil {
		t.Fatal("expected no request transformer on a too-small conversation")
	}
	appender, ok := match.Transformer.(responseAppendTransformer)
	if !ok {
		t.Fatalf("transformer type = %T", match.Transformer)
	}
	if appender.content != "" {
		t.Fatalf("expected empty content on the provider fallback, got %q", appender.content)
	}
	if appender.provider == nil {
		t.Fatal("expected the disk provider to be set on the fallback path")
	}
}

func TestSplitConversationBudgetsToolBlocks(t *testing.T) {
	t.Parallel()
	bigInput := `{"command":"` + strings.Repeat("x", 300) + `"}`
	messages := []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"m1"`)},
		{Role: "assistant", Content: json.RawMessage(`"m2"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","name":"Bash","input":` + bigInput + `}]`)},
		{Role: "user", Content: json.RawMessage(`"m4"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Your task is to create a detailed summary"}]`)},
	}
	promptIndex := 4
	// A 50-byte cap fits only the tiny newest message. The tool_use message renders
	// to ~330 bytes via renderBlocks, so it must be excluded even though text() would
	// count it as zero.
	recentStart, ok := splitConversation(messages, promptIndex, 50)
	if !ok {
		t.Fatal("expected a split")
	}
	if recentStart != 3 {
		t.Fatalf("recentStart = %d, want 3 (only the newest message fits; the tool_use message is budgeted by renderBlocks, not text)", recentStart)
	}
}

func TestSplitConversationEndsOlderHalfOnAssistant(t *testing.T) {
	t.Parallel()
	msg := func(role string) anthropicMessage {
		return anthropicMessage{Role: role, Content: json.RawMessage(`"x"`)}
	}
	// The count boundary would land right after the system at index 2, leaving the
	// older half ending on a Claude Code system-reminder, which Anthropic rejects
	// (a system message must precede an assistant or end the array). The split must
	// snap back so the older half ends on the assistant at index 1.
	messages := []anthropicMessage{
		msg("user"), msg("assistant"), msg("system"),
		msg("assistant"), msg("user"), msg("system"),
		msg("user"),
	}
	promptIndex := 6
	recentStart, ok := splitConversation(messages, promptIndex, 0)
	if !ok {
		t.Fatal("expected a split")
	}
	if messages[recentStart-1].Role != "assistant" {
		t.Fatalf("older half ends on %q at index %d, want it to end on an assistant so no orphaned system reminder is left", messages[recentStart-1].Role, recentStart-1)
	}
	if recentStart != 2 {
		t.Fatalf("recentStart = %d, want 2 (snapped back past the system at index 2)", recentStart)
	}
}

func TestSplitConversationFallsBackWithoutAssistantBoundary(t *testing.T) {
	t.Parallel()
	msg := func(role string) anthropicMessage {
		return anthropicMessage{Role: role, Content: json.RawMessage(`"x"`)}
	}
	// A leading run with no assistant to end the older half on: the split cannot
	// form a valid older half, so it must fall back to no split rather than emit an
	// invalid message sequence.
	messages := []anthropicMessage{
		msg("user"), msg("system"), msg("user"), msg("system"),
		msg("user"),
	}
	promptIndex := 4
	if _, ok := splitConversation(messages, promptIndex, 0); ok {
		t.Fatal("expected no split when the older half cannot end on an assistant")
	}
}

func TestHookDetectionTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"prompt is final user message (headless)", compactRequestBody, true},
		{"prompt then one trailing system message (interactive)", compactWithTrailingSystemBody, true},
		{"prompt then multiple trailing non-user messages", compactWithMultipleTrailingBody, true},
		{"normal turn with ordinary last user message", normalRequestBody, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := New(fixedContentProvider("recovered"), 0)
			match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
				Method: http.MethodPost,
				Path:   "/v1/messages",
				Header: http.Header{},
				Body:   staticHookBody{body: []byte(tc.body)},
			})
			if err != nil {
				t.Fatalf("MatchRequestResponse err = %v", err)
			}
			if match.Matched != tc.want {
				t.Fatalf("Matched = %v, want %v", match.Matched, tc.want)
			}
		})
	}
}

func TestHookMatchesCompactionWithTrailingSystemMessage(t *testing.T) {
	t.Parallel()
	// Interactive Claude Code appends a system-reminder after the compaction
	// prompt, so the final message is not the prompt. Detection must scan back to
	// the last user message and still match.
	hook := New(fixedContentProvider("recovered"), 0)
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Header: http.Header{},
		Body:   staticHookBody{body: []byte(compactWithTrailingSystemBody)},
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse err = %v", err)
	}
	if !match.Matched {
		t.Fatal("a compaction request with a trailing system message must match")
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
	hook := New(fixedContentProvider("recovered"), 0)
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
	hook := New(fixedContentProvider("recovered"), 0)
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
	hook := New(fixedContentProvider("recovered"), 0)
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
	hook := New(fixedContentProvider("recovered"), 0)
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
	transformer := responseAppendTransformer{provider: provider, sessionID: "sess-abc", maxBytes: 1234}
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
	if !strings.Contains(body, reorienttag.PreCompactionTranscriptOpen) || !strings.Contains(body, reorienttag.PreCompactionTranscriptClose) {
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
	if !strings.Contains(body, "RECOVERED-TRANSCRIPT") || !strings.Contains(body, reorienttag.PreCompactionTranscriptOpen) {
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
	failing := func(context.Context, string, int) (string, error) {
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

func TestTransformForwardsMaxBytesToProvider(t *testing.T) {
	t.Parallel()
	const wantMaxBytes = 4321
	gotMaxBytes := 0
	provider := func(_ context.Context, _ string, maxBytes int) (string, error) {
		gotMaxBytes = maxBytes
		return "RECOVERED", nil
	}
	transformer := responseAppendTransformer{
		provider:  provider,
		sessionID: "sess-abc",
		maxBytes:  wantMaxBytes,
	}
	_, err := transformer.TransformResponse(
		context.Background(),
		eventStreamResponse(summarySSEResponse),
	)
	if err != nil {
		t.Fatalf("TransformResponse err = %v", err)
	}
	if gotMaxBytes != wantMaxBytes {
		t.Fatalf("provider maxBytes = %d, want %d", gotMaxBytes, wantMaxBytes)
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
