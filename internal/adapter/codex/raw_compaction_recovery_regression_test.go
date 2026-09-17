package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRawResponsesCompactionV2RecoveryResponseRejectsNonfinalOutput(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status string
		item   string
	}{
		{name: "failed", status: "failed", item: recoveryFinalTextItem("final_answer")},
		{name: "incomplete", status: "incomplete", item: recoveryFinalTextItem("final_answer")},
		{name: "missing status", status: "", item: recoveryFinalTextItem("final_answer")},
		{name: "commentary", status: "completed", item: recoveryFinalTextItem("commentary")},
		{name: "unknown phase", status: "completed", item: recoveryFinalTextItem("unknown")},
		{name: "empty phase", status: "completed", item: recoveryFinalTextItem("")},
		{name: "refusal", status: "completed", item: `{"id":"message-1","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"refusal","refusal":"declined"}]}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", testCase.name, stream), func(t *testing.T) {
				body := recoveryResponseFixture(testCase.status, testCase.item, stream)
				assertRecoveryResponsePreserved(t, body, stream)
			})
		}
	}
}

func TestRawResponsesCompactionV2RecoveryResponseRejectsDuplicateFields(t *testing.T) {
	item := recoveryFinalTextItem("final_answer")
	for _, testCase := range []struct {
		name string
		item string
	}{
		{name: "text", item: strings.Replace(item, `"text":"answer"`, `"text":"ignored-first","text":"client-visible-last"`, 1)},
		{name: "escaped text", item: strings.Replace(item, `"text":"answer"`, `"text":"ignored-first","te\u0078t":"client-visible-last"`, 1)},
		{name: "content", item: strings.Replace(item, `"content":`, `"content":[],"content":`, 1)},
		{name: "role", item: strings.Replace(item, `"role":"assistant"`, `"role":"user","role":"assistant"`, 1)},
		{name: "type", item: strings.Replace(item, `"type":"message"`, `"type":"reasoning","type":"message"`, 1)},
		{name: "phase", item: strings.Replace(item, `"phase":"final_answer"`, `"phase":"commentary","phase":"final_answer"`, 1)},
		{name: "mixed case phase", item: strings.Replace(item, `"phase":"final_answer"`, `"phase":"commentary","Phase":"final_answer"`, 1)},
		{name: "mixed case text", item: strings.Replace(item, `"text":"answer"`, `"text":"ignored-first","Text":"client-visible-last"`, 1)},
		{name: "id", item: strings.Replace(item, `"id":"message-1"`, `"id":"other","id":"message-1"`, 1)},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", testCase.name, stream), func(t *testing.T) {
				assertRecoveryResponsePreserved(t, recoveryResponseFixture("completed", testCase.item, stream), stream)
			})
		}
	}
	for _, field := range []string{"output", "status"} {
		t.Run(field, func(t *testing.T) {
			body := recoveryResponseFixture("completed", item, false)
			body = strings.Replace(body, `"`+field+`":`, `"`+field+`":null,"`+field+`":`, 1)
			assertRecoveryResponsePreserved(t, body, false)
		})
	}
}

func TestRawResponsesCompactionPreservesEscapedUnrelatedKeys(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize"}]}],"opaque\/key":{"kept":true}}`)
	request, transformer := PrepareRawResponsesCompaction(rawCompactionRequest(t, body), RawResponsesCompactionSettings{Enabled: true})
	if transformer == nil || bytes.Equal(request.Body, body) {
		t.Fatal("escaped unrelated request key prevented compaction")
	}
	if !bytes.Contains(request.Body, []byte(`"opaque\/key":{"kept":true}`)) {
		t.Fatalf("escaped request key changed: %s", request.Body)
	}
	original := `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}],"opaque\/key":{"kept":true}}`
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(original))}
	got := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Contains(got, []byte("recent user")) || !bytes.Contains(got, []byte(`"opaque\/key":{"kept":true}`)) {
		t.Fatalf("escaped unrelated response key prevented recovery or changed: %s", got)
	}
}

