package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/livetrack"
)

// Registration retry cadence. A boot-time dial or register failure no longer
// disables semantic search for the process life: the runtime keeps retrying in
// the background with bounded exponential backoff so a later engine recovery
// makes search work again with no daemon reload.
const (
	semanticInitialRetryBackoff = time.Second
	semanticMaxRetryBackoff     = 30 * time.Second
	// semanticDialRegisterTimeout bounds each dial-and-register attempt so a
	// slow or wedged engine cannot stall boot or a retry tick indefinitely.
	semanticDialRegisterTimeout = 10 * time.Second
)

// conversationSemanticRuntime owns the daemon's engine-backed semantic search
// connection. It is created whenever semantic search is configured, even when
// the engine is unreachable at boot: registration is retried in the background
// until it succeeds, and the search read path resolves the current client
// through currentSearchClient so a later success needs no reload. The livetrack
// registry is attached once at construction; the grpc connection registers a
// session on it whenever a dial-and-register attempt succeeds, and the group
// drains that session on reload and shutdown. The retry worker itself is group
// owned too: it is cancelled and waited for by a PhaseWorkers before-hook, so it
// cannot outlive the drain that closes the registry.
type conversationSemanticRuntime struct {
	log       *slog.Logger
	connector semanticConnector
	registry  *livetrack.Registry[conversationSemanticConnectionMeta]
	meta      conversationSemanticConnectionMeta

	// initialRetryBackoff and maxRetryBackoff bound the registration retry
	// cadence. They are fields rather than direct constant reads so a test can
	// drive the real retry loop without waiting on the production cadence.
	initialRetryBackoff time.Duration
	maxRetryBackoff     time.Duration

	// attemptMu serializes registration attempts so the initial boot attempt
	// and the background retry loop never dial concurrently.
	attemptMu sync.Mutex

	// retryMu guards the retry worker's lifecycle handles below. retryCancel
	// stops the worker and retryDone closes once it has returned, so the
	// PhaseWorkers before-hook can wait the worker out before the connection
	// registry drains. Both stay nil until a retry worker starts.
	retryMu     sync.Mutex
	retryCancel context.CancelFunc
	retryDone   <-chan struct{}

	// mu guards the resolved-connection state below.
	mu         sync.Mutex
	search     conversationSemanticSearchClient
	feeder     conversationSemanticClient
	registered bool
}

// semanticConnection is one established, collection-registered engine
// connection: the search surface the control server queries, the feeder surface
// the background sync worker drives, the grpc connection livetrack owns for
// drain, and the closer that tears the connection down on a failed livetrack
// registration.
type semanticConnection struct {
	search     conversationSemanticSearchClient
	feeder     conversationSemanticClient
	connCloser io.Closer
	close      func() error
}

// semanticConnector dials the engine and registers the collection, returning a
// ready connection or an error. Production dials the lm-semantic-search daemon;
// tests inject a fake that fails and then succeeds.
type semanticConnector func(ctx context.Context) (semanticConnection, error)

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

// semsearchConnector builds the production connector: it dials the
// lm-semantic-search daemon and registers the conversation collection, and on
// success returns the concrete client as both the search and feeder surface.
// Each failing external call is logged here, at the boundary that made it, so
// the retry loop above only has to record the retry cadence.
func semsearchConnector(socketPath, collectionID string, log *slog.Logger) semanticConnector {
	return func(ctx context.Context) (semanticConnection, error) {
		client, err := semsearch.Dial(ctx, socketPath)
		if err != nil {
			log.WarnContext(ctx, "daemon.conversation_semantic.dial_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"socket_path", socketPath,
				"err", err,
			)
			return semanticConnection{search: nil, feeder: nil, connCloser: nil, close: nil}, fmt.Errorf("dial semantic search daemon: %w", err)
		}
		if registerErr := client.Register(ctx, collectionID); registerErr != nil {
			if closeErr := client.Close(); closeErr != nil {
				registerErr = errors.Join(registerErr, closeErr)
			}
			log.WarnContext(ctx, "daemon.conversation_semantic.collection_register_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"collection_id", collectionID,
				"err", registerErr,
			)
			return semanticConnection{search: nil, feeder: nil, connCloser: nil, close: nil}, fmt.Errorf("register semantic conversation collection: %w", registerErr)
		}
		return semanticConnection{
			search:     client,
			feeder:     client,
			connCloser: client.Conn(),
			close:      client.Close,
		}, nil
	}
}

