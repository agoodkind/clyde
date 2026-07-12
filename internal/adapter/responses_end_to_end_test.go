package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

// postResponses issues a POST to the live adapter listener and returns the
// status, response headers, and full body. It reads the body to completion, so
// a streaming Responses SSE body arrives as the concatenated frames the mock
// upstream wrote before it closed.
func postResponses(t *testing.T, url string, body string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new responses request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", url, err)
	}
	return response.StatusCode, response.Header, responseBody
}

// drainCodex consumes one recorded codex upstream request, or fails if the mock
// received none within the timeout (which means the request never dispatched to
// the codex backend).
func drainCodex(t *testing.T, ch <-chan adaptercodex.HTTPTransportRequest) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("codex mock upstream received no request")
	}
}

// TestResponsesEndpointEndToEnd drives POST /v1/responses through the real
// adapter server against the mock codex and anthropic upstreams the routing
// harness stands up. It confirms the Responses object shape (non-streaming), the
// Responses SSE event sequence with no [DONE] (streaming), and the
// X-Clyde-Warning surface for a codex request that carries an omit-and-warn
// field. It exercises the full parse, resolve, dispatch, and Responses render
// path with no real provider traffic.
func TestResponsesEndpointEndToEnd(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, _ := startRoutingListeners(t, srv)

	// Codex non-streaming: the mock streams response.output_text.delta "ok", so
	// the Responses object carries a message output item with that text.
	status, _, body := postResponses(t, openAIURL+"/v1/responses",
		`{"model":"gpt-future","input":"say hi","stream":false}`)
	if status != http.StatusOK {
		t.Fatalf("codex responses status = %d; body=%s", status, body)
	}
	var object struct {
		Object string `json:"object"`
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode responses object: %v; body=%s", err, body)
	}
	if object.Object != "response" {
		t.Fatalf("object = %q, want response; body=%s", object.Object, body)
	}
	if len(object.ID) < 5 || object.ID[:5] != "resp_" {
		t.Fatalf("id = %q, want resp_ prefix", object.ID)
	}
	if object.ID == "resp-routing" || bytes.Contains(body, []byte(`"resp-routing"`)) {
		t.Fatalf("provider response id escaped into the public Responses object: %s", body)
	}
	if !bytes.Contains(body, []byte(`"output_text"`)) || !bytes.Contains(body, []byte(`"ok"`)) {
		t.Fatalf("codex responses object missing output_text ok; body=%s", body)
	}
	drainCodex(t, fakes.codexReqs)

	// Codex non-streaming with a temperature the codex backend omits: a
	// compatibility warning must appear as an X-Clyde-Warning header and under
	// clyde.warnings in the object.
	status, header, body := postResponses(t, openAIURL+"/v1/responses",
		`{"model":"gpt-future","input":"say hi","temperature":0.5,"stream":false}`)
	if status != http.StatusOK {
		t.Fatalf("codex warn status = %d; body=%s", status, body)
	}
	warning := header.Get("X-Clyde-Warning")
	if warning == "" {
		t.Fatalf("expected X-Clyde-Warning header for codex temperature; headers=%v", header)
	}
	if !bytes.Contains([]byte(warning), []byte("temperature")) {
		t.Fatalf("X-Clyde-Warning = %q, want it to name temperature", warning)
	}
	if !bytes.Contains(body, []byte(`"clyde"`)) || !bytes.Contains(body, []byte(`"warnings"`)) {
		t.Fatalf("codex responses object missing clyde.warnings; body=%s", body)
	}
	drainCodex(t, fakes.codexReqs)

	// Codex streaming: the Responses SSE sequence with named events and no [DONE].
	status, _, body = postResponses(t, openAIURL+"/v1/responses",
		`{"model":"gpt-future","input":"say hi","stream":true}`)
	if status != http.StatusOK {
		t.Fatalf("codex stream status = %d; body=%s", status, body)
	}
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.completed",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("codex responses stream missing %q; body=%s", want, body)
		}
	}
	if bytes.Contains(body, []byte("[DONE]")) {
		t.Fatalf("codex responses stream must not emit [DONE]; body=%s", body)
	}
	drainCodex(t, fakes.codexReqs)

	// Anthropic non-streaming: the anthropic provider returns a FinalResponse
	// rather than streamed events, so this confirms the Responses collector
	// assembles the object from the provider's final response too.
	status, _, body = postResponses(t, openAIURL+"/v1/responses",
		`{"model":"claude-future","input":"say hi","stream":false}`)
	if status != http.StatusOK {
		t.Fatalf("anthropic responses status = %d; body=%s", status, body)
	}
	var anthObject struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal(body, &anthObject); err != nil {
		t.Fatalf("decode anthropic responses object: %v; body=%s", err, body)
	}
	if anthObject.Object != "response" {
		t.Fatalf("anthropic object = %q, want response; body=%s", anthObject.Object, body)
	}
	if !bytes.Contains(body, []byte(`"ok"`)) {
		t.Fatalf("anthropic responses object missing FinalResponse text ok; body=%s", body)
	}
	select {
	case <-fakes.anthReqs:
	case <-time.After(3 * time.Second):
		t.Fatal("anthropic mock upstream received no request")
	}
}
