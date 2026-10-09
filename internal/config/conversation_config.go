package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConversationSemanticCollectionID       = "clyde-conversations"
	defaultConversationSemanticMilvusAddress      = "localhost:19530"
	defaultConversationSemanticMilvusDatabase     = "default"
	defaultConversationSemanticEmbeddingBaseURL   = "http://localhost:5400/v1"
	defaultConversationSemanticEmbeddingModel     = "nvidia/NV-EmbedCode-7b-v1"
	defaultConversationSemanticEmbeddingDimension = 4096
	defaultConversationSearchNProbe               = 64
	defaultConversationLocalStoreDirName          = "conversation-local"
	defaultConversationWorkerInterval             = time.Minute
	minimumConversationWorkerInterval             = time.Second
)

// ConversationConfig configures raw conversation indexing integrations.
type ConversationConfig struct {
	// IncludeSubagentConversations exposes the transcripts a dispatched agent
	// wrote alongside the conversations a person started. It defaults to false
	// because one working session that dispatches agents can write hundreds of
	// agent transcripts in a day, which crowd list and search results and consume
	// embedding capacity ahead of the user's own work. Turning it on takes effect
	// on the next daemon generation and needs no cache deletion, because the index
	// stores every conversation's origin and applies this setting when it reads.
	//
	// It selects whole conversations, which is a different level from
	// [ConversationSemanticConfig.IndexedContent]. A conversation this hides is
	// absent from every clyde surface, and the engine retains whatever a short
	// manifest omits, so hiding one removes nothing already stored.
	IncludeSubagentConversations bool                       `json:"includeSubagentConversations,omitempty" toml:"include_subagent_conversations,omitempty"`
	Cursor                       CursorConversationConfig   `json:"cursor,omitzero" toml:"cursor,omitempty"`
	Semantic                     ConversationSemanticConfig `json:"semantic,omitzero" toml:"semantic,omitempty"`
}

// CursorConversationConfig configures Cursor raw transcript indexing.
type CursorConversationConfig struct {
	Enabled *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
}

// RawIndexingEnabled reports whether Cursor discovery and projection run.
func (cursor CursorConversationConfig) RawIndexingEnabled() bool {
	return cursor.Enabled == nil || *cursor.Enabled
}

