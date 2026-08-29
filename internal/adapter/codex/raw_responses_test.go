package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/config"
)

type rawResponsesAuth struct {
	refreshes atomic.Int32
}

func rawResponsesConfig(baseURL string) config.AdapterConfig {
	return config.AdapterConfig{Codex: config.AdapterCodex{BaseURL: baseURL}}
}

func (a *rawResponsesAuth) Token(context.Context) (string, error) {
	return "configured-token", nil
}

func (a *rawResponsesAuth) ForceRefresh(context.Context) (string, error) {
	a.refreshes.Add(1)
	return "refreshed-token", nil
}

func TestProviderOpenRawResponsesPreservesBytesAndStripsInboundCredentials(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"keep every byte"}]}],"tools":[{"type":"custom","name":"apply_patch"}],"metadata":{"opaque":true}}`)
	responseBody := []byte(`{"id":"resp-native","status":"completed","opaque":{"keep":true}}`)
	var gotBody []byte
	var gotHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotBody, _ = io.ReadAll(request.Body)
		gotHeader = request.Header.Clone()
		writer.Header().Set("X-Upstream-Marker", "kept")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	provider := NewProvider(adapterprovider.Deps{Config: rawResponsesConfig(upstream.URL), Auth: &rawResponsesAuth{}, HTTPClient: upstream.Client()}, ProviderOptions{})
	response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{
		Body: requestBody,
		Header: http.Header{
			"Authorization":         {"Bearer inbound-secret"},
			"Proxy-Authorization":   {"Basic inbound-proxy-secret"},
			"Connection":            {"X-Remove"},
			"X-Remove":              {"hop-by-hop"},
			"Chatgpt-Account-Id":    {"native-account"},
			"X-Codex-Turn-Metadata": {`{"session_id":"native-session","thread_source":"user","turn_id":"","sandbox":"none"}`},
			"X-Preserve":            {"opaque"},
		},
	})
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	gotResponse, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if response.StatusCode != http.StatusCreated || !bytes.Equal(gotResponse, responseBody) || response.Header.Get("X-Upstream-Marker") != "kept" {
		t.Fatalf("response status=%d header=%v body=%s", response.StatusCode, response.Header, gotResponse)
	}
	if !bytes.Equal(gotBody, requestBody) {
		t.Fatalf("upstream body changed:\n got: %s\nwant: %s", gotBody, requestBody)
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer configured-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if gotHeader.Get("Proxy-Authorization") != "" || gotHeader.Get("X-Remove") != "" || gotHeader.Get("Connection") != "" {
		t.Fatalf("credential or hop header leaked: %v", gotHeader)
	}
	if got := gotHeader.Get("Chatgpt-Account-Id"); got != "native-account" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := gotHeader.Get("X-Preserve"); got != "opaque" {
		t.Fatalf("X-Preserve = %q", got)
	}
}

func TestProviderOpenRawResponsesRefreshesOnceAfterUnauthorized(t *testing.T) {
	auth := &rawResponsesAuth{}
	var requests atomic.Int32
	var lastAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lastAuthorization = request.Header.Get("Authorization")
		if requests.Add(1) == 1 {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"error":"still forbidden"}`))
	}))
	t.Cleanup(upstream.Close)

	provider := NewProvider(adapterprovider.Deps{Config: rawResponsesConfig(upstream.URL), Auth: auth, HTTPClient: upstream.Client()}, ProviderOptions{})
	response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{Body: []byte(`{"model":"gpt-native"}`), Header: http.Header{}})
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusForbidden || requests.Load() != 2 || auth.refreshes.Load() != 1 || lastAuthorization != "Bearer refreshed-token" {
		t.Fatalf("status=%d requests=%d refreshes=%d authorization=%q", response.StatusCode, requests.Load(), auth.refreshes.Load(), lastAuthorization)
	}
}
