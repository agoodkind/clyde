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
	session  *livetrack.Session[conversationSemanticConnectionMeta]
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

func startConversationSemanticRuntime(ctx context.Context, cfg *config.Config, log *slog.Logger, group *livetrack.Group) *conversationSemanticRuntime {
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
	// The semantic-search grpc connection is internal daemon-to-daemon work, so
	// it attaches as a non-quiet-relevant PhaseWorkers member: the group drains
	// it on reload and shutdown, but it never holds the quiet-wait.
	registry := livetrack.Attach[conversationSemanticConnectionMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseWorkers,
		QuietRelevant: false,
		CancelNoWait:  false,
	}, livetrack.Options[conversationSemanticConnectionMeta]{
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
	session, registerErr := registry.Register(ctx, "conversation.semantic.grpc", meta, &conversationSemanticConnectionCloser{
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
	runtime := &conversationSemanticRuntime{client: client, registry: registry, session: session}
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

// close tears the semantic runtime down outside the group lifecycle. The group
// drains the connection registry on reload and shutdown; close is the explicit
// path used on the startup error branch and when the in-process config apply
// restarts the runtime. It releases the livetrack session (so a later group
// drain skips it) and closes the grpc client connection directly.
func (r *conversationSemanticRuntime) close(ctx context.Context, log *slog.Logger, reason string) {
	if r == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if r.session != nil && r.registry != nil {
		r.registry.Release(ctx, r.session, reason)
		r.session = nil
	}
	r.registry = nil
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
