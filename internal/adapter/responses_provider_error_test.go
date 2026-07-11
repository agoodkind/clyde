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
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

func TestResponsesPreparedCodexErrorPreservesProviderClassification(t *testing.T) {
	t.Parallel()

	const contextMessage = "This model's maximum context length was exceeded. Please reduce the length of the messages."
	cases := []struct {
		name        string
		err         error
		wantClass   adapterErrorClass
		wantCode    string
		wantMessage string
	}{
		{
			name:        "context overflow",
			err:         &adaptercodex.ContextWindowError{Message: "context_length_exceeded"},
			wantClass:   adapterErrorContextLengthExceeded,
			wantCode:    "context_length_exceeded",
			wantMessage: contextMessage,
		},
		{
			name:        "unsupported model",
			err:         &adaptercodex.UnsupportedModelError{Message: "requested model is not supported"},
			wantClass:   adapterErrorModelNotSupported,
			wantCode:    "model_not_supported",
			wantMessage: "requested model is not supported",
		},
		{
			name:        "schema violation",
			err:         errors.New("[ObjectParam] [input[2]] [missing_required_parameter]"),
			wantClass:   adapterErrorUpstreamSchemaViolation,
			wantCode:    "upstream_malformed_request",
			wantMessage: "[ObjectParam] [input[2]] [missing_required_parameter]",
		},
		{
			name: "typed upstream body",
			err: &adaptercodex.UpstreamStatusError{
				Status:  http.StatusBadGateway,
				Snippet: "typed upstream response body",
			},
			wantClass:   adapterErrorUpstreamFailed,
			wantCode:    "upstream_failed",
			wantMessage: "codex http transport: upstream status 502: typed upstream response body",
		},
		{
			name:        "generic error",
			err:         errors.New("codex websocket read failed"),
			wantClass:   adapterErrorUpstreamFailed,
			wantCode:    "upstream_failed",
			wantMessage: "codex websocket read failed",
		},
	}
	resolved := adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderCodex,
		Model:    "gpt-5.4-wire",
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			aerr := responsesPreparedProviderError(
				adapterresolver.ProviderCodex,
				"clyde-codex-5.4",
				resolved,
				testCase.err,
			)
			if aerr.Class != testCase.wantClass || aerr.Code != testCase.wantCode {
				t.Fatalf("classification = %s/%s, want %s/%s", aerr.Class, aerr.Code, testCase.wantClass, testCase.wantCode)
			}
			if aerr.Message != testCase.wantMessage {
				t.Fatalf("message = %q, want %q", aerr.Message, testCase.wantMessage)
			}
			if aerr.Backend != "codex" || aerr.ModelAlias != "clyde-codex-5.4" || aerr.ResolvedModelName != "gpt-5.4-wire" {
				t.Fatalf("request context = backend %q alias %q resolved %q", aerr.Backend, aerr.ModelAlias, aerr.ResolvedModelName)
			}
			if !errors.Is(aerr.Cause, testCase.err) {
				t.Fatalf("cause = %v, want original error %v", aerr.Cause, testCase.err)
			}
		})
	}
}

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
			srv.dispatchResolvedResponsesWithID(
				response,
				request,
				resolved.OpenAI,
				"req-provider-error",
				responsesResponseID("req-provider-error"),
				nil,
				resolved,
				adaptercompat.WarningSet{},
			)
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

func TestResponsesNonStreamingProviderErrorCarriesCanonicalWarnings(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		Prepare: func(_ context.Context, req adapterresolver.ResolvedRequest, requestID string) (anthropic.PreparedRequest, error) {
			resolved := req
			return anthropic.PreparedRequest{RequestID: requestID, Resolved: &resolved}, nil
		},
		ExecutePrepared: func(_ context.Context, _ anthropic.PreparedRequest, _ adapterprovider.EventWriter) (adapterprovider.Result, error) {
			return adapterprovider.Result{}, errors.New("provider-visible failure")
		},
	})
	openAIURL, _ := startRoutingListeners(t, srv)
	response, body := postResponsesRaw(t, openAIURL+"/v1/responses", `{"model":"claude-future","input":"hello","background":true}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", response.StatusCode, body)
	}
	if len(response.Header.Values("X-Clyde-Warning")) == 0 {
		t.Fatalf("missing warning header: %v", response.Header)
	}
	var envelope struct {
		Error struct {
			Clyde *struct {
				Warnings []adaptercompat.CompatibilityWarning `json:"warnings"`
			} `json:"clyde"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal error: %v; body=%s", err, body)
	}
	if envelope.Error.Clyde == nil || len(envelope.Error.Clyde.Warnings) != 1 {
		t.Fatalf("error warnings=%+v want one canonical warning", envelope.Error.Clyde)
	}
	if envelope.Error.Clyde.Warnings[0].Param != "background" {
		t.Fatalf("error warning=%+v", envelope.Error.Clyde.Warnings[0])
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
