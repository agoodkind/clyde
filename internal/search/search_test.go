package search

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/transcript"
)

type embeddingRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

type embeddingResponse struct {
	Data   []embeddingDatum `json:"data"`
	Model  string           `json:"model"`
	Object string           `json:"object"`
	Usage  embeddingUsage   `json:"usage"`
}

type embeddingDatum struct {
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
	Object    string    `json:"object"`
}

type embeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func TestEmbeddingOnlySearchUsesOpenAIEmbeddingsEndpoint(t *testing.T) {
	t.Parallel()

	var seenPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenPaths = append(seenPaths, request.URL.Path)
		if request.URL.Path != "/v1/embeddings" {
			http.Error(writer, "unexpected path", http.StatusNotFound)
			return
		}

		var payload embeddingRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		inputs := decodeEmbeddingInputs(t, payload.Input)
		response := embeddingResponse{
			Data:   make([]embeddingDatum, 0, len(inputs)),
			Model:  payload.Model,
			Object: "list",
			Usage:  embeddingUsage{PromptTokens: len(inputs), TotalTokens: len(inputs)},
		}
		for index, input := range inputs {
			vector := []float64{0, 1}
			if strings.Contains(input, "needle") {
				vector = []float64{1, 0}
			}
			response.Data = append(response.Data, embeddingDatum{
				Embedding: vector,
				Index:     index,
				Object:    "embedding",
			})
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	cfg := config.SearchConfig{
		Backend: "local",
		Local: config.SearchLocal{
			URL:                server.URL,
			EmbeddingURL:       server.URL,
			ChunkSize:          60,
			EmbeddingThreshold: 0.6,
			EmbeddingModel:     "test-embed",
		},
	}
	messages := []transcript.Message{
		{Role: "user", Timestamp: time.Unix(1, 0), Text: "needle topic lives here"},
		{Role: "assistant", Timestamp: time.Unix(2, 0), Text: "different subject entirely"},
	}

	results, err := embeddingOnlySearch(context.Background(), slog.New(slog.DiscardHandler), messages, "needle", cfg)
	if err != nil {
		t.Fatalf("embeddingOnlySearch() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if len(results[0].Messages) != 1 || results[0].Messages[0].Text != "needle topic lives here" {
		t.Fatalf("matched messages = %#v", results[0].Messages)
	}
	if len(seenPaths) != 2 {
		t.Fatalf("embedding requests = %d, want 2", len(seenPaths))
	}
	for _, path := range seenPaths {
		if path != "/v1/embeddings" {
			t.Fatalf("path = %q, want /v1/embeddings", path)
		}
	}
}

func decodeEmbeddingInputs(t *testing.T, raw json.RawMessage) []string {
	t.Helper()

	var arrayInputs []string
	if err := json.Unmarshal(raw, &arrayInputs); err == nil {
		return arrayInputs
	}

	var singleInput string
	if err := json.Unmarshal(raw, &singleInput); err == nil {
		return []string{singleInput}
	}

	t.Fatalf("unsupported embedding input payload: %s", string(raw))
	return nil
}
