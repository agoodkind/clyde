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