// ConversationSemanticConfig configures conversation semantic search: offering
// conversations to the search engine, and reading them back.
//
// The two are separate settings because they carry different costs. Offering
// conversations embeds text, which occupies the GPU and grows the store, so an
// operator has real reasons to stop it. Reading queries a corpus that already
// exists and costs nothing, so stopping the writes does not put the stored
// conversations out of reach.
type ConversationSemanticConfig struct {
	IngestionEnabled bool   `json:"ingestionEnabled,omitempty" toml:"ingestion_enabled,omitempty"`
	SearchEnabled    bool   `json:"searchEnabled,omitempty" toml:"search_enabled,omitempty"`
	SocketPath       string `json:"socketPath,omitempty" toml:"socket_path,omitempty"`
	CollectionID     string `json:"collectionId,omitempty" toml:"collection_id,omitempty"`
	// IndexedContent names the content kinds offered to the search engine, using
	// the same selector vocabulary the export surface accepts. The names and their
	// validation belong to the conversation package's content-kind taxonomy, which
	// this package cannot import without a cycle, so the values are carried as
	// written and resolved where they are used.
	//
	// It selects parts of a message, which is a different level from
	// [ConversationConfig.IncludeSubagentConversations]. The conversation is still
	// delivered, so the engine reconciles the message rows it stops receiving
	// rather than retaining them.
	//
	// An absent or empty list means the indexing default. Naming no kinds is not
	// how an operator turns indexing off, because that would quietly stop
	// embedding everything; `ingestion_enabled = false` is.
	IndexedContent []string `json:"indexedContent,omitempty" toml:"indexed_content,omitempty"`

	// Backend selects the store for ingestion and search. Empty, "milvus" and
	// "lms" select Milvus. "local" selects LocalRoot and the bundled model.
	// The loader rejects "local" with a Milvus address and rejects "embedded".
	Backend ConversationSemanticBackend `json:"backend,omitempty" toml:"backend,omitempty"`
	// LocalRoot applies to the local backend only. An empty value selects a
	// directory under the Clyde state directory.
	LocalRoot string `json:"localRoot,omitempty" toml:"local_root,omitempty"`

	// IndexedProviders and IndexedRoles limit embedded ingestion and search to
	// the listed providers and message roles. Empty lists select every one.
	IndexedProviders []string `json:"indexedProviders,omitempty" toml:"indexed_providers,omitempty"`
	IndexedRoles     []string `json:"indexedRoles,omitempty" toml:"indexed_roles,omitempty"`
	// IncludeArchived and IncludeSubagents admit archived and subagent
	// conversations to embedded ingestion and search.
	IncludeArchived  bool `json:"includeArchived,omitempty" toml:"include_archived,omitempty"`
	IncludeSubagents bool `json:"includeSubagents,omitempty" toml:"include_subagents,omitempty"`

	// CatalogPath, LockPath, and PoolID identify the embedded library's SQLite
	// catalog, writer lock file, and vector pool.
	CatalogPath string `json:"catalogPath,omitempty" toml:"catalog_path,omitempty"`
	LockPath    string `json:"lockPath,omitempty" toml:"lock_path,omitempty"`
	PoolID      string `json:"poolId,omitempty" toml:"pool_id,omitempty"`

	// MilvusAddress, MilvusDatabase, and MilvusCollection locate the Milvus
	// collection that stores the embedded library's vectors.
	MilvusAddress    string `json:"milvusAddress,omitempty" toml:"milvus_address,omitempty"`
	MilvusDatabase   string `json:"milvusDatabase,omitempty" toml:"milvus_database,omitempty"`
	MilvusCollection string `json:"milvusCollection,omitempty" toml:"milvus_collection,omitempty"`

	// EmbeddingBaseURL is the OpenAI-compatible embedding endpoint. The loader
	// accepts at most one of EmbeddingAPIKeyEnv and EmbeddingAPIKeyFile, and a
	// local endpoint may set neither.
	EmbeddingBaseURL    string `json:"embeddingBaseUrl,omitempty" toml:"embedding_base_url,omitempty"`
	EmbeddingAPIKeyEnv  string `json:"embeddingApiKeyEnv,omitempty" toml:"embedding_api_key_env,omitempty"`
	EmbeddingAPIKeyFile string `json:"embeddingApiKeyFile,omitempty" toml:"embedding_api_key_file,omitempty"`
	// EmbeddingRequestTimeout bounds one embedding request. Zero uses the
	// caller deadline. EmbeddingMaxAttempts and EmbeddingBackoffBase configure
	// retries; unset values use 4 attempts and 200 milliseconds.
	EmbeddingRequestTimeout Duration `json:"embeddingRequestTimeout,omitempty" toml:"embedding_request_timeout,omitempty"`
	EmbeddingMaxAttempts    *int     `json:"embeddingMaxAttempts,omitempty" toml:"embedding_max_attempts,omitempty"`
	EmbeddingBackoffBase    Duration `json:"embeddingBackoffBase,omitempty" toml:"embedding_backoff_base,omitempty"`

	// EmbeddingModel, EmbeddingRevision, VectorDimension, and Normalization
	// form the store descriptor that binds a catalog to one model generation.
	// The catalog saves AnalyzerIdentity as its lexical analyzer identity, and
	// an empty value selects the library's standard analyzer.
	// The query embedding input begins with QueryInstructionPrefix.
	EmbeddingModel         string `json:"embeddingModel,omitempty" toml:"embedding_model,omitempty"`
	EmbeddingDimension     int    `json:"embeddingDimension,omitempty" toml:"embedding_dimension,omitempty"`
	EmbeddingRevision      string `json:"embeddingRevision,omitempty" toml:"embedding_revision,omitempty"`
	VectorDimension        int    `json:"vectorDimension,omitempty" toml:"vector_dimension,omitempty"`
	Normalization          string `json:"normalization,omitempty" toml:"normalization,omitempty"`
	AnalyzerIdentity       string `json:"analyzerIdentity,omitempty" toml:"analyzer_identity,omitempty"`
	QueryInstructionPrefix string `json:"queryInstructionPrefix,omitempty" toml:"query_instruction_prefix,omitempty"`
	// SearchNProbe is the IVF nprobe of a dense conversation search. Unset uses
	// 64, and 0 uses the Milvus index default.
	SearchNProbe *int `json:"searchNprobe,omitempty" toml:"search_nprobe,omitempty"`

	// MaxBatchRows and MaxBatchBytes bound one library write batch.
	// RawBatchTargetBytes bounds the transcript bytes Clyde loads per batch.
	// Zero values use the library and Clyde defaults.
	MaxBatchRows        int   `json:"maxBatchRows,omitempty" toml:"max_batch_rows,omitempty"`
	MaxBatchBytes       int64 `json:"maxBatchBytes,omitempty" toml:"max_batch_bytes,omitempty"`
	RawBatchTargetBytes int64 `json:"rawBatchTargetBytes,omitempty" toml:"raw_batch_target_bytes,omitempty"`

	// The query budgets set the same named library configuration fields.
	// Zero values use the library defaults.
	QueryBlockSize    int      `json:"queryBlockSize,omitempty" toml:"query_block_size,omitempty"`
	QueryWorkers      int      `json:"queryWorkers,omitempty" toml:"query_workers,omitempty"`
	MaxTemporaryBytes int64    `json:"maxTemporaryBytes,omitempty" toml:"max_temporary_bytes,omitempty"`
	MaxSnapshotBytes  int64    `json:"maxSnapshotBytes,omitempty" toml:"max_snapshot_bytes,omitempty"`
	SnapshotTTL       Duration `json:"snapshotTtl,omitempty" toml:"snapshot_ttl,omitempty"`
	QueryTimeout      Duration `json:"queryTimeout,omitempty" toml:"query_timeout,omitempty"`

	// The request limits set the same named library configuration fields.
	// Zero disables each limit.
	MaxPageSize     int `json:"maxPageSize,omitempty" toml:"max_page_size,omitempty"`
	MaxQueryBytes   int `json:"maxQueryBytes,omitempty" toml:"max_query_bytes,omitempty"`
	MaxFilterDepth  int `json:"maxFilterDepth,omitempty" toml:"max_filter_depth,omitempty"`
	MaxFilterValues int `json:"maxFilterValues,omitempty" toml:"max_filter_values,omitempty"`

	// BM25K1, BM25B, and RRFK set the ranking parameters. Zero BM25K1 and RRFK
	// and an unset BM25B use the library defaults; an explicit BM25B of zero is
	// valid.
	BM25K1 float64  `json:"bm25K1,omitempty" toml:"bm25_k1,omitempty"`
	BM25B  *float64 `json:"bm25B,omitempty" toml:"bm25_b,omitempty"`
	RRFK   int      `json:"rrfK,omitempty" toml:"rrf_k,omitempty"`

	// Unset or zero intervals select one minute. Positive intervals must be at
	// least one second.
	SyncInterval         Duration `json:"syncInterval,omitempty" toml:"sync_interval,omitempty"`
	IndexRefreshInterval Duration `json:"indexRefreshInterval,omitempty" toml:"index_refresh_interval,omitempty"`
}

