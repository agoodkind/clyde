package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
)

// errorBoundaryRouteFamily is the family-tag used by table-driven
// rows in the contract test so each row's assertion can pick the
// correct envelope shape to decode.
type errorBoundaryRouteFamily string

const (
	errorBoundaryFamilyOpenAI    errorBoundaryRouteFamily = "openai"
	errorBoundaryFamilyAnthropic errorBoundaryRouteFamily = "anthropic"
)

// errorBoundaryAssertion captures the shape every contract row checks
// after writeShapedError or mapUpstreamForFamily runs. The neutral
// adapterError carries only Class/Code/Param now, so rows assert the
// neutral Code on the struct and assert the provider wire type by
// decoding the rendered envelope end-to-end.
type errorBoundaryAssertion struct {
	wantStatus      int
	wantNeutralCode string
	wantWireType    string
	wantMsgContains string
	family          errorBoundaryRouteFamily
}

// fakeUpstreamError exposes a minimal anthropic.UpstreamError-shaped
// fixture so the contract test can drive anthropicProviderAdapterError
// without booting a live upstream.
type fakeUpstreamError struct {
	status  int
	message string
	class   anthropic.ResponseClass
}

func (e *fakeUpstreamError) build() *anthropic.UpstreamError {
	return &anthropic.UpstreamError{
		Status:  e.status,
		Message: e.message,
		Classification: anthropic.Classification{
			Class: e.class,
		},
	}
}

// renderedOpenAIEnvelope renders an adapterError through the full
// boundary on the OpenAI route family and decodes the resulting
// envelope so contract rows can assert the provider wire type that
// actually reaches the client, not a cached struct field.
func renderedOpenAIEnvelope(t *testing.T, aerr *adapterError) adapteropenai.ErrorResponse {
	t.Helper()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	resp := httptest.NewRecorder()
	srv.writeShapedError(resp, req, aerr)
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal openai envelope: %v body=%s", err, resp.Body.String())
	}
	return out
}

func TestMapUpstreamForFamilyCatchAllDefault(t *testing.T) {
	t.Parallel()
	aerr := mapUpstreamForFamily(adapterRouteOpenAI, "codex", 0, upstreamClassUnknown, "", "")
	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("catch-all status=%d", aerr.HTTPStatus)
	}
	if aerr.Code != "upstream_failed" {
		t.Fatalf("catch-all code=%q", aerr.Code)
	}
	if !strings.Contains(aerr.Message, "provider=codex") {
		t.Fatalf("catch-all message missing provider field: %q", aerr.Message)
	}
	env := renderedOpenAIEnvelope(t, aerr)
	if env.Error.Type != "invalid_request_error" {
		t.Fatalf("catch-all rendered type=%q", env.Error.Type)
	}
	if env.Error.Code != "upstream_failed" {
		t.Fatalf("catch-all rendered code=%q", env.Error.Code)
	}
}

func TestApplyFamilyShapeFlipsServerError(t *testing.T) {
	t.Parallel()
	aerr := newAdapterError(adapterErrorUpstreamFailed, "boom upstream message")
	aerr.HTTPStatus = http.StatusBadGateway
	got := applyFamilyShape(adapterRouteOpenAI, aerr)
	if got.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("expected status flipped to 400, got %d", got.HTTPStatus)
	}
	if !strings.Contains(got.Message, "boom upstream message") {
		t.Fatalf("upstream message lost: %q", got.Message)
	}
	env := renderedOpenAIEnvelope(t, got)
	if env.Error.Type != "invalid_request_error" {
		t.Fatalf("expected rendered type invalid_request_error, got %q", env.Error.Type)
	}
}

