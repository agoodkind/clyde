package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/lm-semantic-search/library"
	"goodkind.io/lm-semantic-search/library/embedding"
	"goodkind.io/lm-semantic-search/library/milvus"
	"goodkind.io/lm-semantic-search/library/observation"

	"goodkind.io/clyde/internal/config"
)

// embeddedConversationStore is the open embedded ingestion runtime. Clyde owns
// the Milvus client. The library owns its catalog and writer lock. Clyde owns
// the outbox.
type embeddedConversationStore struct {
	milvusClient *milvusclient.Client
	library      *library.Library
	outbox       *conversationSemanticOutbox
	namespace    library.NamespaceSpec
	delivery     *embeddedConversationDelivery
}

// openEmbeddedConversationStore creates the Milvus client for the configured
// address and database, the Milvus vector adapter, and the OpenAI-compatible
// embedder, opens the library with the configured store descriptor and
// budgets, registers the conversation namespace, and opens the outbox at
// outboxPath under lock, the outbox lock that the caller already took. A
// failure closes every part that already opened. The caller still owns lock
// after a failure.
func openEmbeddedConversationStore(
	ctx context.Context,
	semantic config.ConversationSemanticConfig,
	outboxPath string,
	lock *os.File,
	log *slog.Logger,
) (*embeddedConversationStore, error) {
	embedder, err := newObservedEmbeddedConversationEmbedder(ctx, semantic, embeddedOperationObserver{log: log})
	if err != nil {
		log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"stage", "embedder",
			"err", err,
		)
		return nil, err
	}
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: semantic.MilvusAddress, DBName: semantic.MilvusDatabase})
	if err != nil {
		log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"stage", "milvus_client",
			"milvus_address", semantic.MilvusAddress,
			"milvus_database", semantic.MilvusDatabase,
			"err", err,
		)
		return nil, fmt.Errorf("create Milvus client for %s database %s: %w", semantic.MilvusAddress, semantic.MilvusDatabase, err)
	}
	store, err := openEmbeddedConversationStoreWithClient(ctx, semantic, outboxPath, lock, client, embedder, log)
	if err != nil {
		log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"stage", "library",
			"catalog_path", semantic.CatalogPath,
			"err", err,
		)
		if closeErr := client.Close(context.WithoutCancel(ctx)); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("close Milvus client: %w", closeErr))
		}
		return nil, err
	}
	log.InfoContext(
		ctx, "daemon.conversation_semantic_embedded.opened",
		"pid", os.Getpid(),
		"concern", "conversation.semantic",
		"component", "daemon",
		"collection_id", semantic.CollectionID,
		"catalog_path", semantic.CatalogPath,
		"milvus_database", semantic.MilvusDatabase,
		"milvus_collection", semantic.MilvusCollection,
		"outbox_path", outboxPath,
	)
	return store, nil
}

// openEmbeddedConversationStoreWithClient opens the library over client and
// embedder, registers the namespace, and opens the outbox under lock. The
// caller closes client and lock when this function fails.
func openEmbeddedConversationStoreWithClient(
	ctx context.Context,
	semantic config.ConversationSemanticConfig,
	outboxPath string,
	lock *os.File,
	client *milvusclient.Client,
	embedder library.Embedder,
	log *slog.Logger,
) (*embeddedConversationStore, error) {
	vectors, err := milvus.New(client, milvus.Config{
		Observer:           embeddedOperationObserver{log: log},
		Database:           semantic.MilvusDatabase,
		Collection:         semantic.MilvusCollection,
		QueryMode:          semantic.MilvusQueryMode,
		MaxScoreWindow:     semantic.MilvusMaxScoreWindow,
		MaxVerifyBatchRows: semantic.MilvusMaxVerifyBatchRows,
	})
	if err != nil {
		log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.vector_adapter_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("create Milvus vector adapter for %s: %w", semantic.MilvusCollection, err)
	}
	store, err := openEmbeddedConversationLibrary(ctx, semantic, outboxPath, lock, vectors, embedder, log)
	if err != nil {
		return nil, err
	}
	store.milvusClient = client
	return store, nil
}

