package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
)

type initialIndexProgress func(completed int, total int)

type initialSemanticClient interface {
	conversationSemanticClient
	Register(context.Context, string) error
	Close() error
}

var dialInitialSemantic = func(ctx context.Context, socketPath string) (initialSemanticClient, error) {
	return semsearch.Dial(ctx, socketPath)
}

// RunInitialConversationIndex builds the first raw conversation cache during
// native installation. It reports the decision and the result on output so an
// operator can see whether the install performed indexing.
//
// A present cache means the daemon already completed an initial index. The
// daemon's normal background refresh remains responsible for later changes.
func RunInitialConversationIndex(ctx context.Context, output io.Writer, progress initialIndexProgress) error {
	if output == nil {
		output = io.Discard
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.WarnContext(ctx, "daemon.initial_index.config_load_failed", "concern", "conversation.index", "component", "daemon", "err", err)
		return fmt.Errorf("load config for initial conversation indexing: %w", err)
	}
	if !cfg.Conversation.Semantic.FeedsEngine() {
		_, _ = fmt.Fprintln(output, "Initial indexing: skipped because ingestion_enabled=false")
		return nil
	}
	if _, err := os.Stat(conversation.CachePath()); err == nil {
		_, _ = fmt.Fprintln(output, "Initial indexing: already complete")
		return nil
	} else if !os.IsNotExist(err) {
		slog.WarnContext(ctx, "daemon.initial_index.cache_check_failed", "concern", "conversation.index", "component", "daemon", "path", conversation.CachePath(), "err", err)
		return fmt.Errorf("check conversation index cache: %w", err)
	}

	_, _ = fmt.Fprintln(output, "Initial indexing: starting")
	index := conversation.NewIndex(newConversationRegistry(), cfg.Conversation)
	if err := index.Refresh(ctx); err != nil {
		_, _ = fmt.Fprintf(output, "Initial indexing: failed: %v\n", err)
		return fmt.Errorf("refresh initial conversation index: %w", err)
	}
	records, err := index.List(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(output, "Initial indexing: failed: %v\n", err)
		return fmt.Errorf("list initial conversation index: %w", err)
	}
	if progress != nil {
		progress(0, len(records))
		for completed := range records {
			progress(completed+1, len(records))
		}
	}
	semanticIndexed, err := runInitialSemanticIndex(ctx, cfg, index, output)
	if err != nil {
		return err
	}
	if !semanticIndexed {
		return nil
	}
	_, _ = fmt.Fprintln(output, "Initial indexing: complete")
	return nil
}

func runInitialSemanticIndex(
	ctx context.Context,
	cfg *config.Config,
	index conversationSemanticIndex,
	output io.Writer,
) (bool, error) {
	contentKinds, err := SemanticContentKinds(cfg.Conversation.Semantic)
	if err != nil {
		return false, err
	}
	client, err := dialInitialSemantic(ctx, cfg.Conversation.Semantic.SocketPath)
	if err != nil {
		_, _ = fmt.Fprintf(output, "Initial indexing: semantic indexing skipped because semantic service is unavailable: %v\n", err)
		return false, nil
	}
	defer func() { _ = client.Close() }()
	if err := client.Register(ctx, cfg.Conversation.Semantic.CollectionID); err != nil {
		_, _ = fmt.Fprintf(output, "Initial indexing: semantic indexing skipped because semantic service is unavailable: %v\n", err)
		return false, nil
	}
	worker := newConversationSemanticSyncWorker(
		index,
		func() conversationSemanticClient { return client },
		cfg.Conversation.Semantic.CollectionID,
		slog.Default(),
		contentKinds,
	)
	if err := worker.runPass(ctx); err != nil {
		_, _ = fmt.Fprintf(output, "Initial indexing: semantic indexing failed: %v\n", err)
		slog.WarnContext(ctx, "daemon.initial_index.semantic_pass_failed", "concern", "conversation.index", "component", "daemon", "err", err)
		return false, fmt.Errorf("run initial semantic indexing pass: %w", err)
	}
	return true, nil
}
