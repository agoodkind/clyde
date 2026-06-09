package config

import (
	"strings"
)

// SearchDepth identifies a requested search effort tier.
type SearchDepth string

const (
	// SearchDepthQuick disables the local LLM pipeline.
	SearchDepthQuick SearchDepth = "quick"
	// SearchDepthNormal runs the first search pipeline layer.
	SearchDepthNormal SearchDepth = "normal"
	// SearchDepthDeep runs the sweep and rerank pipeline layers.
	SearchDepthDeep SearchDepth = "deep"
	// SearchDepthExtraDeep runs all configured search pipeline layers.
	SearchDepthExtraDeep SearchDepth = "extra-deep"
)

// SearchConfig configures the LLM backend for conversation search. Backend
// names a registered search backend; "local" is the built-in OpenAI-compatible
// backend, and any other name (default "claude") resolves through the search
// backend registry, so provider-specific backends stay out of this package.
type SearchConfig struct {
	// Backend is "claude" (default) or "local".
	Backend string      `json:"backend,omitempty" toml:"backend,omitempty"`
	Local   SearchLocal `json:"local,omitzero" toml:"local,omitempty"`
}

// SearchLocal configures a local OpenAI-compatible LLM endpoint.
type SearchLocal struct {
	URL                string        `json:"url,omitempty" toml:"url,omitempty"`
	Token              string        `json:"token,omitempty" toml:"token,omitempty"`
	EmbeddingURL       string        `json:"embeddingUrl,omitempty" toml:"embedding_url,omitempty"`
	EmbeddingToken     string        `json:"embeddingToken,omitempty" toml:"embedding_token,omitempty"`
	Model              string        `json:"model,omitempty" toml:"model,omitempty"`
	RerankModel        string        `json:"rerankModel,omitempty" toml:"rerank_model,omitempty"`
	DeepModel          string        `json:"deepModel,omitempty" toml:"deep_model,omitempty"`
	Pipeline           []SearchLayer `json:"pipeline,omitempty" toml:"pipeline,omitempty"`
	Temperature        float64       `json:"temperature" toml:"temperature"`
	TopP               float64       `json:"topP" toml:"top_p"`
	FrequencyPenalty   float64       `json:"frequencyPenalty" toml:"frequency_penalty"`
	MaxConcurrent      int           `json:"maxConcurrent,omitempty" toml:"max_concurrent,omitempty"`
	ChunkSize          int           `json:"chunkSize,omitempty" toml:"chunk_size,omitempty"`
	MaxMemoryGB        int           `json:"maxMemoryGB,omitempty" toml:"max_memory_gb,omitempty"`
	ContextLength      int           `json:"contextLength,omitempty" toml:"context_length,omitempty"`
	EmbeddingThreshold float64       `json:"embeddingThreshold,omitempty" toml:"embedding_threshold,omitempty"`
	EmbeddingModel     string        `json:"embeddingModel,omitempty" toml:"embedding_model,omitempty"`
}

// ResolvedEmbeddingURL returns the base URL for OpenAI-style embedding
// requests. When EmbeddingURL is empty it falls back to URL.
func (s SearchLocal) ResolvedEmbeddingURL() string {
	if trimmed := strings.TrimSpace(s.EmbeddingURL); trimmed != "" {
		return strings.TrimSuffix(trimmed, "/")
	}
	return strings.TrimSuffix(strings.TrimSpace(s.URL), "/")
}

// ResolvedEmbeddingToken returns the bearer token for the embedding
// endpoint. When EmbeddingToken is empty it falls back to Token.
func (s SearchLocal) ResolvedEmbeddingToken() string {
	if s.EmbeddingToken != "" {
		return s.EmbeddingToken
	}
	return s.Token
}

// SearchLayer defines one stage of the search pipeline.
type SearchLayer struct {
	Name  string `json:"name" toml:"name"`   // "sweep", "rerank", "deep"
	Model string `json:"model" toml:"model"` // model to use for this layer
}

// ResolvePipeline returns the LLM pipeline layers for a given depth.
func (s SearchLocal) ResolvePipeline(depth string) []SearchLayer {
	searchDepth := SearchDepth(strings.TrimSpace(depth))
	if searchDepth == SearchDepthQuick {
		return nil
	}

	if len(s.Pipeline) > 0 {
		switch searchDepth {
		case SearchDepthQuick:
			return nil
		case SearchDepthNormal:
			if len(s.Pipeline) >= 1 {
				return s.Pipeline[:1]
			}
		case SearchDepthDeep:
			if len(s.Pipeline) >= 2 {
				return s.Pipeline[:2]
			}
			return s.Pipeline
		case SearchDepthExtraDeep:
			return s.Pipeline
		default:
			return s.Pipeline
		}
		return s.Pipeline
	}

	var layers []SearchLayer
	model := s.Model
	if model == "" {
		model = "qwen2.5-coder-32b"
	}
	layers = append(layers, SearchLayer{Name: "sweep", Model: model})

	if searchDepth == SearchDepthNormal {
		return layers
	}

	if s.RerankModel != "" {
		layers = append(layers, SearchLayer{Name: "rerank", Model: s.RerankModel})
	}

	if searchDepth == SearchDepthExtraDeep && s.DeepModel != "" {
		layers = append(layers, SearchLayer{Name: "deep", Model: s.DeepModel})
	}

	return layers
}