// openEmbeddedConversationLibrary opens the library over vectors and
// embedder, registers the conversation namespace, and opens the outbox under
// lock. The returned store has no Milvus client. The caller sets it. The
// caller still owns lock after a failure.
func openEmbeddedConversationLibrary(
	ctx context.Context,
	semantic config.ConversationSemanticConfig,
	outboxPath string,
	lock *os.File,
	vectors library.VectorStore,
	embedder library.Embedder,
	log *slog.Logger,
) (*embeddedConversationStore, error) {
	libraryConfig := embeddedLibraryConfig(semantic, vectors, embedder)
	libraryConfig.Observer = embeddedOperationObserver{log: log}
	opened, err := library.Open(ctx, libraryConfig)
	if err != nil {
		log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.library_open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"catalog_path", semantic.CatalogPath,
			"err", err,
		)
		return nil, fmt.Errorf("open shared search library catalog %s: %w", semantic.CatalogPath, err)
	}
	namespace := embeddedConversationNamespaceForProfile(semantic.CollectionID, semantic.ProjectionProfile)
	if err := opened.RegisterNamespace(ctx, namespace); err != nil {
		log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.register_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", semantic.CollectionID,
			"err", err,
		)
		return nil, errors.Join(fmt.Errorf("register namespace %s: %w", semantic.CollectionID, err), opened.Close())
	}
	outbox, err := openLockedConversationSemanticOutbox(ctx, outboxPath, lock)
	if err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	maxBatchRows := semantic.MaxBatchRows
	if maxBatchRows == 0 {
		maxBatchRows = embeddedDefaultMaxBatchRows
	}
	maxBatchBytes := semantic.MaxBatchBytes
	if maxBatchBytes == 0 {
		maxBatchBytes = embeddedDefaultMaxBatchBytes
	}
	return &embeddedConversationStore{
		milvusClient: nil,
		library:      opened,
		outbox:       outbox,
		namespace:    namespace,
		delivery: &embeddedConversationDelivery{
			library:        opened,
			outbox:         outbox,
			maxBatchRows:   maxBatchRows,
			maxBatchBytes:  maxBatchBytes,
			maxReplayBytes: conversationSemanticBatchBytes,
			log:            log,
		},
	}, nil
}

// embeddedLibraryConfig maps the configuration keys onto the library
// configuration. A zero budget selects the library default, and an empty
// analyzer identity selects [library.StandardAnalyzer].
func embeddedLibraryConfig(semantic config.ConversationSemanticConfig, vectors library.VectorStore, embedder library.Embedder) library.Config {
	return library.Config{
		Observer: nil,
		Store: library.StoreDescriptor{
			CatalogPath:       semantic.CatalogPath,
			LockPath:          semantic.LockPath,
			PoolID:            semantic.PoolID,
			EmbeddingModel:    semantic.EmbeddingModel,
			EmbeddingRevision: semantic.EmbeddingRevision,
			Dimension:         semantic.VectorDimension,
			Normalization:     semantic.Normalization,
		},
		Vectors:                vectors,
		Embedder:               embedder,
		MaxBatchRows:           semantic.MaxBatchRows,
		MaxBatchBytes:          semantic.MaxBatchBytes,
		QueryBlockSize:         semantic.QueryBlockSize,
		QueryWorkers:           semantic.QueryWorkers,
		MaxTemporaryBytes:      semantic.MaxTemporaryBytes,
		SnapshotTTL:            semantic.SnapshotTTL.AsDuration(),
		MaxSnapshotBytes:       semantic.MaxSnapshotBytes,
		QueryTimeout:           semantic.QueryTimeout.AsDuration(),
		MaxPageSize:            semantic.MaxPageSize,
		MaxQueryBytes:          semantic.MaxQueryBytes,
		MaxFilterDepth:         semantic.MaxFilterDepth,
		MaxFilterValues:        semantic.MaxFilterValues,
		AnalyzerIdentity:       semantic.AnalyzerIdentity,
		QueryInstructionPrefix: semantic.QueryInstructionPrefix,
		SearchMode:             0,
		BM25K1:                 semantic.BM25K1,
		BM25B:                  semantic.BM25B,
		RRFK:                   semantic.RRFK,
	}
}

// embeddedCredentialSource is the kind of embedding credential reference the
// configuration sets.
type embeddedCredentialSource string

const (
	embeddedCredentialSourceNone embeddedCredentialSource = "none"
	embeddedCredentialSourceEnv  embeddedCredentialSource = "env"
	embeddedCredentialSourceFile embeddedCredentialSource = "file"
)

// errEmbeddedCredentialUnavailable wraps every credential resolution failure.
// The failure messages state the source kind only. They never contain the
// environment variable name, the key file path, or the credential.
var errEmbeddedCredentialUnavailable = errors.New("embedding credential unavailable")

func embeddedCredentialSourceOf(semantic config.ConversationSemanticConfig) embeddedCredentialSource {
	switch {
	case semantic.EmbeddingAPIKeyEnv != "":
		return embeddedCredentialSourceEnv
	case semantic.EmbeddingAPIKeyFile != "":
		return embeddedCredentialSourceFile
	default:
		return embeddedCredentialSourceNone
	}
}

