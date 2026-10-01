// Package searchacceptance validates isolated search measurement reports.
package searchacceptance

// SchemaVersion selects the serialized measurement contract.
const SchemaVersion = 1

// Battery defines the frozen workload and source-derived expected identities.
type Battery struct {
	Concurrency int     `json:"concurrency"`
	TimeoutMS   int64   `json:"timeout_ms"`
	Entries     []Query `json:"entries"`
}

// Query defines one public search and its expected complete occurrence set.
type Query struct {
	ID                    string   `json:"id"`
	Query                 string   `json:"query"`
	Filter                Filter   `json:"filter"`
	PageSize              int      `json:"page_size"`
	ExpectedOccurrenceIDs []string `json:"expected_occurrence_ids"`
	ExpectedTotal         int      `json:"expected_total"`
}

// Filter defines the public selectors in a frozen query battery.
type Filter struct {
	Provider             *string  `json:"provider"`
	Workspace            *string  `json:"workspace"`
	ConversationIDs      []string `json:"conversation_ids"`
	After                *string  `json:"after"`
	Before               *string  `json:"before"`
	Roles                []string `json:"roles"`
	IncludeArchived      bool     `json:"include_archived"`
	IncludeSubagents     bool     `json:"include_subagents"`
	MinScore             *float64 `json:"min_score"`
	PerConversationLimit *int     `json:"per_conversation_limit"`
}

// Model identifies the embedding configuration used in a measurement.
type Model struct {
	Name          string `json:"name"`
	Revision      string `json:"revision"`
	Dimension     int    `json:"dimension"`
	Normalization string `json:"normalization"`
}

// Page records returned source identities and continuation state.
type Page struct {
	OccurrenceIDs []string `json:"occurrence_ids"`
	ElapsedMS     float64  `json:"elapsed_ms"`
	HasMore       bool     `json:"has_more"`
}

// Traversal records every page from one search snapshot.
type Traversal struct {
	QueryID string `json:"query_id"`
	Cold    bool   `json:"cold"`
	Total   int    `json:"total"`
	Pages   []Page `json:"pages"`
	Error   string `json:"error,omitempty"`
}

// Resources records measured process memory and storage allocation.
type Resources struct {
	ClydePeakRSSBytes  int64 `json:"clyde_peak_rss_bytes"`
	ModelPeakRSSBytes  int64 `json:"model_peak_rss_bytes"`
	MilvusPeakRSSBytes int64 `json:"milvus_peak_rss_bytes"`
	TotalPeakRSSBytes  int64 `json:"total_peak_rss_bytes"`
	TemporaryBytes     int64 `json:"temporary_bytes"`
	CompactedBytes     int64 `json:"compacted_backend_bytes"`
}

// Ingestion records initial publication and unchanged-pass measurements.
type Ingestion struct {
	Completed                      bool             `json:"completed"`
	UnchangedPassCompleted         bool             `json:"unchanged_pass_completed"`
	SourceReadMS                   float64          `json:"source_read_ms"`
	SelectionMS                    float64          `json:"selection_ms"`
	EmbeddingMS                    float64          `json:"embedding_ms"`
	PersistenceMS                  float64          `json:"persistence_ms"`
	SearchableCompletionMS         float64          `json:"searchable_completion_ms"`
	ModelStartupMS                 float64          `json:"model_startup_ms"`
	SelectedCounts                 map[string]int64 `json:"selected_counts"`
	ExcludedCounts                 map[string]int64 `json:"excluded_counts"`
	DistinctEmbeddings             int64            `json:"distinct_embeddings"`
	VectorWrites                   int64            `json:"vector_writes"`
	VectorReuse                    int64            `json:"vector_reuse"`
	UnchangedPassEmbeddingRequests int64            `json:"unchanged_pass_embedding_requests"`
	UnchangedPassVectorWrites      int64            `json:"unchanged_pass_vector_writes"`
}

// Report combines workload identities and isolated application measurements.
type Report struct {
	SchemaVersion    int         `json:"schema_version"`
	BuildRevision    string      `json:"build_revision"`
	CorpusDigest     string      `json:"corpus_digest"`
	HardwareIdentity string      `json:"hardware_identity"`
	ModelDescriptor  Model       `json:"model_descriptor"`
	BatteryDigest    string      `json:"battery_digest"`
	Concurrency      int         `json:"concurrency"`
	Traversals       []Traversal `json:"traversals"`
	Ingestion        Ingestion   `json:"ingestion"`
	Resources        Resources   `json:"resources"`
}
