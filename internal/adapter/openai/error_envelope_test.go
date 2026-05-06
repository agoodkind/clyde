package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"goodkind.io/clyde/internal/adapter/errcontract"
)

func TestErrorRendererIncludesClydeDiagnostics(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	info := errcontract.ErrorInfo{
		Type:    "invalid_request_error",
		Code:    "upstream_failed",
		Message: "upstream failed",
		Diagnostics: &errcontract.ErrorDiagnostics{
			RequestID:          "chatcmpl-test",
			TraceID:            "11111111111111111111111111111111",
			ChatKey:            "root.b02",
			ChatRootKey:        "root",
			ChatBranchKey:      "b02",
			UpstreamRequestID:  "upstream-req",
			UpstreamResponseID: "resp-123",
			Provider:           "codex",
			ModelAlias:         "clyde-codex-5.5-high",
			HeaderNames:        []string{"content-type", "traceparent"},
			Headers: map[string]string{
				"authorization": "[redacted]",
				"content-type":  "application/json",
			},
			LogHint: "adapter/http/errors.jsonl request_id=chatcmpl-test",
		},
	}

	if err := NewErrorRenderer().Render(rec, http.StatusBadRequest, info); err != nil {
		t.Fatalf("Render: %v", err)
	}
	var out ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Error.Message != "upstream failed" || out.Error.Type != "invalid_request_error" || out.Error.Code != "upstream_failed" {
		t.Fatalf("canonical fields changed: %+v", out.Error)
	}
	if out.Error.Clyde == nil {
		t.Fatalf("missing Clyde diagnostics: %+v", out.Error)
	}
	if out.Error.Clyde.RequestID != "chatcmpl-test" {
		t.Fatalf("request_id=%q", out.Error.Clyde.RequestID)
	}
	if out.Error.Clyde.ChatKey != "root.b02" || out.Error.Clyde.ChatRootKey != "root" || out.Error.Clyde.ChatBranchKey != "b02" {
		t.Fatalf("chat diagnostics=%+v", out.Error.Clyde)
	}
	if out.Error.Clyde.UpstreamRequestID != "upstream-req" || out.Error.Clyde.UpstreamResponseID != "resp-123" {
		t.Fatalf("upstream diagnostics=%+v", out.Error.Clyde)
	}
	if out.Error.Clyde.Headers["authorization"] != "[redacted]" {
		t.Fatalf("authorization header was not redacted: %+v", out.Error.Clyde.Headers)
	}
}

func TestErrorRendererOmitsEmptyClydeDiagnostics(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	info := errcontract.ErrorInfo{
		Type:    "invalid_request_error",
		Code:    "invalid_request",
		Message: "bad request",
	}

	if err := NewErrorRenderer().Render(rec, http.StatusBadRequest, info); err != nil {
		t.Fatalf("Render: %v", err)
	}
	var out ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Error.Clyde != nil {
		t.Fatalf("unexpected Clyde diagnostics: %+v", out.Error.Clyde)
	}
}
