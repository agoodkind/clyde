//go:build live

package live

import (
	"fmt"
	"io"
	"net/http"
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
		t.Fatalf("non-streaming probe %q status = %d, response length = %d", nonStreamingProbe, nonStreamingResponse.statusCode, len(nonStreamingResponse.body))
	}
	if !strings.Contains(nonStreamingResponse.body, nonStreamingProbe) {
		t.Fatalf("non-streaming response missing probe %q; response length = %d", nonStreamingProbe, len(nonStreamingResponse.body))
	}
	assertResponsesResponseMetadata(t, nonStreamingResponse.requestID, nonStreamingResponse.body, nonStreamingProbe)
	request := upstream.waitForRequest(t, 10*time.Second)
	assertResponsesPassthroughRequest(t, request, nonStreamingBody, nonStreamingProbe, false)
	assertResponsesCaptures(t, h.waitForResponsesCaptures(t, nonStreamingProbe, 10*time.Second), nonStreamingProbe, false)

	streamingProbe := uniqueProbeID(t, "streaming")
	streamingBody := responsesRequestBody(streamingProbe, true)
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, streamingBody)
	firstBytes := stream.waitForFirstFrame(t, 10*time.Second)
	if !strings.Contains(firstBytes, "response.created") {
		t.Fatalf("streaming probe %q first frame missing field response.created; frame length = %d", streamingProbe, len(firstBytes))
	}
	if strings.Contains(firstBytes, "response.completed") {
		t.Fatalf("streaming probe %q completed before upstream release; frame length = %d", streamingProbe, len(firstBytes))
	}
	streamRequest := upstream.waitForRequest(t, 10*time.Second)
	assertResponsesPassthroughRequest(t, streamRequest, streamingBody, streamingProbe, true)

	upstream.releaseStreams()
	streamingResponse := stream.waitForCompletion(t, 10*time.Second)
	for _, frame := range []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"} {
		if !strings.Contains(streamingResponse, frame) {
			t.Fatalf("streaming probe %q missing field %s; response length = %d", streamingProbe, frame, len(streamingResponse))
		}
	}
	if strings.Contains(streamingResponse, "[DONE]") {
		t.Fatalf("streaming probe %q contains field chat_done_sentinel; response length = %d", streamingProbe, len(streamingResponse))
	}
	assertResponsesResponseMetadata(t, stream.requestID, streamingResponse, streamingProbe)
	assertResponsesCaptures(t, h.waitForResponsesCaptures(t, streamingProbe, 10*time.Second), streamingProbe, true)
}

func TestResponsesStreamWaitsForCompleteFrame(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close() })
	t.Cleanup(func() { _ = writer.Close() })
	stream := startResponsesBodyStream(reader)
	go func() {
		_, _ = io.WriteString(writer, "event: response.created\n")
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\"}\n\n")
		_ = writer.Close()
	}()

	frame := stream.waitForFirstFrame(t, time.Second)
	if !strings.HasSuffix(frame, "\n\n") || !strings.Contains(frame, `data: {"type":"response.created"}`) {
		t.Fatalf("first SSE frame was incomplete; frame length = %d, has terminator = %t, has response.created = %t", len(frame), strings.HasSuffix(frame, "\n\n"), strings.Contains(frame, `data: {"type":"response.created"}`))
	}
}

func TestResponsesHTTPClientTimeoutPolicies(t *testing.T) {
	nonStreamingClient := newResponsesNonStreamingClient()
	if nonStreamingClient.Timeout != responsesRequestTimeout {
		t.Fatalf("non-streaming client timeout = %s, want %s", nonStreamingClient.Timeout, responsesRequestTimeout)
	}

	streamingClient := newResponsesStreamingClient()
	if streamingClient.Timeout != 0 {
		t.Fatalf("streaming client whole-request timeout = %s, want no whole-request timeout", streamingClient.Timeout)
	}
	transport, ok := streamingClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("streaming client transport type = %T, want *http.Transport", streamingClient.Transport)
	}
	if transport.ResponseHeaderTimeout != responsesResponseHeaderTimeout {
		t.Fatalf("streaming response header timeout = %s, want %s", transport.ResponseHeaderTimeout, responsesResponseHeaderTimeout)
	}
}

