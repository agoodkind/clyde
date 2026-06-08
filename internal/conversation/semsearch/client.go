// Package semsearch wraps the lm-semantic-search daemon client for conversation
// sync.
package semsearch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	lmclient "goodkind.io/lm-semantic-search/client"
	lmsemanticsearchv1 "goodkind.io/lm-semantic-search/gen/go/lmsemanticsearch/v1"
	"google.golang.org/grpc"
)

// SemDoc is the conversation-message projection sent to lm-semantic-search.
type SemDoc struct {
	ConversationID string
	MessageIndex   int32
	Role           string
	TimestampUnix  int64
	Text           string
}

// SemHit is one conversation-message match returned by the engine's
// cross-conversation search.
type SemHit struct {
	ConversationID string
	MessageIndex   int32
	Role           string
	TimestampUnix  int64
	Content        string
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
			ConversationID: result.GetConversationId(),
			MessageIndex:   result.GetMessageIndex(),
			Role:           result.GetRole(),
			TimestampUnix:  result.GetTimestampUnix(),
			Content:        result.GetContent(),
		})
	}
	return out
}

func conversationDocuments(docs []SemDoc) []*lmsemanticsearchv1.ConversationDocument {
	out := make([]*lmsemanticsearchv1.ConversationDocument, 0, len(docs))
	for _, doc := range docs {
		out = append(out, &lmsemanticsearchv1.ConversationDocument{
			ConversationId: doc.ConversationID,
			MessageIndex:   doc.MessageIndex,
			Role:           doc.Role,
			TimestampUnix:  doc.TimestampUnix,
			Text:           doc.Text,
		})
	}
	return out
}