// newEmbeddedConversationEmbedder builds the OpenAI-compatible embedder from
// the embedding keys. It resolves the credential reference. Its log records
// and errors state only whether a credential is configured and its source
// kind.
func newEmbeddedConversationEmbedder(ctx context.Context, semantic config.ConversationSemanticConfig) (library.Embedder, error) {
	return newObservedEmbeddedConversationEmbedder(ctx, semantic, nil)
}

func newObservedEmbeddedConversationEmbedder(ctx context.Context, semantic config.ConversationSemanticConfig, observer observation.Observer) (library.Embedder, error) {
	apiKey, err := resolveEmbeddedConversationAPIKey(semantic)
	if err != nil {
		source := embeddedCredentialSourceOf(semantic)
		slog.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.credential_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"embedding_credential_configured", source != embeddedCredentialSourceNone,
			"embedding_credential_source", string(source),
			"err", err,
		)
		return nil, err
	}
	maxAttempts := 0
	if semantic.EmbeddingMaxAttempts != nil {
		maxAttempts = *semantic.EmbeddingMaxAttempts
	}
	embedder, err := embedding.NewOpenAI(ctx, embedding.OpenAIConfig{
		Observer:       observer,
		BaseURL:        semantic.EmbeddingBaseURL,
		APIKey:         apiKey,
		Model:          semantic.EmbeddingModel,
		Dimension:      semantic.VectorDimension,
		RequestTimeout: semantic.EmbeddingRequestTimeout.AsDuration(),
		MaxAttempts:    maxAttempts,
		BackoffBase:    semantic.EmbeddingBackoffBase.AsDuration(),
	})
	if err != nil {
		slog.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.embedder_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"embedding_base_url", semantic.EmbeddingBaseURL,
			"embedding_model", semantic.EmbeddingModel,
			"err", err,
		)
		return nil, fmt.Errorf("create embedding adapter for %s: %w", semantic.EmbeddingBaseURL, err)
	}
	return embedder, nil
}

// resolveEmbeddedConversationAPIKey reads the credential that
// embedding_api_key_env or embedding_api_key_file names. With neither key the
// embedder sends no Authorization header. A named source that yields an empty
// value is an error. A file read failure keeps only its [fs.ErrNotExist] or
// [fs.ErrPermission] class and drops the path.
func resolveEmbeddedConversationAPIKey(semantic config.ConversationSemanticConfig) (string, error) {
	if semantic.EmbeddingAPIKeyEnv != "" {
		value := strings.TrimSpace(os.Getenv(semantic.EmbeddingAPIKeyEnv))
		if value == "" {
			return "", fmt.Errorf("%w: the configured embedding credential environment variable is empty or unset", errEmbeddedCredentialUnavailable)
		}
		return value, nil
	}
	if semantic.EmbeddingAPIKeyFile != "" {
		contents, err := os.ReadFile(semantic.EmbeddingAPIKeyFile)
		if err != nil {
			reason := "read failed"
			switch {
			case errors.Is(err, fs.ErrNotExist):
				reason = "file does not exist"
			case errors.Is(err, fs.ErrPermission):
				reason = "permission denied"
			}
			slog.Warn(
				"daemon.conversation_semantic_embedded.credential_file_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"embedding_credential_source", string(embeddedCredentialSourceFile),
				"err", reason,
			)
			return "", fmt.Errorf("%w: read the configured embedding credential file: %s", errEmbeddedCredentialUnavailable, reason)
		}
		value := strings.TrimSpace(string(contents))
		if value == "" {
			return "", fmt.Errorf("%w: the configured embedding credential file is empty", errEmbeddedCredentialUnavailable)
		}
		return value, nil
	}
	return "", nil
}

// close runs in the reverse order of the open. It closes the outbox database,
// then the library, then the Milvus client, and then releases the outbox
// lock. The library leaves the Milvus client open, and Clyde closes it.
func (store *embeddedConversationStore) close(ctx context.Context) error {
	outboxErr := store.outbox.closeDatabase()
	libraryErr := store.library.Close()
	var milvusErr error
	if store.milvusClient != nil {
		if err := store.milvusClient.Close(ctx); err != nil {
			milvusErr = fmt.Errorf("close Milvus client: %w", err)
		}
	}
	lockErr := store.outbox.releaseLock()
	err := errors.Join(outboxErr, libraryErr, milvusErr, lockErr)
	if err != nil {
		slog.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.close_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
	}
	return err
}