// SyncPassInterval permits shorter sync cadences without shortening the
// transcript growth window.
func (semantic ConversationSemanticConfig) SyncPassInterval() time.Duration {
	if semantic.SyncInterval <= 0 {
		return defaultConversationWorkerInterval
	}
	return time.Duration(semantic.SyncInterval)
}

// IndexRefreshPeriod supplies the raw index cadence independently of semantic
// sync passes.
func (semantic ConversationSemanticConfig) IndexRefreshPeriod() time.Duration {
	if semantic.IndexRefreshInterval <= 0 {
		return defaultConversationWorkerInterval
	}
	return time.Duration(semantic.IndexRefreshInterval)
}

// ConversationSemanticBackend is the implementation behind conversation
// semantic ingestion and search.
type ConversationSemanticBackend string

const (
	// ConversationSemanticBackendLMS selects Milvus, like the empty value.
	ConversationSemanticBackendLMS ConversationSemanticBackend = "lms"
	// ConversationSemanticBackendMilvus selects Milvus.
	ConversationSemanticBackendMilvus ConversationSemanticBackend = "milvus"
	// ConversationSemanticBackendLocal selects the local code store and the
	// static model compiled into the binary.
	ConversationSemanticBackendLocal ConversationSemanticBackend = "local"
	// ConversationSemanticBackendEmbedded selects the in-process shared search
	// library.
	ConversationSemanticBackendEmbedded ConversationSemanticBackend = "embedded"
)