func TestResponsesCaptureValidationRequiresEachBoundary(t *testing.T) {
	tests := []struct {
		name     string
		captures []responsesCapture
	}{
		{
			name: "duplicate ingress",
			captures: []responsesCapture{
				{client: "adapter.ingress", requestBody: "request", responseBody: "body", responseHeaders: "headers"},
				{client: "adapter.ingress", requestBody: "request", responseBody: "body", responseHeaders: "headers"},
			},
		},
		{
			name: "missing ingress request body",
			captures: []responsesCapture{
				{client: "adapter.ingress", responseBody: "body", responseHeaders: "headers"},
				{client: "adapter.passthrough", requestBody: "request", responseBody: "body", responseHeaders: "headers"},
			},
		},
		{
			name: "missing ingress response body",
			captures: []responsesCapture{
				{client: "adapter.ingress", requestBody: "request", responseHeaders: "headers"},
				{client: "adapter.passthrough", requestBody: "request", responseBody: "body", responseHeaders: "headers"},
			},
		},
		{
			name: "missing passthrough response headers",
			captures: []responsesCapture{
				{client: "adapter.ingress", requestBody: "request", responseBody: "body", responseHeaders: "headers"},
				{client: "adapter.passthrough", requestBody: "request", responseBody: "body"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateResponsesCaptureBoundaries(test.captures); err == nil {
				t.Fatal("incomplete capture boundaries passed validation")
			}
		})
	}
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
		t.Fatalf("reload probe %q first frame missing field response.created; frame length = %d", probeID, len(firstBytes))
	}
	_ = upstream.waitForRequest(t, 10*time.Second)

	h.writeCombinedReloadEdit(t, upstream.baseURL())
	if !h.waitForDaemonLog(`"route":"reload"`, 10*time.Second) {
		t.Fatal("Responses reload-routed edit was not classified as reload")
	}
	assertDaemonLogAbsentWhileStreamBlocked(t, h, reloadTriggeredKey, time.Second)

	upstream.releaseStreams()
	completed := stream.waitForCompletion(t, 10*time.Second)
	if !strings.Contains(completed, "response.completed") {
		t.Fatalf("reload probe %q missing field response.completed; response length = %d", probeID, len(completed))
	}
	if !h.waitForDaemonLog(reloadTriggeredKey, 15*time.Second) {
		t.Fatal("reload did not fire after the Responses stream completed")
	}
}

func TestLiveResponsesStreamForceClosesAfterReloadDrainCap(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	h.extraEnv = []string{"CLYDE_RELOAD_QUIET_WAIT=2s"}
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, upstream.baseURL())
	h.boot(t)

	probeID := uniqueProbeID(t, "force-close")
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, responsesRequestBody(probeID, true))
	firstFrame := stream.waitForFirstFrame(t, 10*time.Second)
	if !strings.Contains(firstFrame, "response.created") {
		t.Fatalf("force-close probe %q first frame missing field response.created; frame length = %d", probeID, len(firstFrame))
	}
	_ = upstream.waitForRequest(t, 10*time.Second)

	h.writeCombinedReloadEdit(t, upstream.baseURL())
	if !h.waitForDaemonLog("daemon.config_watch.quiet_wait_timeout", 15*time.Second) {
		t.Fatal("quiet-wait cap did not fire for blocked Responses stream")
	}
	interrupted := stream.waitForCompletion(t, 70*time.Second)
	if strings.Contains(interrupted, "response.completed") {
		t.Fatalf("force-close probe %q unexpectedly contained field response.completed; response length = %d", probeID, len(interrupted))
	}
	if !h.waitForDaemonLog("livetrack.session.force_close", 10*time.Second) {
		t.Fatal("blocked Responses stream ended without a lifecycle force-close event")
	}
}

func responsesRequestBody(probeID string, stream bool) string {
	return fmt.Sprintf(`{"model":"local-test","input":"probe %s","stream":%t,"temperature":0.25,"parallel_tool_calls":true,"compat_unknown":{"probe":"%s","mode":"preserve"}}`, probeID, stream, probeID)
}

