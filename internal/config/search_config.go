package config

// SearchConfig configures the LLM backend for the conversation analyze step.
// Search itself is engine-backed semantic retrieval and uses no LLM; the model
// configured here reads a completed search's result set on demand. Backend
// names a registered analyze backend; "local" is the built-in
// OpenAI-compatible backend, and any other name (default "claude") resolves
// through the backend registry, so provider-specific backends stay out of this
// package.
type SearchConfig struct {
	// Backend is "claude" (default) or "local".
	Backend string      `json:"backend,omitempty" toml:"backend,omitempty"`
	Local   SearchLocal `json:"local,omitzero" toml:"local,omitempty"`
}

// SearchLocal configures a local OpenAI-compatible LLM endpoint for analyze.
type SearchLocal struct {
	URL              string  `json:"url,omitempty" toml:"url,omitempty"`
	Token            string  `json:"token,omitempty" toml:"token,omitempty"`
	Model            string  `json:"model,omitempty" toml:"model,omitempty"`
	Temperature      float64 `json:"temperature" toml:"temperature"`
	TopP             float64 `json:"topP" toml:"top_p"`
	FrequencyPenalty float64 `json:"frequencyPenalty" toml:"frequency_penalty"`
}
