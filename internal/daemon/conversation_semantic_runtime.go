package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/livetrack"
)

type conversationSemanticRuntime struct {
	client   *semsearch.Client
	registry *livetrack.Registry[conversationSemanticConnectionMeta]
}

type conversationSemanticConnectionMeta struct {
	CollectionID string
	SocketPath   string
}

var _ livetrack.Meta = conversationSemanticConnectionMeta{
	CollectionID: "",
	SocketPath:   "",
}

// IsLivetrackMeta satisfies the livetrack.Meta constraint.
func (conversationSemanticConnectionMeta) IsLivetrackMeta() {}

type conversationSemanticConnectionCloser struct {
	closer io.Closer
	log    *slog.Logger
}

// Close closes the semantic-search daemon connection during livetrack drain.
func (c *conversationSemanticConnectionCloser) Close(reason string) error {
	if c == nil || c.closer == nil {
		return nil
	}
	if err := c.closer.Close(); err != nil {
		if c.log != nil {
			c.log.Warn("daemon.conversation_semantic.grpc_close_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"reason", reason,
				"err", err,
			)
		}
		return fmt.Errorf("close semantic search grpc connection: %w", err)
	}
	return nil
}

func startConversationSemanticRuntime(ctx context.Context, cfg *config.Config, log *slog.Logger) *conversationSemanticRuntime {
	if cfg == nil || !cfg.Conversation.Semantic.Enabled {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	semanticCfg := cfg.Conversation.Semantic
	client, err := semsearch.Dial(ctx, semanticCfg.SocketPath)
	if err != nil {
		log.WarnContext(ctx, "daemon.conversation_semantic.dial_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"socket_path", semanticCfg.SocketPath,
			"err", err,
		)
		return nil
	}
	registry := livetrack.New[conversationSemanticConnectionMeta](livetrack.Options[conversationSemanticConnectionMeta]{
		Component:     "daemon",
		Concern:       "conversation.semantic",
		Log:           log,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: false,
		Now:           nil,
	})
	meta := conversationSemanticConnectionMeta{
		CollectionID: semanticCfg.CollectionID,
		SocketPath:   semanticCfg.SocketPath,
	}
	meta.IsLivetrackMeta()
	_, registerErr := registry.Register(ctx, "conversation.semantic.grpc", meta, &conversationSemanticConnectionCloser{
		closer: client.Conn(),
		log:    log,
	})
	if registerErr != nil {
		if closeErr := client.Close(); closeErr != nil {
			registerErr = errors.Join(registerErr, closeErr)
		}
		log.WarnContext(ctx, "daemon.conversation_semantic.livetrack_register_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", registerErr,
		)
		return nil
	}
	runtime := &conversationSemanticRuntime{client: client, registry: registry}
	if err := client.Register(ctx, semanticCfg.CollectionID); err != nil {
		runtime.close(ctx, log, "register_failed")
		log.WarnContext(ctx, "daemon.conversation_semantic.collection_register_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", semanticCfg.CollectionID,
			"err", err,
		)
		return nil
	}
	log.InfoContext(ctx, "daemon.conversation_semantic.started",
		"concern", "conversation.semantic",
		"component", "daemon",
		"collection_id", semanticCfg.CollectionID,
	)
	return runtime
}

func (r *conversationSemanticRuntime) close(ctx context.Context, log *slog.Logger, reason string) {
	if r == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if r.registry != nil {
		result := r.registry.Drain(ctx, reason)
		if len(result.Errors) > 0 {
			log.WarnContext(ctx, "daemon.conversation_semantic.close_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"reason", reason,
				"err", errors.Join(result.Errors...),
			)
		}
		r.registry = nil
		r.client = nil
		return
	}
	if r.client != nil {
		if err := r.client.Close(); err != nil {
			log.WarnContext(ctx, "daemon.conversation_semantic.close_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"reason", reason,
				"err", fmt.Errorf("close semantic runtime: %w", err),
			)
		}
		r.client = nil
	}
}

func (r *runtimeServices) closeConversationSemanticRuntime(ctx context.Context, log *slog.Logger, reason string) {
	if r == nil || r.semantic == nil {
		return
	}
	r.semantic.close(ctx, log, reason)
	r.semantic = nil
}
