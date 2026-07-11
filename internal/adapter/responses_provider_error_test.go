package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

func TestResponsesProviderExecutionPreservesClientErrorAndLogsContext(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			fakes := newRoutingFakeEndpoints(t)
			srv := newRoutingIntegrationServer(t, fakes)
			var logs bytes.Buffer
			srv.log = slog.New(slog.NewJSONHandler(&logs, nil))
			const secretValue = "secret-response-provider-token"
			const message = "provider-visible-sentinel"
			sentinel := errors.New(message)
			srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
				Prepare: func(_ context.Context, req adapterresolver.ResolvedRequest, requestID string) (anthropic.PreparedRequest, error) {
					resolved := req
					return anthropic.PreparedRequest{RequestID: requestID, Resolved: &resolved}, nil
				},
				ExecutePrepared: func(_ context.Context, _ anthropic.PreparedRequest, _ adapterprovider.EventWriter) (adapterprovider.Result, error) {
					return adapterprovider.Result{}, sentinel
				},
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resolved := adapterresolver.ResolvedRequest{
				Provider: adapterresolver.ProviderAnthropic,
				Model:    "claude-future",
				OpenAI:   adapteropenai.ChatRequest{Model: "claude-future", Stream: stream},
			}
			srv.dispatchResolvedResponses(response, request, resolved.OpenAI, "req-provider-error", nil, resolved, adaptercompat.WarningSet{})
			body := response.Body.Bytes()

			if stream {
				if response.Code != http.StatusOK {
					t.Fatalf("stream status = %d, want 200; body=%s", response.Code, body)
				}
				if got := responsesFailedMessage(t, body); got != message {
					t.Fatalf("stream error message = %q, want %q", got, message)
				}
			} else {
				if response.Code != http.StatusBadRequest {
					t.Fatalf("collect status = %d, want 400; body=%s", response.Code, body)
				}
				var envelope adapteropenai.ErrorResponse
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatalf("unmarshal collect error: %v; body=%s", err, body)
				}
				if envelope.Error.Clyde == nil {
					t.Fatalf("collect error omitted Clyde diagnostics: %s", body)
				}
				expected := message + " (Clyde request_id=" + envelope.Error.Clyde.RequestID + ")"
				if envelope.Error.Message != expected {
					t.Fatalf("collect error message = %q, want %q", envelope.Error.Message, expected)
				}
			}
			logOutput := logs.String()
			if !strings.Contains(logOutput, `"msg":"adapter.responses.provider_failed"`) || !strings.Contains(logOutput, `"stage":"execute"`) {
				t.Fatalf("missing contextual provider error log: %s", logOutput)
			}
			if strings.Contains(logOutput, secretValue) {
				t.Fatalf("provider error log leaked secret value: %s", logOutput)
			}
		})
	}
}

func responsesFailedMessage(t *testing.T, body []byte) string {
	t.Helper()
	for _, frame := range strings.Split(string(body), "\n\n") {
		if !strings.Contains(frame, "event: response.failed") {
			continue
		}
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var envelope struct {
				Response struct {
					Error *adapteropenai.ResponsesError `json:"error"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope); err != nil {
				t.Fatalf("unmarshal response.failed frame: %v", err)
			}
			if envelope.Response.Error == nil {
				t.Fatalf("response.failed omitted error: %s", frame)
			}
			return envelope.Response.Error.Message
		}
	}
	t.Fatalf("missing response.failed frame: %s", body)
	return ""
}
