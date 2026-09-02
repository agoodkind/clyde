package codex

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestInjectRawResponsesCompactionV2Recovery(t *testing.T) {
	now := time.Unix(1, 0)
	registry := NewRawResponsesCompactionV2Registry(func() time.Time { return now })
	if !registry.Arm("session-1", "cipher", "recovered transcript") {
		t.Fatal("arm registry")
	}
	request := RawResponsesRequest{
		Body:   []byte(`{"model":"gpt-native","opaque":{"keep":true},"input":[ { "type":"message","role":"user","content":[{"type":"input_text","text":"next turn"}]}, {"type":"compaction","encrypted_content":"cipher","opaque":true}, {"type":"reasoning","summary":[]} ]}`),
		Header: http.Header{CodexTurnMetadataHeader: {`{"session_id":"session-1","thread_source":"user","sandbox":"none"}`}},
		Stream: true,
	}

	transformed, recovery, changed := InjectRawResponsesCompactionV2Recovery(request, registry)
	if !changed || recovery == nil {
		t.Fatal("matching request did not inject recovery")
	}
	if !bytes.Contains(transformed.Body, []byte(`"opaque":{"keep":true}`)) {
		t.Fatalf("unrelated fields changed: %s", transformed.Body)
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(transformed.Body, &body); err != nil {
		t.Fatalf("decode transformed request: %v", err)
	}
	var injected struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body.Input[2], &injected); err != nil {
		t.Fatalf("decode injected item: %v", err)
	}
	if len(body.Input) != 4 || injected.Role != "assistant" || len(injected.Content) != 1 || injected.Content[0].Text != wrappedRawCompactionTranscript("recovered transcript") {
		t.Fatalf("input = %s", transformed.Body)
	}
	if !bytes.Contains(body.Input[1], []byte(`"encrypted_content":"cipher"`)) || !bytes.Contains(body.Input[1], []byte(`"opaque":true`)) {
		t.Fatalf("encrypted item changed: %s", body.Input[1])
	}
}

func TestInjectRawResponsesCompactionV2RecoveryFailsOpen(t *testing.T) {
	now := time.Unix(1, 0)
	registry := NewRawResponsesCompactionV2Registry(func() time.Time { return now })
	if !registry.Arm("session-1", "cipher", "recovered transcript") {
		t.Fatal("arm registry")
	}
	base := RawResponsesRequest{
		Body:   []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`),
		Header: http.Header{CodexTurnMetadataHeader: {`{"session_id":"session-1","thread_source":"user","sandbox":"none"}`}},
	}
	tests := []struct {
		name    string
		request RawResponsesRequest
	}{
		{name: "wrong session", request: RawResponsesRequest{Body: base.Body, Header: http.Header{CodexTurnMetadataHeader: {`{"session_id":"other","thread_source":"user","sandbox":"none"}`}}}},
		{name: "wrong digest", request: RawResponsesRequest{Body: []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"other"}]}`), Header: base.Header}},
		{name: "malformed request", request: RawResponsesRequest{Body: []byte(`{"input":[`), Header: base.Header}},
		{name: "duplicate compaction", request: RawResponsesRequest{Body: []byte(`{"input":[{"type":"compaction","encrypted_content":"cipher"},{"type":"compaction","encrypted_content":"cipher"}]}`), Header: base.Header}},
		{name: "existing transcript", request: RawResponsesRequest{Body: []byte(`{"input":[{"type":"compaction","encrypted_content":"cipher"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<pre-compaction-transcript>kept</pre-compaction-transcript>"}]}]}`), Header: base.Header}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, recovery, changed := InjectRawResponsesCompactionV2Recovery(testCase.request, registry)
			if changed || recovery != nil || !bytes.Equal(got.Body, testCase.request.Body) {
				t.Fatalf("request changed: %s", got.Body)
			}
		})
	}
	now = now.Add(rawResponsesCompactionV2TTL)
	got, recovery, changed := InjectRawResponsesCompactionV2Recovery(base, registry)
	if changed || recovery != nil || !bytes.Equal(got.Body, base.Body) {
		t.Fatalf("expired request changed: %s", got.Body)
	}
}

func TestInjectRawResponsesCompactionV2RecoveryCompletionCapability(t *testing.T) {
	registry := NewRawResponsesCompactionV2Registry(nil)
	if !registry.Arm("session-1", "cipher", "recovered transcript") || !registry.Arm("session-1", "other", "other transcript") {
		t.Fatal("arm registry")
	}
	request := RawResponsesRequest{
		Body:   []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`),
		Header: http.Header{CodexTurnMetadataHeader: {`{"session_id":"session-1","thread_source":"user","sandbox":"none"}`}},
	}
	_, recovery, changed := InjectRawResponsesCompactionV2Recovery(request, registry)
	if !changed || recovery == nil {
		t.Fatal("matching request did not create recovery")
	}
	if _, ok := registry.Match("session-1", "cipher"); ok {
		t.Fatal("injected recovery did not retain its reservation")
	}
	if !recovery.CompleteRecovery() {
		t.Fatal("completion capability did not complete matching recovery")
	}
	if _, ok := registry.Match("session-1", "cipher"); ok {
		t.Fatal("matching recovery remained")
	}
	if transcript, ok := registry.Match("session-1", "other"); !ok || transcript != "other transcript" {
		t.Fatalf("completion affected another entry: %q, %t", transcript, ok)
	}
	if recovery.CompleteRecovery() {
		t.Fatal("completion capability completed twice")
	}
}

func TestInjectRawResponsesCompactionV2RecoveryReleaseCapability(t *testing.T) {
	registry := NewRawResponsesCompactionV2Registry(nil)
	if !registry.Arm("session-1", "cipher", "recovered transcript") {
		t.Fatal("arm registry")
	}
	request := RawResponsesRequest{
		Body:   []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`),
		Header: http.Header{CodexTurnMetadataHeader: {`{"session_id":"session-1","thread_source":"user","sandbox":"none","request_kind":"turn","compaction":{"phase":"final_answer"}}`}},
	}
	_, recovery, changed := InjectRawResponsesCompactionV2Recovery(request, registry)
	if !changed || recovery == nil {
		t.Fatal("matching request did not create recovery")
	}
	if !recovery.ReleaseRecovery() {
		t.Fatal("release capability did not release matching recovery")
	}
	if recovery.CompleteRecovery() {
		t.Fatal("completion succeeded after release")
	}
	if transcript, ok := registry.Match("session-1", "cipher"); !ok || transcript != "recovered transcript" {
		t.Fatalf("released recovery did not become available: %q, %t", transcript, ok)
	}
	_, generation, ok := registry.Reserve("session-1", "cipher")
	if !ok || !registry.Complete("session-1", "cipher", generation) {
		t.Fatal("complete reserved released recovery")
	}
}
