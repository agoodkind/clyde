package adapter

import (
	"encoding/json"
	"net/http"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestOpenAIConformanceModelsListAndRetrieve(t *testing.T) {
	listeners := startConformanceServer(t, newConformanceUpstream(conformanceUsageWithoutDetails))

	list := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models", "")
	if list.status != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", list.status, list.body)
	}
	listObject := decodeJSONObject(t, list.body)
	if string(requireMember(t, listObject, "object")) != `"list"` {
		t.Fatalf("list object = %s, want list", listObject["object"])
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(requireMember(t, listObject, "data"), &entries); err != nil || len(entries) == 0 {
		t.Fatalf("decode list data: %v; body=%s", err, list.body)
	}
	for _, entry := range entries {
		for _, key := range []string{"id", "object", "created", "owned_by"} {
			requireMember(t, entry, key)
		}
		var created int64
		if err := json.Unmarshal(entry["created"], &created); err != nil || created <= 0 {
			t.Fatalf("model created = %s, want a positive Unix time", entry["created"])
		}
	}

	cursorList := sendConformance(t, http.MethodGet, listeners.cursor+"/v1/models", "")
	var cursorModels struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(cursorList.body, &cursorModels); err != nil || len(cursorModels.Data) == 0 {
		t.Fatalf("decode cursor list: %v; body=%s", err, cursorList.body)
	}
	if _, present := cursorModels.Data[0]["created"]; present {
		t.Fatalf("cursor model list gained created: %s", cursorList.body)
	}

	for _, modelID := range []string{"gpt-anthropic-alias", "gpt-future"} {
		retrieved := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models/"+modelID, "")
		if retrieved.status != http.StatusOK {
			t.Fatalf("retrieve %s status = %d; body=%s", modelID, retrieved.status, retrieved.body)
		}
		var entry adapteropenai.ModelEntry
		if err := json.Unmarshal(retrieved.body, &entry); err != nil {
			t.Fatalf("decode retrieved model: %v", err)
		}
		if entry.ID != modelID || entry.Object != "model" || entry.Created <= 0 || entry.OwnedBy == "" {
			t.Fatalf("retrieved model = %+v, want id %s with documented fields", entry, modelID)
		}
	}

	fallback := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models/"+routingFallbackModelID, "")
	if fallback.status != http.StatusOK || string(fallback.body) != routingFallbackModelBody {
		t.Fatalf("fallback model = %d %s, want the fallback upstream model object unchanged", fallback.status, fallback.body)
	}

	oversized := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models/"+routingFallbackOversizedModelID, "")
	if oversized.status != http.StatusBadGateway || decodeErrorEnvelope(t, oversized.body).Type != "server_error" {
		t.Fatalf("oversized fallback model = %d %s, want 502 server_error", oversized.status, oversized.body)
	}

	missing := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models/does-not-exist", "")
	missingError := decodeErrorEnvelope(t, missing.body)
	if missing.status != http.StatusNotFound || missingError.Type != "invalid_request_error" || missingError.Code != "model_not_found" {
		t.Fatalf("missing model = %d %+v, want 404 invalid_request_error model_not_found", missing.status, missingError)
	}

	wrongMethod := sendConformance(t, http.MethodPost, listeners.openAI+"/v1/models", `{}`)
	if wrongMethod.status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models status = %d, want 405; body=%s", wrongMethod.status, wrongMethod.body)
	}
}
