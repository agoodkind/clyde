package antigravity_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/config"
)

func TestAvailableModelsReportsOnlyAccountGeminiIDs(t *testing.T) {
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost ||
				request.URL.Path != "/v1internal:fetchAvailableModels" {
				t.Errorf("unexpected model discovery target %s %s", request.Method, request.URL)
			}

			var body struct {
				Project string `json:"project"`
			}

			if err := json.NewDecoder(request.Body).Decode(&body); err != nil ||
				body.Project != "model-project" {
				t.Errorf("unexpected model discovery body %v, error %v", body, err)
			}

			_, _ = io.WriteString(
				writer,
				`{"models":{"gemini-test":{},"claude-test":{},"gpt-oss-test":{}}}`,
			)
		}),
	)
	defer server.Close()

	backend, err := antigravity.NewProvider(provider.Deps{
		Config: config.AdapterConfig{
			Antigravity: config.AdapterAntigravity{
				BaseURL:       server.URL,
				Project:       "model-project",
				ClientVersion: "1.0.0",
			},
		},
		Auth:       fixtureAuth{},
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	models, err := backend.AvailableModels(t.Context())
	if err != nil || !slices.Equal(models, []string{"gemini-test"}) {
		t.Fatalf("available models = %v, error = %v", models, err)
	}
}
