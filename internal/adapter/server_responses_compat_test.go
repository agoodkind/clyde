package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// postResponsesRaw posts one /v1/responses body and returns the full
// response so the test can inspect both headers and body.
func postResponsesRaw(t *testing.T, url string, body string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new responses request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", url, err)
	}
	return response, responseBody
}

// responsesClydeWarnings unmarshals the clyde.warnings extension from a
// Responses response object body, returning nil when the field is absent.
func responsesClydeWarnings(t *testing.T, body []byte) []adaptercompat.CompatibilityWarning {
	t.Helper()
	var object struct {
		Clyde *struct {
			Warnings []adaptercompat.CompatibilityWarning `json:"warnings"`
		} `json:"clyde"`
	}
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("unmarshal responses object: %v (body=%s)", err, body)
	}
	if object.Clyde == nil {
		return nil
	}
	return object.Clyde.Warnings
}

func firstStreamResponseObject(t *testing.T, body []byte) []byte {
	t.Helper()
	for _, frame := range strings.Split(string(body), "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var envelope struct {
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope); err != nil {
				t.Fatalf("unmarshal stream frame: %v", err)
			}
			if len(envelope.Response) > 0 {
				return envelope.Response
			}
		}
	}
	t.Fatalf("no stream frame carried a response object; body=%s", body)
	return nil
}

func warningForParam(warnings []adaptercompat.CompatibilityWarning, param string) (adaptercompat.CompatibilityWarning, bool) {
	for _, warning := range warnings {
		if warning.Param == param {
			return warning, true
		}
	}
	return adaptercompat.CompatibilityWarning{}, false
}

func TestResponsesNonStreamingCodexTemperatureWarns(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, _ := startRoutingListeners(t, srv)

	response, body := postResponsesRaw(t, openAIURL+"/v1/responses", `{"model":"gpt-future","input":"hello","temperature":0.5}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}

	headers := response.Header.Values("X-Clyde-Warning")
	if len(headers) == 0 {
		t.Fatalf("missing X-Clyde-Warning header; headers=%v", response.Header)
	}
	if !strings.Contains(strings.Join(headers, "|"), `"param":"temperature"`) {
		t.Fatalf("X-Clyde-Warning did not mention temperature: %v", headers)
	}

	warnings := responsesClydeWarnings(t, body)
	warning, ok := warningForParam(warnings, "temperature")
	if !ok {
		t.Fatalf("clyde.warnings missing temperature: %v", warnings)
	}
	if warning.Code != "field_omitted" || warning.Disposition != "omitted" {
		t.Fatalf("temperature warning = %+v", warning)
	}
}

func TestResponsesStreamingCodexTemperatureWarnsInFirstEvent(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, _ := startRoutingListeners(t, srv)

	response, body := postResponsesRaw(t, openAIURL+"/v1/responses", `{"model":"gpt-future","input":"hello","temperature":0.5,"stream":true}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if len(response.Header.Values("X-Clyde-Warning")) == 0 {
		t.Fatalf("missing X-Clyde-Warning header on stream; headers=%v", response.Header)
	}

	firstObject := firstStreamResponseObject(t, body)
	warnings := responsesClydeWarnings(t, firstObject)
	if _, ok := warningForParam(warnings, "temperature"); !ok {
		t.Fatalf("first stream event missing temperature warning: %s", firstObject)
	}
}

func TestResponsesStreamingCodexUnsupportedToolChoiceWarnsBeforeHeaders(t *testing.T) {
	for _, test := range []struct {
		name       string
		toolChoice string
	}{
		{name: "none", toolChoice: `"none"`},
		{name: "required", toolChoice: `"required"`},
		{name: "required function", toolChoice: `{"type":"function","name":"secret-tool-choice"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakes := newRoutingFakeEndpoints(t)
			srv := newRoutingIntegrationServer(t, fakes)
			openAIURL, _ := startRoutingListeners(t, srv)
			requestBody := `{"model":"gpt-future","input":"hello","stream":true,` +
				`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],` +
				`"tool_choice":` + test.toolChoice + `}`

			response, body := postResponsesRaw(t, openAIURL+"/v1/responses", requestBody)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d; body=%s", response.StatusCode, body)
			}
			codexRequest := <-fakes.codexReqs
			if codexRequest.ToolChoice != "auto" {
				t.Fatalf("Codex tool_choice=%q want auto", codexRequest.ToolChoice)
			}

			headers := strings.Join(response.Header.Values("X-Clyde-Warning"), "|")
			if !strings.Contains(headers, `"param":"tool_choice"`) || !strings.Contains(headers, `"disposition":"overridden"`) {
				t.Fatalf("tool_choice warning was not committed with headers: %v", response.Header)
			}
			if strings.Contains(headers, "secret-tool-choice") {
				t.Fatalf("tool_choice warning header leaked request data: %s", headers)
			}

			firstObject := firstStreamResponseObject(t, body)
			warnings := responsesClydeWarnings(t, firstObject)
			warning, ok := warningForParam(warnings, "tool_choice")
			if !ok {
				t.Fatalf("first stream event missing tool_choice warning: %s", firstObject)
			}
			if warning.Code != "field_overridden" || warning.Disposition != "overridden" || warning.Message != "tool_choice is not supported by the codex backend and was replaced with auto" {
				t.Fatalf("tool_choice warning = %+v", warning)
			}
		})
	}
}

func TestResponsesCodexAutoToolChoiceDoesNotWarn(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, _ := startRoutingListeners(t, srv)

	response, body := postResponsesRaw(t, openAIURL+"/v1/responses", `{"model":"gpt-future","input":"hello","tool_choice":"auto"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if headers := response.Header.Values("X-Clyde-Warning"); len(headers) != 0 {
		t.Fatalf("supported tool_choice emitted warning headers: %v", headers)
	}
	if warnings := responsesClydeWarnings(t, body); len(warnings) != 0 {
		t.Fatalf("supported tool_choice emitted response warnings: %v", warnings)
	}
}

func TestResponsesPreparationFailurePrecedesStreamingHeadersAndFrames(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	// Responses dispatch must use the concrete provider retained by Server,
	// not the generic registry entry. Removing this preparation dependency
	// leaves the registry intact and distinguishes the two boundaries.
	srv.codexProvider = nil
	openAIURL, _ := startRoutingListeners(t, srv)

	response, body := postResponsesRaw(t, openAIURL+"/v1/responses", `{"model":"gpt-future","input":"hello","stream":true}`)
	if response.StatusCode == http.StatusOK {
		t.Fatalf("status = %d, want typed pre-header error; body=%s", response.StatusCode, body)
	}
	if bytes.Contains(body, []byte("response.created")) || bytes.Contains(body, []byte("response.in_progress")) {
		t.Fatalf("preparation failure emitted lifecycle frames: %s", body)
	}
	var envelope adapteropenai.ErrorResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal error envelope: %v; body=%s", err, body)
	}
	if envelope.Error.Type != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", envelope.Error.Type)
	}
}

func TestResponsesUnsupportedToolOmittedAndWarned(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, _ := startRoutingListeners(t, srv)

	body := `{"model":"gpt-future","input":"hello","tools":[` +
		`{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},` +
		`{"type":"web_search"}` +
		`]}`
	response, respBody := postResponsesRaw(t, openAIURL+"/v1/responses", body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.StatusCode, respBody)
	}

	codexRequest := <-fakes.codexReqs
	if len(codexRequest.Tools) != 1 {
		t.Fatalf("Codex egress tools = %d, want only the function tool", len(codexRequest.Tools))
	}

	headers := response.Header.Values("X-Clyde-Warning")
	if !strings.Contains(strings.Join(headers, "|"), `"code":"tool_unsupported"`) {
		t.Fatalf("X-Clyde-Warning missing tool_unsupported: %v", headers)
	}

	warnings := responsesClydeWarnings(t, respBody)
	warning, ok := warningForParam(warnings, "tools")
	if !ok {
		t.Fatalf("clyde.warnings missing tools warning: %v", warnings)
	}
	if warning.Code != "tool_unsupported" || warning.Disposition != "omitted" {
		t.Fatalf("tool warning = %+v, want tool_unsupported/omitted", warning)
	}
}

