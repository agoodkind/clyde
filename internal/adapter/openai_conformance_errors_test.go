package adapter

import (
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIConformanceUpstreamFailureStatusByListener(t *testing.T) {
	tests := []struct {
		name           string
		upstreamStatus int
		wantOpenAI     int
		wantOpenAIType string
	}{
		{name: "rate limit", upstreamStatus: http.StatusTooManyRequests, wantOpenAI: http.StatusTooManyRequests, wantOpenAIType: "rate_limit_error"},
		{name: "server error", upstreamStatus: http.StatusInternalServerError, wantOpenAI: http.StatusInternalServerError, wantOpenAIType: "server_error"},
		{name: "overloaded", upstreamStatus: http.StatusServiceUnavailable, wantOpenAI: http.StatusServiceUnavailable, wantOpenAIType: "service_unavailable_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newFailingConformanceUpstream(test.upstreamStatus, `{"error":{"message":"upstream said no"}}`)
			listeners := startConformanceServer(t, upstream)
			body := `{"model":"gpt-future","messages":[{"role":"user","content":"hi"}]}`

			openAI := postConformance(t, listeners.openAI+"/v1/chat/completions", body)
			drainConformanceRequest(t, upstream)
			openAIError := decodeErrorEnvelope(t, openAI.body)
			if openAI.status != test.wantOpenAI || openAIError.Type != test.wantOpenAIType {
				t.Fatalf("OpenAI listener = %d %q, want %d %q; body=%s", openAI.status, openAIError.Type, test.wantOpenAI, test.wantOpenAIType, openAI.body)
			}
			if !strings.Contains(openAIError.Message, "upstream said no") {
				t.Fatalf("OpenAI listener message = %q, want upstream diagnostic", openAIError.Message)
			}

			cursor := postConformance(t, listeners.cursor+"/v1/chat/completions", body)
			drainConformanceRequest(t, upstream)
			cursorError := decodeErrorEnvelope(t, cursor.body)
			if cursor.status != http.StatusBadRequest || cursorError.Type != "invalid_request_error" {
				t.Fatalf("Cursor listener = %d %q, want 400 invalid_request_error; body=%s", cursor.status, cursorError.Type, cursor.body)
			}
			if !strings.Contains(cursorError.Message, "upstream said no") {
				t.Fatalf("Cursor listener message = %q, want upstream diagnostic", cursorError.Message)
			}
		})
	}
}
