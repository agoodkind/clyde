package adapter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

// postNativeCodexResponses sets native Codex turn metadata. The adapter
// classifies the request for raw forwarding.
func postNativeCodexResponses(t *testing.T, url string, body string) conformanceResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new native request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read native response: %v", err)
	}
	return conformanceResponse{status: response.StatusCode, header: response.Header, body: responseBody}
}

func TestOpenAIListenerKeepsNativeCodexForwardingContract(t *testing.T) {
	requestBody := `{"model":"gpt-native","input":"native","vendor_extension":{"kept":true}}`
	upstreamError := `{"error":{"message":"native upstream failure","type":"server_error"}}`
	var forwardedBody []byte
	upstream := newLoopbackHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		forwardedBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(upstreamError))
	}))
	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	openAIURL, _ := startRoutingListeners(t, srv)

	forwarded := postNativeCodexResponses(t, openAIURL+"/v1/responses", requestBody)
	if forwarded.status != http.StatusInternalServerError || string(forwarded.body) != upstreamError {
		t.Fatalf("native forwarding = %d %s, want the upstream status and body unchanged", forwarded.status, forwarded.body)
	}
	if string(forwardedBody) != requestBody {
		t.Fatalf("native upstream body = %s, want unknown fields forwarded unchanged", forwardedBody)
	}

	// A resolver failure after native classification returns the
	// compatibility status 400 instead of the documented 404.
	unknownModel := postNativeCodexResponses(t, openAIURL+"/v1/responses", `{"model":"unrouted-native","input":"native"}`)
	unknownModelError := decodeErrorEnvelope(t, unknownModel.body)
	if unknownModel.status != http.StatusBadRequest || unknownModelError.Type != "invalid_request_error" {
		t.Fatalf("native resolver failure = %d %q, want 400 invalid_request_error; body=%s", unknownModel.status, unknownModelError.Type, unknownModel.body)
	}
}

func TestOpenAIListenerKeepsNativeCodexTransportFailureContract(t *testing.T) {
	closed := newLoopbackHTTPServer(t, http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	srv := newNativeResponsesServer(t, closedURL, &nativeRawRefreshAuth{})
	openAIURL, _ := startRoutingListeners(t, srv)

	failed := postNativeCodexResponses(t, openAIURL+"/v1/responses", `{"model":"gpt-native","input":"native"}`)
	failure := decodeErrorEnvelope(t, failed.body)
	if failed.status != http.StatusBadRequest || failure.Type != "invalid_request_error" {
		t.Fatalf("native transport failure = %d %q, want 400 invalid_request_error; body=%s", failed.status, failure.Type, failed.body)
	}
}