func TestResponsesNoCompatFieldsStaysClydeFree(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, _ := startRoutingListeners(t, srv)

	response, body := postResponsesRaw(t, openAIURL+"/v1/responses", `{"model":"gpt-future","input":"hello"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if headers := response.Header.Values("X-Clyde-Warning"); len(headers) != 0 {
		t.Fatalf("unexpected X-Clyde-Warning header: %v", headers)
	}
	if bytes.Contains(body, []byte(`"clyde"`)) {
		t.Fatalf("clyde-free response carried clyde field: %s", body)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("unmarshal object: %v (body=%s)", err, body)
	}
	if _, present := keys["clyde"]; present {
		t.Fatalf("clyde key present in warning-free object: %s", body)
	}
}

func TestResponsesProjectionPreservesAnthropicOutputCapSpellings(t *testing.T) {
	request, err := adapteropenai.UnmarshalResponsesRequest([]byte(`{"model":"claude","input":"hello","max_output_tokens":300,"max_completion_tokens":200,"max_tokens":100}`))
	if err != nil {
		t.Fatalf("UnmarshalResponsesRequest: %v", err)
	}
	projected, _, projectionErr := responsesRequestToChatRequest(request)
	if projectionErr != nil {
		t.Fatalf("responsesRequestToChatRequest: %v", projectionErr)
	}
	if projected.MaxOutputTokens == nil || *projected.MaxOutputTokens != 300 {
		t.Fatalf("max_output_tokens = %v, want 300", projected.MaxOutputTokens)
	}
	if projected.MaxComplTokens == nil || *projected.MaxComplTokens != 200 {
		t.Fatalf("max_completion_tokens = %v, want 200", projected.MaxComplTokens)
	}
	if projected.MaxTokens == nil || *projected.MaxTokens != 100 {
		t.Fatalf("max_tokens = %v, want 100", projected.MaxTokens)
	}

	omitted, err := adapteropenai.UnmarshalResponsesRequest([]byte(`{"model":"claude","input":"hello"}`))
	if err != nil {
		t.Fatalf("UnmarshalResponsesRequest omitted: %v", err)
	}
	omittedProjection, _, omittedErr := responsesRequestToChatRequest(omitted)
	if omittedErr != nil {
		t.Fatalf("responsesRequestToChatRequest omitted: %v", omittedErr)
	}
	if omittedProjection.MaxOutputTokens != nil || omittedProjection.MaxComplTokens != nil || omittedProjection.MaxTokens != nil {
		t.Fatalf("omitted output caps = %+v", omittedProjection)
	}
}
