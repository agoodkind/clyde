//go:build live

package live

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLiveResponsesCaptureAndStreaming(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, upstream.baseURL())
	h.boot(t)

	nonStreamingProbe := uniqueProbeID(t, "non-streaming")
	nonStreamingBody := responsesRequestBody(nonStreamingProbe, false)
	nonStreamingResponse := h.postResponses(t, h.cfg.AdapterPort, nonStreamingBody)
	if nonStreamingResponse.statusCode != 200 {
		t.Fatalf("non-streaming status = %d, body = %s", nonStreamingResponse.statusCode, nonStreamingResponse.body)
	}
	if !strings.Contains(nonStreamingResponse.body, nonStreamingProbe) {
		t.Fatalf("non-streaming response missing probe %q: %s", nonStreamingProbe, nonStreamingResponse.body)
	}
	request := upstream.waitForRequest(t, 10*time.Second)
	assertResponsesPassthroughRequest(t, request, nonStreamingProbe, false)
	assertResponsesCaptures(t, h.waitForResponsesCaptures(t, nonStreamingProbe, 10*time.Second), nonStreamingProbe, false)

	streamingProbe := uniqueProbeID(t, "streaming")
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, responsesRequestBody(streamingProbe, true))
	firstBytes := stream.waitForFirstFrame(t, 10*time.Second)
	if !strings.Contains(firstBytes, "response.created") {
		t.Fatalf("first streaming bytes missing response.created: %s", firstBytes)
	}
	if strings.Contains(firstBytes, "response.completed") {
		t.Fatalf("stream completed before upstream release: %s", firstBytes)
	}
	streamRequest := upstream.waitForRequest(t, 10*time.Second)
	assertResponsesPassthroughRequest(t, streamRequest, streamingProbe, true)

	upstream.releaseStreams()
	streamingResponse := stream.waitForCompletion(t, 10*time.Second)
	for _, frame := range []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"} {
		if !strings.Contains(streamingResponse, frame) {
			t.Fatalf("stream missing %s: %s", frame, streamingResponse)
		}
	}
	if strings.Contains(streamingResponse, "[DONE]") {
		t.Fatalf("Responses stream contains Chat sentinel [DONE]: %s", streamingResponse)
	}
	assertResponsesCaptures(t, h.waitForResponsesCaptures(t, streamingProbe, 10*time.Second), streamingProbe, true)
}

func TestLiveResponsesStreamDefersReloadUntilCompletion(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, upstream.baseURL())
	h.boot(t)

	probeID := uniqueProbeID(t, "reload")
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, responsesRequestBody(probeID, true))
	firstBytes := stream.waitForFirstFrame(t, 10*time.Second)
	if !strings.Contains(firstBytes, "response.created") {
		t.Fatalf("first streaming bytes missing response.created: %s", firstBytes)
	}
	_ = upstream.waitForRequest(t, 10*time.Second)

	h.writeCombinedReloadEdit(t, upstream.baseURL())
	if !h.waitForDaemonLog(`"route":"reload"`, 10*time.Second) {
		t.Fatal("Responses reload-routed edit was not classified as reload")
	}
	time.Sleep(time.Second)
	if h.logContains(reloadTriggeredKey) {
		t.Fatal("reload fired while a Responses stream was still in flight")
	}

	upstream.releaseStreams()
	completed := stream.waitForCompletion(t, 10*time.Second)
	if !strings.Contains(completed, "response.completed") {
		t.Fatalf("released Responses stream did not complete: %s", completed)
	}
	if !h.waitForDaemonLog(reloadTriggeredKey, 15*time.Second) {
		t.Fatal("reload did not fire after the Responses stream completed")
	}
}

func responsesRequestBody(probeID string, stream bool) string {
	return fmt.Sprintf(`{"model":"local-test","input":"probe %s","stream":%t,"temperature":0.25,"parallel_tool_calls":true,"compat_unknown":{"probe":"%s","mode":"preserve"}}`, probeID, stream, probeID)
}

func assertResponsesPassthroughRequest(t *testing.T, request responsesUpstreamRequest, probeID string, stream bool) {
	t.Helper()
	if request.path != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", request.path)
	}
	for _, exactField := range []string{
		fmt.Sprintf(`"input":"probe %s"`, probeID),
		fmt.Sprintf(`"stream":%t`, stream),
		`"temperature":0.25`,
		`"parallel_tool_calls":true`,
		fmt.Sprintf(`"compat_unknown":{"probe":"%s","mode":"preserve"}`, probeID),
	} {
		if !strings.Contains(request.body, exactField) {
			t.Fatalf("upstream request missing preserved field %s: %s", exactField, request.body)
		}
	}
}

func assertResponsesCaptures(t *testing.T, captures []responsesCapture, probeID string, stream bool) {
	t.Helper()
	if len(captures) != 2 {
		t.Fatalf("capture count = %d, want ingress and passthrough rows: %+v", len(captures), captures)
	}
	wantPaths := map[string]string{
		"adapter.ingress":     "/v1/responses",
		"adapter.passthrough": "/v1/responses",
	}
	for _, capture := range captures {
		if capture.path != wantPaths[capture.client] {
			t.Fatalf("%s capture path = %q, want %q", capture.client, capture.path, wantPaths[capture.client])
		}
		if capture.status != 200 {
			t.Fatalf("%s capture status = %d", capture.client, capture.status)
		}
		if !strings.Contains(capture.requestBody, probeID) {
			t.Fatalf("%s request capture missing probe %q: %s", capture.client, probeID, capture.requestBody)
		}
		if !strings.Contains(capture.responseBody, probeID) {
			t.Fatalf("%s response capture missing probe %q: %s", capture.client, probeID, capture.responseBody)
		}
		if !strings.Contains(capture.responseHeaders, "X-Upstream-Marker") {
			t.Fatalf("%s response capture missing upstream marker: %s", capture.client, capture.responseHeaders)
		}
		if strings.Contains(capture.responseHeaders, "X-Clyde-Warning") {
			t.Fatalf("%s passthrough capture unexpectedly recorded a compatibility warning: %s", capture.client, capture.responseHeaders)
		}
		if stream && !strings.Contains(capture.responseBody, "response.completed") {
			t.Fatalf("%s stream capture missing response.completed: %s", capture.client, capture.responseBody)
		}
	}
}