func TestApplyFamilyShapePreservesAnthropicNative(t *testing.T) {
	t.Parallel()
	aerr := newAdapterError(adapterErrorRateLimited, "anthropic rate limit reset in 30s")
	got := applyFamilyShape(adapterRouteAnthropic, aerr)
	if got.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("anthropic native path must preserve 429, got %d", got.HTTPStatus)
	}
	srv, _ := newLoggingServer(t, config.LoggingConfig{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp := httptest.NewRecorder()
	srv.writeShapedError(resp, req, got)
	var env anthropic.ErrorEnvelope
	if err := json.Unmarshal(resp.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal anthropic envelope: %v body=%s", err, resp.Body.String())
	}
	if env.Error.Type != "rate_limit_error" {
		t.Fatalf("anthropic rendered envelope type=%q", env.Error.Type)
	}
}

func TestErrorBoundaryAnthropicProviderAdapterError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		upstream  *fakeUpstreamError
		assertion errorBoundaryAssertion
	}{
		{
			name: "rate_limit_429",
			upstream: &fakeUpstreamError{
				status:  http.StatusTooManyRequests,
				message: "rate-limit-canary-DEADBEEF",
				class:   anthropic.ResponseClassRetryableError,
			},
			assertion: errorBoundaryAssertion{
				wantStatus:      http.StatusBadRequest,
				wantWireType:    "invalid_request_error",
				wantNeutralCode: "upstream_rate_limited",
				wantMsgContains: "rate-limit-canary-DEADBEEF",
				family:          errorBoundaryFamilyOpenAI,
			},
		},
		{
			name: "server_error_500",
			upstream: &fakeUpstreamError{
				status:  http.StatusInternalServerError,
				message: "anthropic-500-canary-CAFEBABE",
				class:   anthropic.ResponseClassRetryableError,
			},
			assertion: errorBoundaryAssertion{
				wantStatus:      http.StatusBadRequest,
				wantWireType:    "invalid_request_error",
				wantNeutralCode: "upstream_failed",
				wantMsgContains: "anthropic-500-canary-CAFEBABE",
				family:          errorBoundaryFamilyOpenAI,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			aerr := anthropicProviderAdapterError(tc.upstream.build())
			if aerr.HTTPStatus != tc.assertion.wantStatus {
				t.Fatalf("status=%d want=%d", aerr.HTTPStatus, tc.assertion.wantStatus)
			}
			if aerr.Code != tc.assertion.wantNeutralCode {
				t.Fatalf("neutral code=%q want=%q", aerr.Code, tc.assertion.wantNeutralCode)
			}
			if !strings.Contains(aerr.Message, tc.assertion.wantMsgContains) {
				t.Fatalf("message=%q missing canary=%q", aerr.Message, tc.assertion.wantMsgContains)
			}
			env := renderedOpenAIEnvelope(t, aerr)
			if env.Error.Type != tc.assertion.wantWireType {
				t.Fatalf("rendered type=%q want=%q", env.Error.Type, tc.assertion.wantWireType)
			}
			if env.Error.Code != tc.assertion.wantNeutralCode {
				t.Fatalf("rendered code=%q want=%q", env.Error.Code, tc.assertion.wantNeutralCode)
			}
		})
	}
}

