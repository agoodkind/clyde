// Package search provides LLM-powered semantic search across conversation transcripts.
package search

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"

	"goodkind.io/clyde/internal/clock"
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

// defaultBackend is the search backend used when cfg.Backend is empty.
const defaultBackend = "claude"

// newClientForModel creates a client using a specific model. The built-in
// "local" backend is constructed inline; every other backend name is resolved
// through the backend registry, so provider-specific backends register
// themselves rather than being named here.
func newClientForModel(cfg config.SearchConfig, model string) Client {
	if cfg.Backend == "local" {
		localCfg := cfg.Local
		localCfg.Model = model
		return newLocalClient(localCfg)
	}
	backend := cfg.Backend
	if backend == "" {
		backend = defaultBackend
	}
	factory, ok := defaultBackendRegistry.lookup(backend)
	if !ok {
		return errorClient{err: fmt.Errorf("search backend %q is not registered", backend)}
	}
	return factory(model)
}

// BackendFactory builds a search [Client] for one model. A backend package
// registers a factory under its name with [RegisterBackend].
type BackendFactory func(model string) Client

// backendRegistry maps a backend name to its client factory. Backend
// implementations other than the built-in local backend (such as the Claude
// CLI backend) live in their own package and self-register via init(); the
// search package never imports a backend package, so the registry inverts the
// dependency the way the MITM provider registry does.
type backendRegistry struct {
	mu        sync.RWMutex
	factories map[string]BackendFactory
}

var defaultBackendRegistry = &backendRegistry{mu: sync.RWMutex{}, factories: map[string]BackendFactory{}}

// RegisterBackend installs a backend factory under name. Backend packages call
// it once from an init() function at package load. A second registration under
// the same name replaces the first.
func RegisterBackend(name string, factory BackendFactory) {
	if factory == nil {
		return
	}
	defaultBackendRegistry.register(name, factory)
}

func (r *backendRegistry) register(name string, factory BackendFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

func (r *backendRegistry) lookup(name string) (BackendFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[name]
	return factory, ok
}

// errorClient is returned when no backend is registered for the configured
// name. Its Complete reports the configuration error to the caller instead of
// panicking, so a misconfigured backend surfaces as a clear search failure.
type errorClient struct {
	err error
}

// Complete reports the registration error.
func (c errorClient) Complete(context.Context, string) (string, error) {
	return "", c.err
}

// localRequestTimeout bounds one search completion so a wedged or unreachable
// local model cannot consume the whole search RPC budget. It is well under the
// daemon's 10-minute analysis RPC timeout.
const localRequestTimeout = 90 * time.Second

// localClient uses an OpenAI-compatible endpoint (LM Studio, Ollama, etc.)
type localClient struct {
	client  *openai.Client
	model   string
	baseURL string
	cfg     config.SearchLocal
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
		option.WithRequestTimeout(localRequestTimeout),
	}
	if cfg.Token != "" {
		opts = append(opts, option.WithAPIKey(cfg.Token))
	} else {
		opts = append(opts, option.WithAPIKey("not-needed"))
	}

	c := openai.NewClient(opts...)
	return &localClient{
		client:  &c,
		model:   model,
		baseURL: url,
		cfg:     cfg,
	}
}

func (c *localClient) Complete(ctx context.Context, prompt string) (string, error) {
	log := slog.Default()
	promptLen := len(prompt)
	start := clock.Now()

	log.DebugContext(ctx, "llm request", "concern", "search", "model", c.model,
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
	elapsed := clock.Since(start)

	if err != nil {
		log.ErrorContext(ctx, "llm request failed", "concern", "search", "model", c.model,
			"endpoint", c.baseURL,
			"duration", elapsed.Round(time.Millisecond),
			"err", err,
		)
		return "", fmt.Errorf("local LLM request to %s failed: %w", c.baseURL, err)
	}
	if len(resp.Choices) == 0 {
		log.WarnContext(ctx, "llm returned no choices", "concern", "search", "model", c.model, "duration", elapsed.Round(time.Millisecond))
		return "", fmt.Errorf("local LLM returned no choices")
	}

	result := resp.Choices[0].Message.Content
	log.InfoContext(ctx, "llm response", "concern", "search", "model", c.model,
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
