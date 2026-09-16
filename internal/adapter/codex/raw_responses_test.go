package codex

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog/correlation"
)

type rawResponsesAuth struct {
	refreshes atomic.Int32
	accountID string
}

type rawResponsesRefreshFailureAuth struct {
	refreshes      atomic.Int32
	refreshedToken string
	refreshErr     error
}

type rawResponsesNoRefreshAuth struct{}

func (rawResponsesNoRefreshAuth) Token(context.Context) (string, error) {
	return "configured-token", nil
}

func (rawResponsesNoRefreshAuth) AccountID(context.Context) (string, error) {
	return "configured-account", nil
}

func (a *rawResponsesRefreshFailureAuth) Token(context.Context) (string, error) {
	return "configured-token", nil
}

func (a *rawResponsesRefreshFailureAuth) ForceRefresh(context.Context) (string, error) {
	a.refreshes.Add(1)
	return a.refreshedToken, a.refreshErr
}

func (a *rawResponsesRefreshFailureAuth) AccountID(context.Context) (string, error) {
	return "configured-account", nil
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

func (a *rawResponsesAuth) AccountID(context.Context) (string, error) {
	return a.accountID, nil
}

func TestAuthManagerAccountIDReadsConfiguredAuthFile(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"account_id":"configured-account"}}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	manager := NewAuthManager(authPath, AuthManagerOptions{})
	accountID, err := manager.AccountID(context.Background())
	if err != nil {
		t.Fatalf("AccountID() error = %v", err)
	}
	if accountID != "configured-account" {
		t.Fatalf("AccountID() = %q", accountID)
	}
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

	provider := NewProvider(adapterprovider.Deps{Config: rawResponsesConfig(upstream.URL), Auth: &rawResponsesAuth{accountID: "configured-account"}, HTTPClient: upstream.Client()}, ProviderOptions{})
	response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{
		Body: requestBody,
		Header: http.Header{
			"Authorization":         {"Bearer inbound-secret"},
			"Proxy-Authorization":   {"Basic inbound-proxy-secret"},
			"Openai-Api-Key":        {"inbound-openai-secret"},
			"X-Clyde-Token":         {"inbound-clyde-secret"},
			"X-Amz-Security-Token":  {"inbound-aws-secret"},
			"Connection":            {"X-Remove"},
			"X-Remove":              {"hop-by-hop"},
			"Chatgpt-Account-Id":    {"untrusted-account"},
			"X-Codex-Turn-Metadata": {`{"session_id":"native-session","thread_source":"user","turn_id":"","sandbox":"none"}`},
			"X-Preserve":            {"opaque"},
		},
		RequestID: "raw-request-id",
		Correlation: correlation.Context{
			TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef",
			ParentSpanID: "fedcba9876543210", RequestID: "stale-request-id", IdentityAttributes: nil,
		},
		Stream: false,
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
	if gotHeader.Get("Proxy-Authorization") != "" || gotHeader.Get("Openai-Api-Key") != "" ||
		gotHeader.Get("X-Clyde-Token") != "" || gotHeader.Get("X-Amz-Security-Token") != "" ||
		gotHeader.Get("X-Remove") != "" || gotHeader.Get("Connection") != "" {
		t.Fatalf("credential or hop header leaked: %v", gotHeader)
	}
	if got := gotHeader.Get("Chatgpt-Account-Id"); got != "configured-account" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := gotHeader.Get("X-Preserve"); got != "opaque" {
		t.Fatalf("X-Preserve = %q", got)
	}
	if got := gotHeader.Get(clydeingress.HeaderRequestID); got != "raw-request-id" {
		t.Fatalf("request id = %q", got)
	}
	if got := gotHeader.Get(clydeingress.HeaderTraceID); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace id = %q", got)
	}
}

func TestProviderOpenRawResponsesPreservesGzipWithoutInboundEncoding(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(`{"id":"compressed-response"}`)); err != nil {
		t.Fatalf("write gzip response: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip response: %v", err)
	}
	responseBody := compressed.Bytes()
	var gotEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotEncoding = request.Header.Get("Accept-Encoding")
		writer.Header().Set("Content-Encoding", "gzip")
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	provider := NewProvider(adapterprovider.Deps{
		Config: rawResponsesConfig(upstream.URL), Auth: rawResponsesNoRefreshAuth{}, HTTPClient: upstream.Client(),
	}, ProviderOptions{})
	response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{
		Body: []byte(`{"model":"gpt-native"}`), Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
	}
	if response.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(body, responseBody) {
		t.Fatalf("gzip response changed: header=%q body=%x", response.Header.Get("Content-Encoding"), body)
	}
}

func TestProviderOpenRawResponsesForcesIdentityForV1Compaction(t *testing.T) {
	var gotEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotEncoding = request.Header.Get("Accept-Encoding")
		_, _ = writer.Write([]byte(`{"id":"compaction-response"}`))
	}))
	t.Cleanup(upstream.Close)

	provider := NewProvider(adapterprovider.Deps{
		Config: rawResponsesConfig(upstream.URL), Auth: rawResponsesNoRefreshAuth{}, HTTPClient: upstream.Client(),
	}, ProviderOptions{})
	response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{
		Body: []byte(`{"model":"gpt-native"}`),
		Header: http.Header{
			"Accept-Encoding":       {"gzip, br"},
			CodexTurnMetadataHeader: {`{"session_id":"native-session","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses"}}`},
		},
	})
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
	}
}

