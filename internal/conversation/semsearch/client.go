// Package semsearch wraps the lm-semantic-search daemon client for conversation
// sync.
package semsearch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	lmclient "goodkind.io/lm-semantic-search/client"
	lmsemanticsearchv1 "goodkind.io/lm-semantic-search/gen/go/lmsemanticsearch/v1"
	"google.golang.org/grpc"
)

// Engine job-state strings reported by the lm-semantic-search daemon GetJob RPC.
// These mirror the engine's internal JobState vocabulary (queued, running,
// cancelling, completed, failed, cancelled); the three terminal states below are
// the only values that end a wait.
const (
	// JobStateCompleted is the engine's terminal success state.
	JobStateCompleted = "completed"
	// JobStateFailed is the engine's terminal failure state.
	JobStateFailed = "failed"
	// JobStateCancelled is the engine's terminal cancellation state.
	JobStateCancelled = "cancelled"
)

// SemDoc is the conversation-message projection sent to lm-semantic-search.
type SemDoc struct {
	ConversationID string
	// ParentConversationID is the derived conversation id of this conversation's
	// lineage parent, or "" when the conversation has no resolvable parent. It is
	// the same for every message of one conversation so forks group with parents
	// in the index.
	ParentConversationID string
	MessageIndex         int32
	Role                 string
	TimestampUnix        int64
	Text                 string
}

// SemHit is one conversation-message match returned by the engine's
// cross-conversation search.
type SemHit struct {
	ConversationID string
	// ParentConversationID is the derived conversation id of the matched
	// conversation's lineage parent, or "" when it has no resolvable parent.
	ParentConversationID string
	MessageIndex         int32
	Role                 string
	TimestampUnix        int64
	Content              string
}

// Client wraps the lm-semantic-search daemon gRPC client.
type Client struct {
	conn   *grpc.ClientConn
	daemon lmsemanticsearchv1.SemanticSearchDaemonServiceClient
}

// Dial opens a gRPC connection to the lm-semantic-search daemon.
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	resolvedSocketPath := strings.TrimSpace(socketPath)
	if resolvedSocketPath == "" {
		resolvedSocketPath = lmclient.ResolveSocketPath()
	}
	if resolvedSocketPath == "" {
		return nil, fmt.Errorf("resolve semantic search daemon socket path")
	}
	conn, daemonClient, err := lmclient.DialDaemon(ctx, resolvedSocketPath)
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.dial_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", err,
		)
		return nil, fmt.Errorf("dial semantic search daemon: %w", err)
	}
	return &Client{conn: conn, daemon: daemonClient}, nil
}

// Conn returns the underlying daemon gRPC connection for livetrack ownership.
func (c *Client) Conn() *grpc.ClientConn {
	if c == nil {
		return nil
	}
	return c.conn
}

