package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func TestRunDirectDoesNotReusePreviousResponseIDForRepeatedFreshPromptWithoutConversationID(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	requests := make(chan ResponseCreateWsRequest, 2)
	serverErrors := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer func() { _ = conn.Close() }()

		_, body, err := conn.ReadMessage()
		if err != nil {
			serverErrors <- err
			return
		}
		var req ResponseCreateWsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			serverErrors <- err
			return
		}
		requests <- req

		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp-fresh"}}`,
			`{"type":"response.completed","response":{"id":"resp-fresh","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		} {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	cfg := DirectConfig{
		HTTPClient:       server.Client(),
		WebsocketEnabled: true,
		WebsocketURL:     "ws" + server.URL[len("http"):],
		Token:            "test-token",
		SessionCache:     NewWebsocketSessionCache(nil, 0, nil),
	}
	req := adapteropenai.ChatRequest{
		User: "github|user_123",
		Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"identical first prompt"`),
		}},
	}
	resolved := codexResolvedForTest(adaptermodel.ResolvedAlias{Alias: "gpt-5.4", WireModel: "gpt-5.4"})

	for range 2 {
		if _, err := RunDirect(context.Background(), cfg, req, resolved, "", func(adapterrender.Event) error {
			return nil
		}); err != nil {
			t.Fatalf("run direct: %v", err)
		}
	}

	first := <-requests
	second := <-requests
	if first.PreviousResponseID != "" {
		t.Fatalf("first previous_response_id=%q want empty", first.PreviousResponseID)
	}
	if second.PreviousResponseID != "" {
		t.Fatalf("second previous_response_id=%q want empty for repeated fresh chat", second.PreviousResponseID)
	}
	if first.PromptCacheKey == "" || second.PromptCacheKey == "" {
		t.Fatalf("expected prompt cache keys, got first=%q second=%q", first.PromptCacheKey, second.PromptCacheKey)
	}
	if first.PromptCacheKey != second.PromptCacheKey {
		t.Fatalf("prompt cache key should remain stable for cache reuse: first=%q second=%q", first.PromptCacheKey, second.PromptCacheKey)
	}
	select {
	case err := <-serverErrors:
		t.Fatalf("server error: %v", err)
	default:
	}
}
