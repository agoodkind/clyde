// Package search provides LLM-powered semantic search across conversation transcripts.
package search

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"

	"goodkind.io/clyde/internal/config"
)

// Client abstracts the LLM backend for conversation search.
type Client interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// NewClientForModel is the exported form of newClientForModel for use outside this package.
func NewClientForModel(cfg config.SearchConfig, model string) Client {
	return newClientForModel(cfg, model)
}

// newClientForModel creates a client using a specific model, inheriting
// all other settings (URL, token, sampling params) from the local config.
func newClientForModel(cfg config.SearchConfig, model string) Client {
	if cfg.Backend == "local" {
		localCfg := cfg.Local
		localCfg.Model = model
		return newLocalClient(localCfg)
	}
	claudeCfg := cfg.Claude
	claudeCfg.Model = model
	return newClaudeClient(claudeCfg)
}

// localClient uses an OpenAI-compatible endpoint (LM Studio, Ollama, etc.)
type localClient struct {
	client *openai.Client
	model  string
	cfg    config.SearchLocal
}

func newLocalClient(cfg config.SearchLocal) *localClient {
	url := cfg.URL
	if url == "" {
		url = "http://localhost:1234"
	}
	model := cfg.Model
	if model == "" {
		model = "qwen2.5-coder-32b"
	}

	opts := []option.RequestOption{
		option.WithBaseURL(url + "/v1"),
	}
	if cfg.ResolvedToken() != "" {
		opts = append(opts, option.WithAPIKey(cfg.ResolvedToken()))
	} else {
		opts = append(opts, option.WithAPIKey("not-needed"))
	}

	c := openai.NewClient(opts...)
	return &localClient{
		client: &c,
		model:  model,
		cfg:    cfg,
	}
}

func (c *localClient) Complete(ctx context.Context, prompt string) (string, error) {
	log := slog.Default()
	promptLen := len(prompt)
	start := searchClock.Now()

	log.DebugContext(ctx, "llm request",
		"model", c.model,
		"prompt_chars", promptLen,
		"prompt_preview", truncate(prompt, 200),
	)

	resp, err := c.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
		Temperature:      param.NewOpt(c.cfg.Temperature),
		TopP:             param.NewOpt(c.cfg.TopP),
		FrequencyPenalty: param.NewOpt(c.cfg.FrequencyPenalty),
		MaxTokens:        param.NewOpt(int64(512)),
	})
	elapsed := time.Since(start)

	if err != nil {
		log.ErrorContext(ctx, "llm request failed",
			"model", c.model,
			"duration", elapsed.Round(time.Millisecond),
			"err", err,
		)
		return "", fmt.Errorf("local LLM request failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		log.WarnContext(ctx, "llm returned no choices", "model", c.model, "duration", elapsed.Round(time.Millisecond))
		return "", fmt.Errorf("local LLM returned no choices")
	}

	result := resp.Choices[0].Message.Content
	log.InfoContext(ctx, "llm response",
		"model", c.model,
		"prompt_chars", promptLen,
		"response_chars", len(result),
		"response_preview", truncate(result, 200),
		"duration", elapsed.Round(time.Millisecond),
	)
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// claudeClient shells out to `claude -p` for search.
type claudeClient struct {
	model string
}

func newClaudeClient(cfg config.SearchClaude) *claudeClient {
	model := cfg.Model
	if model == "" {
		model = "haiku"
	}
	return &claudeClient{model: model}
}

func (c *claudeClient) Complete(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", c.model, prompt)
	output, err := cmd.Output()
	if err != nil {
		slog.ErrorContext(ctx, "claude search request failed", "model", c.model, "err", err)
		return "", fmt.Errorf("claude -p failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}