func TestInjectRawResponsesCompactionV2RecoveryRejectsDuplicateFields(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"compaction","encrypted_content":"cipher"}],"input":[]}`,
		`{"input":[{"type":"compaction","type":"compaction","encrypted_content":"cipher"}]}`,
		`{"input":[{"type":"compaction","encrypted_content":"other","encrypted_content":"cipher"}]}`,
	} {
		registry := NewRawResponsesCompactionV2Registry(nil)
		if !registry.Arm("session-1", "cipher", "recovery-probe") {
			t.Fatal("arm registry")
		}
		request := recoveryRequestFixture(body, false)
		got, recovery, changed := InjectRawResponsesCompactionV2Recovery(request, registry)
		if changed || recovery != nil || !bytes.Equal(got.Body, request.Body) {
			t.Fatalf("ambiguous request changed: %s", got.Body)
		}
		if _, _, reserved := registry.Reserve("session-1", "cipher"); !reserved {
			t.Fatal("ambiguous request consumed or reserved recovery")
		}
	}
}

func TestPlanRawResponsesCompactionV2RetainsUnfinishedCommentaryTurn(t *testing.T) {
	request := rawResponsesCompactionV2DeveloperRequest(t, "recent instructions")
	unfinished := `{"type":"message","role":"user","content":[{"type":"input_text","text":"current-user"}]},{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"still-working"}]},{"type":"custom_tool_call","call_id":"pending","name":"exec_command","input":"run"}`
	request.Body = bytes.Replace(request.Body, []byte(`{"type":"compaction_trigger"}`), []byte(unfinished+`,{"type":"compaction_trigger"}`), 1)
	plan, ok := PlanRawResponsesCompactionV2(request, RawResponsesCompactionSettings{Enabled: true, RecentFraction: 0.5})
	if !ok {
		t.Fatal("completed recent turn should remain removable")
	}
	for _, text := range []string{"current-user", "still-working", `"call_id":"pending"`} {
		if !bytes.Contains(plan.Request.Body, []byte(text)) || strings.Contains(plan.Transcript, text) {
			t.Fatalf("unfinished turn moved to recovery: request=%s transcript=%s", plan.Request.Body, plan.Transcript)
		}
	}
	if !strings.Contains(plan.Transcript, "recent instructions") {
		t.Fatal("completed recent turn was not selected")
	}
}

func TestPlanRawResponsesCompactionV2HonorsConfiguredBudget(t *testing.T) {
	request := rawResponsesCompactionV2DeveloperRequest(t, strings.Repeat("x", 4096))
	for _, settings := range []RawResponsesCompactionSettings{
		{Enabled: true, MaxTokens: 64, ContextWindowTokens: 10_000, ContextWindowFraction: 1, BytesPerToken: 1},
		{Enabled: true, ContextWindowTokens: 128, ContextWindowFraction: 0.5, BytesPerToken: 1},
		{Enabled: true, FallbackContextWindowTokens: 128, ContextWindowFraction: 0.5, BytesPerToken: 1},
		{Enabled: true, MaxTokens: 32, ContextWindowTokens: 10_000, ContextWindowFraction: 1, BytesPerToken: 2},
	} {
		if plan, ok := PlanRawResponsesCompactionV2(request, settings); ok {
			t.Fatalf("selected %d-byte whole turn with 64-byte budget", len(plan.Transcript))
		}
	}
	if _, ok := PlanRawResponsesCompactionV2(request, RawResponsesCompactionSettings{Enabled: true, MaxTokens: 8192, ContextWindowTokens: 8192, ContextWindowFraction: 1, BytesPerToken: 1}); !ok {
		t.Fatal("larger configured budget rejected complete turn")
	}
}

func recoveryRequestFixture(body string, stream bool) RawResponsesRequest {
	return RawResponsesRequest{
		Body:   []byte(body),
		Header: http.Header{CodexTurnMetadataHeader: {`{"session_id":"session-1","request_kind":"turn","compaction":{"phase":"final_answer"}}`}},
		Stream: stream,
	}
}

func recoveryFinalTextItem(phase string) string {
	return `{"id":"message-1","type":"message","role":"assistant","phase":"` + phase + `","content":[{"type":"output_text","text":"answer"}]}`
}

func recoveryResponseFixture(status, item string, stream bool) string {
	body := `{"output":[` + item + `]}`
	if status != "" {
		body = `{"status":"` + status + `","output":[` + item + `]}`
	}
	if !stream {
		return body
	}
	return "event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"sequence_number":1,"item":` + item + "}\n\nevent: response.completed\ndata: " + `{"type":"response.completed","sequence_number":2,"response":` + body + "}\n\n"
}

func assertRecoveryResponsePreserved(t *testing.T, original string, stream bool) {
	t.Helper()
	registry := NewRawResponsesCompactionV2Registry(nil)
	if !registry.Arm("session-1", "cipher", "recovery-probe") {
		t.Fatal("arm registry")
	}
	request := recoveryRequestFixture(`{"input":[{"type":"compaction","encrypted_content":"cipher"}]}`, stream)
	_, recovery, changed := InjectRawResponsesCompactionV2Recovery(request, registry)
	if !changed || recovery == nil {
		t.Fatal("reserve recovery through request injection")
	}
	transformer := NewRawResponsesCompactionV2FinalAnswerTransformer(request, recovery)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(original))}
	got := readResponseBody(t, transformer.TransformResponse(response))
	if !stream && !json.Valid(got) {
		t.Fatalf("invalid delivered response: %s", got)
	}
	if transformer.DidMutateResponse() {
		recovery.CompleteRecovery()
	} else {
		recovery.ReleaseRecovery()
	}
	if !bytes.Equal(got, []byte(original)) || transformer.DidMutateResponse() {
		t.Fatalf("response changed: %s", got)
	}
	if _, _, reserved := registry.Reserve("session-1", "cipher"); !reserved {
		t.Fatal("unsuitable response consumed pending recovery")
	}
}
