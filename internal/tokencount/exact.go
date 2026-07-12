package tokencount

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// maxRefineCalls bounds how many authoritative count calls a single cap makes.
const maxRefineCalls = 3

// ExactCounter counts tokens with an authoritative provider API. Implementations
// perform network I/O and require a context. They are used only to refine a
// candidate the local counter already located.
type ExactCounter interface {
	Count(ctx context.Context, text, model string) (int, error)
}

// anthropicExactCounter counts tokens via Anthropic's messages/count_tokens
// endpoint. It authenticates with a dedicated x-api-key, never the subscription
// OAuth token, so the count path is isolated from interactive auth.
type anthropicExactCounter struct {
	httpClient *http.Client
	apiKey     string
	url        string
	version    string
}

type anthropicCountMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicCountRequest struct {
	Model    string                  `json:"model"`
	Messages []anthropicCountMessage `json:"messages"`
}

type anthropicCountResponse struct {
	InputTokens int `json:"input_tokens"`
}

// NewAnthropicExactCounter builds an Anthropic count_tokens client that sends
// x-api-key auth. A nil client or empty key yields a counter whose Count always
// errors, so callers fall back to the local estimate.
func NewAnthropicExactCounter(httpClient *http.Client, apiKey, url, version string) ExactCounter {
	return anthropicExactCounter{httpClient: httpClient, apiKey: apiKey, url: url, version: version}
}

// Count returns the exact Anthropic input-token count for text under model.
func (c anthropicExactCounter) Count(ctx context.Context, text, model string) (int, error) {
	if c.httpClient == nil || c.apiKey == "" {
		return 0, errors.New("anthropic count: missing client or api key")
	}
	payload, err := json.Marshal(anthropicCountRequest{
		Model:    model,
		Messages: []anthropicCountMessage{{Role: "user", Content: text}},
	})
	if err != nil {
		return 0, errors.New("anthropic count: marshal request failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return 0, errors.New("anthropic count: build request failed")
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Anthropic-Version", c.version)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, errors.New("anthropic count: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("anthropic count: status %d", resp.StatusCode)
	}
	var decoded anthropicCountResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, errors.New("anthropic count: decode response failed")
	}
	return decoded.InputTokens, nil
}

// openAIExactCounter counts tokens via OpenAI's responses/input_tokens endpoint.
type openAIExactCounter struct {
	httpClient *http.Client
	apiKey     string
	url        string
}

type openAICountRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type openAICountResponse struct {
	InputTokens int `json:"input_tokens"`
}

// NewOpenAIExactCounter builds an OpenAI input_tokens client that sends Bearer
// auth. A nil client or empty key yields a counter whose Count always errors.
func NewOpenAIExactCounter(httpClient *http.Client, apiKey, url string) ExactCounter {
	return openAIExactCounter{httpClient: httpClient, apiKey: apiKey, url: url}
}

// Count returns the exact OpenAI input-token count for text under model.
func (c openAIExactCounter) Count(ctx context.Context, text, model string) (int, error) {
	if c.httpClient == nil || c.apiKey == "" {
		return 0, errors.New("openai count: missing client or api key")
	}
	payload, err := json.Marshal(openAICountRequest{Model: model, Input: text})
	if err != nil {
		return 0, errors.New("openai count: marshal request failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return 0, errors.New("openai count: build request failed")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, errors.New("openai count: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("openai count: status %d", resp.StatusCode)
	}
	var decoded openAICountResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, errors.New("openai count: decode response failed")
	}
	return decoded.InputTokens, nil
}

// CapToLastTokensExact caps body to budget. It uses the local counter to locate
// a candidate tail, then verifies with the exact counter, shrinking the
// candidate up to maxRefineCalls times until the authoritative count is at or
// under budget. Any exact error returns the current candidate, so the cap never
// fails and never exceeds budget. A nil exact counter returns the local result.
func CapToLastTokensExact(ctx context.Context, body string, budget int, local Counter, exact ExactCounter, model string) (string, bool) {
	capped, _, truncated := CapToLastTokens(body, budget, local)
	if exact == nil || capped == "" || budget <= 0 {
		return capped, truncated
	}
	current := capped
	for range maxRefineCalls {
		count, err := exact.Count(ctx, current, model)
		if err != nil {
			return current, truncated
		}
		if count <= budget || count <= 0 {
			return current, truncated
		}
		localCount := local.Estimate(current)
		target := int(float64(localCount) * float64(budget) / float64(count))
		if target >= localCount {
			target = localCount - 1
		}
		if target <= 0 {
			return "", true
		}
		current, _, _ = CapToLastTokens(current, target, local)
		truncated = true
		if current == "" {
			return "", true
		}
	}
	return current, truncated
}