func TestErrorBoundaryCodexProviderAdapterError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		assertion errorBoundaryAssertion
	}{
		{
			name: "schema_violation_object_param",
			err:  errors.New("[ObjectParam] [input[5].summary] [missing_required_parameter]: schema-violation-canary-FEED"),
			assertion: errorBoundaryAssertion{
				wantStatus:      http.StatusBadRequest,
				wantWireType:    "invalid_request_error",
				wantNeutralCode: "upstream_malformed_request",
				wantMsgContains: "schema-violation-canary-FEED",
				family:          errorBoundaryFamilyOpenAI,
			},
		},
		{
			name: "generic_502",
			err:  errors.New("codex 502 bad-gateway-canary-BEAD"),
			assertion: errorBoundaryAssertion{
				wantStatus:      http.StatusBadRequest,
				wantWireType:    "invalid_request_error",
				wantNeutralCode: "upstream_failed",
				wantMsgContains: "bad-gateway-canary-BEAD",
				family:          errorBoundaryFamilyOpenAI,
			},
		},
		{
			name: "http_transport_upstream_503",
			err:  errors.New("codex http transport: upstream status 503: http-503-canary-DEED"),
			assertion: errorBoundaryAssertion{
				wantStatus:      http.StatusBadRequest,
				wantWireType:    "invalid_request_error",
				wantNeutralCode: "upstream_failed",
				wantMsgContains: "http-503-canary-DEED",
				family:          errorBoundaryFamilyOpenAI,
			},
		},
		{
			name: "http_transport_upstream_429",
			err:  errors.New("codex http transport: upstream status 429: http-429-canary-FACE"),
			assertion: errorBoundaryAssertion{
				wantStatus:      http.StatusBadRequest,
				wantWireType:    "invalid_request_error",
				wantNeutralCode: "upstream_failed",
				wantMsgContains: "http-429-canary-FACE",
				family:          errorBoundaryFamilyOpenAI,
			},
		},
		{
			name: "websocket_handshake_401",
			err:  errors.New("websocket handshake failed: status 401: handshake-401-canary-BABE"),
			assertion: errorBoundaryAssertion{
				wantStatus:      http.StatusBadRequest,
				wantWireType:    "invalid_request_error",
				wantNeutralCode: "upstream_failed",
				wantMsgContains: "handshake-401-canary-BABE",
				family:          errorBoundaryFamilyOpenAI,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			aerr := codexProviderAdapterError(tc.err)
			if aerr.HTTPStatus != tc.assertion.wantStatus {
				t.Fatalf("status=%d want=%d", aerr.HTTPStatus, tc.assertion.wantStatus)
			}
			if aerr.Code != tc.assertion.wantNeutralCode {
				t.Fatalf("neutral code=%q want=%q", aerr.Code, tc.assertion.wantNeutralCode)
			}
			if !strings.Contains(aerr.Message, tc.assertion.wantMsgContains) {
				t.Fatalf("message=%q missing canary=%q", aerr.Message, tc.assertion.wantMsgContains)
			}
			env := renderedOpenAIEnvelope(t, aerr)
			if env.Error.Type != tc.assertion.wantWireType {
				t.Fatalf("rendered type=%q want=%q", env.Error.Type, tc.assertion.wantWireType)
			}
			if env.Error.Code != tc.assertion.wantNeutralCode {
				t.Fatalf("rendered code=%q want=%q", env.Error.Code, tc.assertion.wantNeutralCode)
			}
		})
	}
}

func TestErrorBoundaryWriteShapedErrorOpenAIInvalidJSON(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	resp := httptest.NewRecorder()
	srv.writeShapedError(resp, req, adapterErrInvalidJSON("invalid JSON: contract-canary-AABBCC", nil))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.Code)
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "invalid_request_error" || out.Error.Code != "invalid_json" {
		t.Fatalf("envelope=%+v", out.Error)
	}
	if !strings.Contains(out.Error.Message, "contract-canary-AABBCC") {
		t.Fatalf("message=%q", out.Error.Message)
	}
}

func TestErrorBoundaryStreamErrorEmitsSingleFrameThenDone(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})
	rec := newFakeFlushingRecorder()
	sse, err := adapteropenai.NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	upstream := mapUpstreamForFamily(adapterRouteOpenAI, "codex", http.StatusBadGateway, upstreamClassServerError, "", "stream-canary-12345")
	if err := srv.respondAdapterStreamError(context.Background(), sse, upstream); err != nil {
		t.Fatalf("respondAdapterStreamError: %v", err)
	}
	body := rec.Body.String()
	frameCount := strings.Count(body, "data: {")
	if frameCount != 1 {
		t.Fatalf("expected exactly one data frame, got %d, body=%q", frameCount, body)
	}
	if !strings.Contains(body, "stream-canary-12345") {
		t.Fatalf("stream message missing canary: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing [DONE] terminator: %q", body)
	}
}

// fakeFlushingRecorder implements http.Flusher on top of
// httptest.ResponseRecorder so SSEWriter can commit headers.
type fakeFlushingRecorder struct {
	*httptest.ResponseRecorder
}

func newFakeFlushingRecorder() *fakeFlushingRecorder {
	return &fakeFlushingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (f *fakeFlushingRecorder) Flush() {}