// Register ensures the conversation collection exists in the engine.
func (c *Client) Register(ctx context.Context, collectionID string) error {
	if c == nil || c.daemon == nil {
		return fmt.Errorf("register semantic conversation collection: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return fmt.Errorf("register semantic conversation collection: collection id is empty")
	}
	_, err := c.daemon.RegisterConversationCollection(ctx, &lmsemanticsearchv1.RegisterConversationCollectionRequest{
		CollectionId: trimmedCollectionID,
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.register_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"err", err,
		)
		return fmt.Errorf("register semantic conversation collection %q: %w", trimmedCollectionID, err)
	}
	return nil
}

// UpsertConversationDocuments starts an async engine job for conversation docs.
func (c *Client) UpsertConversationDocuments(ctx context.Context, collectionID string, docs []SemDoc) (string, error) {
	if c == nil || c.daemon == nil {
		return "", fmt.Errorf("upsert semantic conversation documents: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return "", fmt.Errorf("upsert semantic conversation documents: collection id is empty")
	}
	response, err := c.daemon.UpsertConversationDocuments(ctx, &lmsemanticsearchv1.UpsertConversationDocumentsRequest{
		CollectionId: trimmedCollectionID,
		Documents:    conversationDocuments(docs),
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.upsert_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"documents", len(docs),
			"err", err,
		)
		return "", fmt.Errorf("upsert semantic conversation documents for collection %q: %w", trimmedCollectionID, err)
	}
	return response.GetJobId(), nil
}

// DeleteConversation starts an async engine job that removes one conversation.
func (c *Client) DeleteConversation(ctx context.Context, collectionID, conversationID string) (string, error) {
	if c == nil || c.daemon == nil {
		return "", fmt.Errorf("delete semantic conversation: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return "", fmt.Errorf("delete semantic conversation: collection id is empty")
	}
	trimmedConversationID := strings.TrimSpace(conversationID)
	if trimmedConversationID == "" {
		return "", fmt.Errorf("delete semantic conversation: conversation id is empty")
	}
	response, err := c.daemon.DeleteConversation(ctx, &lmsemanticsearchv1.DeleteConversationRequest{
		CollectionId:   trimmedCollectionID,
		ConversationId: trimmedConversationID,
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.delete_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"conversation_id", trimmedConversationID,
			"err", err,
		)
		return "", fmt.Errorf("delete semantic conversation %q from collection %q: %w", trimmedConversationID, trimmedCollectionID, err)
	}
	return response.GetJobId(), nil
}

// JobState fetches the current lifecycle state string for one engine job via the
// GetJob RPC. It returns the empty string when the job is missing from the
// response and wraps transport failures with the job id.
func (c *Client) JobState(ctx context.Context, jobID string) (string, error) {
	if c == nil || c.daemon == nil {
		return "", fmt.Errorf("get semantic job state: client is nil")
	}
	trimmedJobID := strings.TrimSpace(jobID)
	if trimmedJobID == "" {
		return "", fmt.Errorf("get semantic job state: job id is empty")
	}
	response, err := c.daemon.GetJob(ctx, &lmsemanticsearchv1.GetJobRequest{
		JobId: trimmedJobID,
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.get_job_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"job_id", trimmedJobID,
			"err", err,
		)
		return "", fmt.Errorf("get semantic job %q: %w", trimmedJobID, err)
	}
	return response.GetJob().GetState(), nil
}

// WaitForJob polls JobState on the given interval until the engine job reaches a
// terminal state, the timeout elapses, or the context is done. It returns the
// last observed state in every case; a timeout or cancellation also returns a
// non-nil error wrapped with the job id and last state.
func (c *Client) WaitForJob(ctx context.Context, jobID string, pollInterval, timeout time.Duration) (string, error) {
	trimmedJobID := strings.TrimSpace(jobID)
	if trimmedJobID == "" {
		return "", fmt.Errorf("wait for semantic job: job id is empty")
	}
	effectiveInterval := pollInterval
	if effectiveInterval <= 0 {
		effectiveInterval = time.Second
	}
	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(effectiveInterval)
	defer ticker.Stop()
	lastState := ""
	for {
		state, err := c.JobState(waitCtx, trimmedJobID)
		if err != nil {
			return lastState, fmt.Errorf("poll semantic job %q in state %q: %w", trimmedJobID, lastState, err)
		}
		lastState = state
		if isTerminalJobState(state) {
			return state, nil
		}
		select {
		case <-waitCtx.Done():
			slog.WarnContext(ctx, "conversation.semsearch.wait_job_unfinished",
				"concern", "conversation.semantic",
				"component", "conversation",
				"job_id", trimmedJobID,
				"state", lastState,
				"err", waitCtx.Err(),
			)
			return lastState, fmt.Errorf("wait for semantic job %q ended in state %q: %w", trimmedJobID, lastState, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func isTerminalJobState(state string) bool {
	switch state {
	case JobStateCompleted, JobStateFailed, JobStateCancelled:
		return true
	default:
		return false
	}
}

// SearchConversations runs an engine-backed cross-conversation search over the
// named collection and returns the bounded message hits.
func (c *Client) SearchConversations(ctx context.Context, collectionID, query string, limit int32) ([]SemHit, error) {
	if c == nil || c.daemon == nil {
		return nil, fmt.Errorf("search semantic conversations: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return nil, fmt.Errorf("search semantic conversations: collection id is empty")
	}
	response, err := c.daemon.SearchConversations(ctx, &lmsemanticsearchv1.SearchConversationsRequest{
		CollectionId: trimmedCollectionID,
		Query:        query,
		Limit:        limit,
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.search_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"err", err,
		)
		return nil, fmt.Errorf("search semantic conversations in collection %q: %w", trimmedCollectionID, err)
	}
	return conversationSearchHits(response.GetResults()), nil
}

// Close closes the underlying daemon gRPC connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	if err := c.conn.Close(); err != nil {
		slog.Warn("conversation.semsearch.close_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", err,
		)
		return fmt.Errorf("close semantic search daemon connection: %w", err)
	}
	return nil
}

func conversationSearchHits(results []*lmsemanticsearchv1.ConversationSearchResult) []SemHit {
	out := make([]SemHit, 0, len(results))
	for _, result := range results {
		out = append(out, SemHit{
			ConversationID:       result.GetConversationId(),
			ParentConversationID: result.GetParentConversationId(),
			MessageIndex:         result.GetMessageIndex(),
			Role:                 result.GetRole(),
			TimestampUnix:        result.GetTimestampUnix(),
			Content:              result.GetContent(),
		})
	}
	return out
}

func conversationDocuments(docs []SemDoc) []*lmsemanticsearchv1.ConversationDocument {
	out := make([]*lmsemanticsearchv1.ConversationDocument, 0, len(docs))
	for _, doc := range docs {
		out = append(out, &lmsemanticsearchv1.ConversationDocument{
			ConversationId:       doc.ConversationID,
			ParentConversationId: doc.ParentConversationID,
			MessageIndex:         doc.MessageIndex,
			Role:                 doc.Role,
			TimestampUnix:        doc.TimestampUnix,
			Text:                 doc.Text,
		})
	}
	return out
}