func startConversationSemanticRuntime(ctx context.Context, cfg *config.Config, log *slog.Logger, group *livetrack.Group) *conversationSemanticRuntime {
	if cfg == nil || !cfg.Conversation.Semantic.Enabled {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	semanticCfg := cfg.Conversation.Semantic
	// The semantic-search grpc connection is internal daemon-to-daemon work, so
	// it attaches as a non-quiet-relevant PhaseWorkers member: the group drains
	// it on reload and shutdown, but it never holds the quiet-wait. The registry
	// is attached once here and holds a session only once a dial-and-register
	// attempt succeeds.
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
	runtime := newConversationSemanticRuntime(log, semsearchConnector(semanticCfg.SocketPath, semanticCfg.CollectionID, log), registry, meta)
	if err := runtime.attemptRegister(ctx); err != nil {
		// A boot-time failure is transient (the engine is often briefly down
		// during a co-restart), so keep the runtime alive and retry in the
		// background rather than disabling search for the process life.
		log.WarnContext(ctx, "daemon.conversation_semantic.initial_register_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", semanticCfg.CollectionID,
			"retry_backoff_ms", semanticInitialRetryBackoff.Milliseconds(),
			"err", err,
		)
		runtime.startRetryWorker(ctx, group)
	} else {
		log.InfoContext(ctx, "daemon.conversation_semantic.started",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", semanticCfg.CollectionID,
		)
	}
	return runtime
}

// newConversationSemanticRuntime builds the runtime around one connector and
// one already-attached livetrack registry, with no connection established yet.
// Registration is the caller's next step; until it succeeds the runtime resolves
// a nil search client, which the query path fails fast on.
func newConversationSemanticRuntime(
	log *slog.Logger,
	connector semanticConnector,
	registry *livetrack.Registry[conversationSemanticConnectionMeta],
	meta conversationSemanticConnectionMeta,
) *conversationSemanticRuntime {
	return &conversationSemanticRuntime{
		log:                 log,
		connector:           connector,
		registry:            registry,
		meta:                meta,
		initialRetryBackoff: semanticInitialRetryBackoff,
		maxRetryBackoff:     semanticMaxRetryBackoff,
		attemptMu:           sync.Mutex{},
		retryMu:             sync.Mutex{},
		retryCancel:         nil,
		retryDone:           nil,
		mu:                  sync.Mutex{},
		search:              nil,
		feeder:              nil,
		registered:          false,
	}
}

// semanticRetryWorkerHookName names the lifecycle hook that stops the
// registration retry worker. It runs in PhaseWorkers, before the phase's
// members drain, so the worker is gone before the semantic connection registry
// closes.
const semanticRetryWorkerHookName = "conversation.semantic.retry_stop"

// installRetryStop creates the retry worker's context and completion channel and
// registers stopRetryWorker as a PhaseWorkers before-hook on the lifecycle group.
// Neither handle exists until the hook is installed, so the worker goroutine
// cannot be launched before the group owns its stop, and a drain that begins
// afterwards cancels and joins the worker inside the workers phase, before the
// semantic connection registry (a PhaseWorkers member) drains. A nil group has no
// owner, so nothing is created and the caller must start nothing.
func (r *conversationSemanticRuntime) installRetryStop(ctx context.Context, group *livetrack.Group) (context.Context, chan struct{}, bool) {
	if group == nil {
		return nil, nil, false
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.retryMu.Lock()
	r.retryCancel = cancel
	r.retryDone = done
	r.retryMu.Unlock()
	group.AddHookBefore(livetrack.PhaseWorkers, semanticRetryWorkerHookName, r.stopRetryWorker)
	return workerCtx, done, true
}

// startRetryWorker launches the background registration retry loop as a
// group-owned worker rather than a bare goroutine, and reports whether it
// started. The worker runs under a context the group's stop hook cancels, so
// reload and shutdown cancel the worker and wait for it to return before the
// semantic connection registry drains, and no retry can dial or register across
// that drain boundary. The daemon starts it before the control server accepts
// reload or rebind requests, so no drain can begin between the hook install and
// the goroutine launch.
func (r *conversationSemanticRuntime) startRetryWorker(ctx context.Context, group *livetrack.Group) bool {
	workerCtx, done, owned := r.installRetryStop(ctx, group)
	if !owned {
		r.log.WarnContext(ctx, "daemon.conversation_semantic.retry_start_skipped_unowned",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", r.meta.CollectionID,
		)
		return false
	}
	go func() {
		defer close(done)
		defer r.cancelRetryWorker()
		defer func() {
			if recovered := recover(); recovered != nil {
				r.log.ErrorContext(workerCtx, "daemon.conversation_semantic.retry_panic",
					"concern", "conversation.semantic",
					"component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		r.retryRegisterLoop(workerCtx)
	}()
	return true
}

// cancelRetryWorker cancels the retry worker's context. The worker calls it on
// return so a loop that ends on a successful registration releases its derived
// context, and stopRetryWorker calls it to stop a running worker. It is nil-safe
// and idempotent.
func (r *conversationSemanticRuntime) cancelRetryWorker() {
	r.retryMu.Lock()
	cancel := r.retryCancel
	r.retryMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// stopRetryWorker cancels the registration retry worker and waits for it to
// return, bounded by the drain context. It is the PhaseWorkers before-hook, so
// the wait completes before the connection registry drains. It is nil-safe,
// idempotent, and a no-op when no retry worker was ever started (the boot
// registration succeeded).
func (r *conversationSemanticRuntime) stopRetryWorker(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.retryMu.Lock()
	done := r.retryDone
	r.retryMu.Unlock()
	if done == nil {
		return nil
	}
	r.cancelRetryWorker()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		r.log.WarnContext(ctx, "daemon.conversation_semantic.retry_stop_timeout",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", r.meta.CollectionID,
			"err", ctx.Err(),
		)
		return fmt.Errorf("wait for semantic registration retry worker: %w", ctx.Err())
	}
}

// attemptRegister dials the engine, registers the collection, and registers the
// grpc connection with livetrack, publishing the resolved client on success. It
// is idempotent: once registered, later calls return nil without dialing. The
// attempt is bounded by semanticDialRegisterTimeout so a wedged engine cannot
// block the caller. The connector logs the failing external call it made, so
// this function only adds context for the livetrack registration step.
func (r *conversationSemanticRuntime) attemptRegister(ctx context.Context) error {
	r.attemptMu.Lock()
	defer r.attemptMu.Unlock()

	r.mu.Lock()
	alreadyRegistered := r.registered
	r.mu.Unlock()
	if alreadyRegistered {
		return nil
	}

	attemptCtx, cancel := context.WithTimeout(ctx, semanticDialRegisterTimeout)
	defer cancel()
	connection, err := r.connector(attemptCtx)
	if err != nil {
		return err
	}
	_, registerErr := r.registry.Register(ctx, "conversation.semantic.grpc", r.meta, &conversationSemanticConnectionCloser{
		closer: connection.connCloser,
		log:    r.log,
	})
	if registerErr != nil {
		if connection.close != nil {
			if closeErr := connection.close(); closeErr != nil {
				r.log.WarnContext(ctx, "daemon.conversation_semantic.abandoned_connection_close_failed",
					"concern", "conversation.semantic",
					"component", "daemon",
					"err", closeErr,
				)
			}
		}
		return fmt.Errorf("register semantic search connection with livetrack: %w", registerErr)
	}
	r.mu.Lock()
	r.search = connection.search
	r.feeder = connection.feeder
	r.registered = true
	r.mu.Unlock()
	return nil
}

// retryRegisterLoop retries registration with bounded exponential backoff until
// it succeeds, the registry closes (reload or shutdown drain), or the context is
// done. A later success makes search work with no manual reload, and is logged
// once here rather than per attempt.
func (r *conversationSemanticRuntime) retryRegisterLoop(ctx context.Context) {
	attempts, registered := r.retryRegisterUntilReady(ctx)
	if !registered {
		return
	}
	r.log.InfoContext(ctx, "daemon.conversation_semantic.register_recovered",
		"concern", "conversation.semantic",
		"component", "daemon",
		"collection_id", r.meta.CollectionID,
		"attempt", attempts,
	)
}

// retryRegisterUntilReady runs the backoff loop and reports the attempt count
// and whether registration succeeded. It logs each failed retry at Debug and
// leaves the success event to its caller, keeping the loop free of Info events.
func (r *conversationSemanticRuntime) retryRegisterUntilReady(ctx context.Context) (int, bool) {
	backoff := r.initialRetryBackoff
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return attempts, false
		case <-time.After(backoff):
		}
		attempts++
		err := r.attemptRegister(ctx)
		if err == nil {
			return attempts, true
		}
		if errors.Is(err, livetrack.ErrRegistryClosed) {
			r.log.DebugContext(ctx, "daemon.conversation_semantic.retry_stopped_registry_closed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"attempt", attempts,
			)
			return attempts, false
		}
		r.log.DebugContext(ctx, "daemon.conversation_semantic.register_retry_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"attempt", attempts,
			"backoff_ms", backoff.Milliseconds(),
			"err", err,
		)
		backoff = min(backoff*2, r.maxRetryBackoff)
	}
}

// currentSearchClient returns the engine-backed search client when registration
// has succeeded, or nil while the engine is still unreachable. The control
// server resolves it per query so a background registration success is picked up
// with no reload.
func (r *conversationSemanticRuntime) currentSearchClient() conversationSemanticSearchClient {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.registered {
		return nil
	}
	return r.search
}

// syncClient returns the engine feeder surface for the background sync worker
// once registration has succeeded, or nil while the engine is unreachable.
func (r *conversationSemanticRuntime) syncClient() conversationSemanticClient {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.registered {
		return nil
	}
	return r.feeder
}