func assertDaemonLogAbsentWhileStreamBlocked(t *testing.T, h *harness, marker string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if h.logContains(marker) {
			t.Fatalf("daemon log unexpectedly contained field %s while Responses stream was blocked", marker)
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertResponsesResponseMetadata(t *testing.T, requestID string, responseBody string, probeID string) {
	t.Helper()
	expectedRequestID := "upstream-" + probeID
	if requestID != expectedRequestID {
		t.Fatalf("response for probe %q has unexpected field X-Request-Id; got length = %d, want length = %d", probeID, len(requestID), len(expectedRequestID))
	}
	if strings.Contains(responseBody, `"usage"`) {
		t.Fatalf("response for probe %q unexpectedly contains field usage; response length = %d", probeID, len(responseBody))
	}
}

func assertResponsesPassthroughRequest(t *testing.T, request responsesUpstreamRequest, expectedBody string, probeID string, stream bool) {
	t.Helper()
	if request.path != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", request.path)
	}
	if request.body != expectedBody {
		t.Fatalf("upstream request body changed for probe %q; got length = %d, want length = %d", probeID, len(request.body), len(expectedBody))
	}
	for _, field := range []struct {
		name  string
		exact string
	}{
		{name: "input", exact: fmt.Sprintf(`"input":"probe %s"`, probeID)},
		{name: "stream", exact: fmt.Sprintf(`"stream":%t`, stream)},
		{name: "temperature", exact: `"temperature":0.25`},
		{name: "parallel_tool_calls", exact: `"parallel_tool_calls":true`},
		{name: "compat_unknown", exact: fmt.Sprintf(`"compat_unknown":{"probe":"%s","mode":"preserve"}`, probeID)},
	} {
		if !strings.Contains(request.body, field.exact) {
			t.Fatalf("upstream request for probe %q missing preserved field %s; body length = %d", probeID, field.name, len(request.body))
		}
	}
}

func assertResponsesCaptures(t *testing.T, captures []responsesCapture, probeID string, stream bool) {
	t.Helper()
	if err := validateResponsesCaptureBoundaries(captures); err != nil {
		t.Fatalf("capture boundaries for probe %q: %v; capture count = %d", probeID, err, len(captures))
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
			t.Fatalf("%s request capture missing probe %q; body length = %d", capture.client, probeID, len(capture.requestBody))
		}
		if !strings.Contains(capture.responseBody, probeID) {
			t.Fatalf("%s response capture missing probe %q; body length = %d", capture.client, probeID, len(capture.responseBody))
		}
		if !strings.Contains(capture.responseHeaders, "X-Upstream-Marker") {
			t.Fatalf("%s response capture missing field X-Upstream-Marker; header length = %d", capture.client, len(capture.responseHeaders))
		}
		if !strings.Contains(capture.responseHeaders, "X-Request-Id") {
			t.Fatalf("%s response capture missing field X-Request-Id; header length = %d", capture.client, len(capture.responseHeaders))
		}
		expectedRequestID := "upstream-" + probeID
		if !strings.Contains(capture.responseHeaders, expectedRequestID) {
			t.Fatalf("%s response capture has unexpected field X-Request-Id for probe %q; header length = %d", capture.client, probeID, len(capture.responseHeaders))
		}
		if strings.Contains(capture.responseHeaders, "X-Clyde-Warning") {
			t.Fatalf("%s response capture unexpectedly recorded field X-Clyde-Warning; header length = %d", capture.client, len(capture.responseHeaders))
		}
		if strings.Contains(capture.responseBody, `"usage"`) {
			t.Fatalf("%s response capture unexpectedly contains field usage for probe %q; body length = %d", capture.client, probeID, len(capture.responseBody))
		}
		if stream && !strings.Contains(capture.responseBody, "response.completed") {
			t.Fatalf("%s stream capture missing field response.completed for probe %q; body length = %d", capture.client, probeID, len(capture.responseBody))
		}
	}
}