func TestProviderOpenRawResponsesForcesIdentityForV2Compaction(t *testing.T) {
	var gotEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotEncoding = request.Header.Get("Accept-Encoding")
		_, _ = writer.Write([]byte(`{"id":"compaction-response"}`))
	}))
	t.Cleanup(upstream.Close)

	provider := NewProvider(adapterprovider.Deps{
		Config: rawResponsesConfig(upstream.URL), Auth: rawResponsesNoRefreshAuth{}, HTTPClient: upstream.Client(),
	}, ProviderOptions{})
	response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{
		Body: []byte(`{"model":"gpt-native"}`),
		Header: http.Header{
			"Accept-Encoding":       {"gzip, br"},
			CodexTurnMetadataHeader: {`{"session_id":"native-session","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`},
		},
	})
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if gotEncoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", gotEncoding)
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

	auth.accountID = "configured-account"
	provider := NewProvider(adapterprovider.Deps{Config: rawResponsesConfig(upstream.URL), Auth: auth, HTTPClient: upstream.Client()}, ProviderOptions{})
	var raw RawResponsesRequest
	raw.Body = []byte(`{"model":"gpt-native"}`)
	raw.Header = http.Header{}
	response, err := provider.OpenRawResponses(context.Background(), raw)
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusForbidden || requests.Load() != 2 || auth.refreshes.Load() != 1 || lastAuthorization != "Bearer refreshed-token" {
		t.Fatalf("status=%d requests=%d refreshes=%d authorization=%q", response.StatusCode, requests.Load(), auth.refreshes.Load(), lastAuthorization)
	}
}

func TestProviderOpenRawResponsesPreservesRejectionWhenRefreshCannotContinue(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		refreshedToken string
		refreshErr     error
	}{
		{name: "refresh fails", status: http.StatusUnauthorized, refreshErr: errors.New("refresh failed")},
		{name: "refresh token is empty", status: http.StatusForbidden, refreshedToken: " \t "},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"error":"original rejection"}`)
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writer.Header().Set("X-Original-Rejection", "kept")
				writer.WriteHeader(testCase.status)
				_, _ = writer.Write(body)
			}))
			t.Cleanup(upstream.Close)

			auth := &rawResponsesRefreshFailureAuth{
				refreshedToken: testCase.refreshedToken,
				refreshErr:     testCase.refreshErr,
			}
			provider := NewProvider(adapterprovider.Deps{
				Config: rawResponsesConfig(upstream.URL), Auth: auth, HTTPClient: upstream.Client(),
			}, ProviderOptions{})
			response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{
				Body: []byte(`{"model":"gpt-native"}`), Header: http.Header{},
			})
			if err != nil {
				t.Fatalf("OpenRawResponses() error = %v", err)
			}
			t.Cleanup(func() { _ = response.Body.Close() })
			gotBody, readErr := io.ReadAll(response.Body)
			if readErr != nil {
				t.Fatalf("read original rejection: %v", readErr)
			}
			if response.StatusCode != testCase.status ||
				response.Header.Get("X-Original-Rejection") != "kept" ||
				!bytes.Equal(gotBody, body) {
				t.Fatalf("status=%d header=%v body=%s", response.StatusCode, response.Header, gotBody)
			}
			if requests.Load() != 1 || auth.refreshes.Load() != 1 {
				t.Fatalf("requests=%d refreshes=%d", requests.Load(), auth.refreshes.Load())
			}
		})
	}
}

func TestProviderOpenRawResponsesPreservesRejectionWithoutRefresher(t *testing.T) {
	body := []byte(`{"error":"original rejection"}`)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write(body)
	}))
	t.Cleanup(upstream.Close)

	provider := NewProvider(adapterprovider.Deps{
		Config: rawResponsesConfig(upstream.URL), Auth: rawResponsesNoRefreshAuth{}, HTTPClient: upstream.Client(),
	}, ProviderOptions{})
	response, err := provider.OpenRawResponses(context.Background(), RawResponsesRequest{
		Body: []byte(`{"model":"gpt-native"}`), Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	gotBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatalf("read original rejection: %v", readErr)
	}
	if response.StatusCode != http.StatusUnauthorized ||
		!bytes.Equal(gotBody, body) || requests.Load() != 1 {
		t.Fatalf("status=%d requests=%d body=%s", response.StatusCode, requests.Load(), gotBody)
	}
}

func TestProviderOpenRawResponsesPreservesRedirectResponse(t *testing.T) {
	redirected := atomic.Bool{}
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirected.Store(true)
		if request.Header.Get("Authorization") != "" || request.Header.Get("Chatgpt-Account-Id") != "" {
			t.Errorf("sensitive headers followed redirect: %v", request.Header)
		}
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(redirectTarget.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", redirectTarget.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = writer.Write([]byte(`{"redirect":"preserve"}`))
	}))
	t.Cleanup(upstream.Close)

	provider := NewProvider(adapterprovider.Deps{Config: rawResponsesConfig(upstream.URL), Auth: &rawResponsesAuth{accountID: "configured-account"}, HTTPClient: upstream.Client()}, ProviderOptions{})
	var raw RawResponsesRequest
	raw.Body = []byte(`{"model":"gpt-native"}`)
	raw.Header = http.Header{}
	response, err := provider.OpenRawResponses(context.Background(), raw)
	if err != nil {
		t.Fatalf("OpenRawResponses() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read redirect response: %v", err)
	}
	if redirected.Load() || response.StatusCode != http.StatusTemporaryRedirect || !bytes.Equal(body, []byte(`{"redirect":"preserve"}`)) {
		t.Fatalf("redirected=%t status=%d body=%s", redirected.Load(), response.StatusCode, body)
	}
}
