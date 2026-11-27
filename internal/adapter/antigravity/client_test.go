package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientStreamSendsCodeAssistRequest(t *testing.T) {
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", request.Method)
			}

			if request.URL.Path != streamGenerateContentPath ||
				request.URL.RawQuery != streamGenerateContentQuery {
				t.Errorf("target = %s?%s", request.URL.Path, request.URL.RawQuery)
			}

			if request.Header.Get("Authorization") != "Bearer access-token" {
				t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
			}

			if request.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q", request.Header.Get("Content-Type"))
			}

			if request.Header.Get("User-Agent") != ClientUserAgent("2.35.2") {
				t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
			}

			if request.Header.Get("Accept") != "" {
				t.Errorf("unexpected Accept header = %q", request.Header.Get("Accept"))
			}

			var envelope CodeAssistEnvelope
			if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
				t.Errorf("decode envelope: %v", err)
			}

			if envelope.Model != "gemini-3-pro" || envelope.Request.SessionID != "session-1" {
				t.Errorf("envelope = %+v", envelope)
			}

			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"response\":{}}\n\n"))
		}),
	)
	defer server.Close()
	client, err := NewClient(ClientOptions{
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	response, err := client.Stream(
		context.Background(),
		"access-token",
		CodeAssistEnvelope{
			Model:   "gemini-3-pro",
			Request: CodeAssistRequest{SessionID: "session-1"},
		},
		ClientUserAgent("2.35.2"),
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestNewClientRequiresSecureRemoteBaseURL(t *testing.T) {
	if _, err := NewClient(ClientOptions{BaseURL: "http://api.example.com"}); err == nil {
		t.Fatal("NewClient accepted an insecure remote base URL")
	}

	if _, err := NewClient(ClientOptions{BaseURL: ""}); err == nil {
		t.Fatal("NewClient accepted an empty base URL")
	}
}

func TestClientStreamReturnsRedirectWithoutFollowingIt(t *testing.T) {
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != streamGenerateContentPath {
				t.Errorf("client followed redirect to %s", request.URL.Path)
				writer.WriteHeader(http.StatusOK)

				return
			}

			writer.Header().Set("Location", "/redirected")
			writer.WriteHeader(http.StatusTemporaryRedirect)
		}),
	)
	defer server.Close()

	client, err := NewClient(ClientOptions{
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Stream(t.Context(), "fixture-token", CodeAssistEnvelope{}, "fixture-client")
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusTemporaryRedirect {
		t.Fatalf("Stream error = %v, want upstream redirect", err)
	}
}