// FeedsEngine reports whether the daemon offers conversations to the search
// engine.
func (semantic ConversationSemanticConfig) FeedsEngine() bool {
	return semantic.IngestionEnabled
}

// AnswersSearch reports whether the daemon answers conversation searches from
// the engine.
func (semantic ConversationSemanticConfig) AnswersSearch() bool {
	return semantic.SearchEnabled
}

// UsesEngine reports whether either direction needs a connection to the engine,
// which is what decides whether the daemon builds one.
func (semantic ConversationSemanticConfig) UsesEngine() bool {
	return semantic.FeedsEngine() || semantic.AnswersSearch()
}

func applyConversationSemanticSearchDefaults(semantic *ConversationSemanticConfig) {
	if strings.TrimSpace(semantic.MilvusAddress) == "" {
		semantic.MilvusAddress = defaultConversationSemanticMilvusAddress
	}
	if strings.TrimSpace(semantic.MilvusDatabase) == "" {
		semantic.MilvusDatabase = defaultConversationSemanticMilvusDatabase
	}
	if strings.TrimSpace(semantic.EmbeddingBaseURL) == "" {
		semantic.EmbeddingBaseURL = defaultConversationSemanticEmbeddingBaseURL
	}
	if strings.TrimSpace(semantic.EmbeddingModel) == "" {
		semantic.EmbeddingModel = defaultConversationSemanticEmbeddingModel
	}
	if semantic.EmbeddingDimension == 0 {
		semantic.EmbeddingDimension = defaultConversationSemanticEmbeddingDimension
	}
	if semantic.SearchNProbe == nil {
		nprobe := defaultConversationSearchNProbe
		semantic.SearchNProbe = &nprobe
	}
}

// Check for an explicit Milvus address before defaults populate it.
func applyConversationSemanticBackendDefaults(semantic *ConversationSemanticConfig) error {
	if ConversationSemanticBackend(strings.TrimSpace(string(semantic.Backend))) != ConversationSemanticBackendLocal {
		applyConversationSemanticSearchDefaults(semantic)
		return nil
	}
	if strings.TrimSpace(semantic.MilvusAddress) != "" {
		return invalidConversationSemanticSetting("milvus_address", fmt.Sprintf("must be unset when %sbackend = %q", conversationSemanticKey, ConversationSemanticBackendLocal))
	}
	if strings.TrimSpace(semantic.LocalRoot) == "" {
		semantic.LocalRoot = filepath.Join(DefaultStateDir(), defaultConversationLocalStoreDirName)
	}
	return nil
}

// DenseSearchParams returns the index search parameters of a dense
// conversation search.
func (semantic ConversationSemanticConfig) DenseSearchParams() map[string]string {
	if semantic.SearchNProbe == nil || *semantic.SearchNProbe <= 0 {
		return nil
	}
	return map[string]string{"nprobe": strconv.Itoa(*semantic.SearchNProbe)}
}

func applyConversationDefaults(conversation *ConversationConfig) error {
	if conversation == nil {
		return nil
	}
	conversation.Semantic.SocketPath = strings.TrimSpace(conversation.Semantic.SocketPath)
	conversation.Semantic.CollectionID = strings.TrimSpace(conversation.Semantic.CollectionID)
	if conversation.Semantic.CollectionID == "" {
		conversation.Semantic.CollectionID = defaultConversationSemanticCollectionID
	}
	if err := applyConversationSemanticBackendDefaults(&conversation.Semantic); err != nil {
		return err
	}
	trimmed := make([]string, 0, len(conversation.Semantic.IndexedContent))
	for _, value := range conversation.Semantic.IndexedContent {
		if selector := strings.TrimSpace(value); selector != "" {
			trimmed = append(trimmed, selector)
		}
	}
	conversation.Semantic.IndexedContent = trimmed
	return normalizeAndValidateConversationSemantic(&conversation.Semantic)
}
