// Package semsearch wraps the lm-semantic-search daemon client for conversation
// sync.
package semsearch

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"
	"unicode/utf8"

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
	Tools                []SemToolCall
	Thinking             string
	// WorkspaceRoot is the conversation's workspace, sent so the engine stores it
	// as a filterable scalar column. The same for every message of one
	// conversation; empty when unknown.
	WorkspaceRoot string
	// Archived is the conversation's archived status, sent so the engine stores
	// it as a filterable scalar column. The same for every message of one
	// conversation.
	Archived bool
}

// SemToolCall is one structured tool call attached to a semantic document.
type SemToolCall struct {
	Name      string
	InputJSON string
	Command   string
	LangHint  string
	Output    string
	IsError   bool
}

// Fingerprint pairs a conversation id with a content fingerprint that changes
// whenever the conversation's messages change. clyde sends the full set each
// pass so the engine can diff against its checkpoint and reply with the ids it
// needs.
type Fingerprint struct {
	ConversationID string
	Value          string
}

// BackfillScalarEntry is one conversation's clyde-sourced enrichment for the
// scalar backfill: the workspace root and archived status clyde observed,
// keyed by conversation id. The engine writes these onto rows whose
// workspaceRoot is empty, preserving each row's vector.
type BackfillScalarEntry struct {
	ConversationID string
	WorkspaceRoot  string
	Archived       bool
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
	// Score is the engine's retrieval relevance: vector similarity from a
	// semantic search, or the keyword rank from the engine's literal fallback.
	Score float64
}

// SearchFilter narrows conversation retrieval by row attributes. Every field
// is optional; the zero value matches everything. Providers filter natively on
// the engine's provider column. WorkspaceRoots maps to the engine's native
// workspace column but is unused for the workspace filter today: workspace_root
// is null on rows indexed before that column existed, so clyde instead resolves
// a workspace prefix to the matching ConversationIDs, which every row carries.
// ConversationIDs scopes to explicit conversations (a positional conversation
// id or a within-search) and to that resolved workspace set.
type SearchFilter struct {
	Providers            []string
	WorkspaceRoots       []string
	Roles                []string
	FromUnix             int64
	UntilUnix            int64
	ConversationIDs      []string
	ParentConversationID string
	MinScore             float64
	MessageIndexFrom     int32
	MessageIndexUntil    int32
}

// wire converts the filter to its proto shape, nil when nothing is set so the
// request stays minimal.
func (filter SearchFilter) wire() *lmsemanticsearchv1.ConversationSearchFilter {
	if len(filter.Providers) == 0 && len(filter.WorkspaceRoots) == 0 &&
		len(filter.Roles) == 0 && filter.FromUnix == 0 && filter.UntilUnix == 0 &&
		len(filter.ConversationIDs) == 0 && filter.ParentConversationID == "" &&
		filter.MinScore == 0 && filter.MessageIndexFrom == 0 && filter.MessageIndexUntil == 0 {
		return nil
	}
	return &lmsemanticsearchv1.ConversationSearchFilter{
		Providers:            filter.Providers,
		WorkspaceRoots:       filter.WorkspaceRoots,
		Roles:                filter.Roles,
		FromUnix:             filter.FromUnix,
		UntilUnix:            filter.UntilUnix,
		ConversationIds:      filter.ConversationIDs,
		ParentConversationId: filter.ParentConversationID,
		MinScore:             filter.MinScore,
		MessageIndexFrom:     filter.MessageIndexFrom,
		MessageIndexUntil:    filter.MessageIndexUntil,
	}
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

// SyncConversationManifest sends the full conversation manifest and returns the
// conversation ids the engine needs: those new or changed since its last ingest.
// The engine owns drift, so clyde keeps no change-tracking state of its own.
func (c *Client) SyncConversationManifest(ctx context.Context, collectionID string, manifest []Fingerprint) ([]string, error) {
	if c == nil || c.daemon == nil {
		return nil, fmt.Errorf("sync semantic conversation manifest: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return nil, fmt.Errorf("sync semantic conversation manifest: collection id is empty")
	}
	response, err := c.daemon.SyncConversationManifest(ctx, &lmsemanticsearchv1.SyncConversationManifestRequest{
		CollectionId: trimmedCollectionID,
		Manifest:     conversationFingerprints(manifest),
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.sync_manifest_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"manifest", len(manifest),
			"err", err,
		)
		return nil, fmt.Errorf("sync semantic conversation manifest for collection %q: %w", trimmedCollectionID, err)
	}
	return response.GetNeededConversationIds(), nil
}

// upsertStreamMaxDocsPerChunk caps the documents in one stream chunk by count, so
// a pass with many small messages still frames into bounded chunks.
const upsertStreamMaxDocsPerChunk = 1000

// upsertStreamMaxBytesPerChunk caps the documents in one stream chunk by
// approximate payload bytes, so a pass with a few large transcripts still frames
// into chunks under the default gRPC max message size. The manifest ships as
// its own chunk and must fit within a single message.
const upsertStreamMaxBytesPerChunk = 3 << 20

// UpsertConversationDocuments starts an async engine job for the changed
// conversations' documents. manifest is the full current conversation set with
// fingerprints, so the engine skips unchanged conversations; documents cover
// only the conversations the engine asked for. The header declares RETAIN, so a
// conversation absent from the manifest is kept rather than deleted. It opens the
// client stream, sends the header, then the documents in bounded chunks so the
// document set is not capped by the gRPC max message size, then the manifest as
// one chunk, which must fit within a single message.
func (c *Client) UpsertConversationDocuments(ctx context.Context, collectionID string, docs []SemDoc, manifest []Fingerprint) (string, error) {
	return c.upsertConversationDocuments(ctx, collectionID, docs, manifest, false)
}

// ReexamineConversationDocuments upserts documents like UpsertConversationDocuments,
// but asks the engine to re-examine every delivered conversation even when its
// fingerprint is unchanged. It is the wire path for the operator-run backfill that
// corrects conversations indexed before a new indexing capability shipped: the
// engine reuses existing vectors and embeds only genuinely-new chunks, so this is
// not a force-reindex. The normal daemon sync never sets it.
func (c *Client) ReexamineConversationDocuments(ctx context.Context, collectionID string, docs []SemDoc, manifest []Fingerprint) (string, error) {
	return c.upsertConversationDocuments(ctx, collectionID, docs, manifest, true)
}

func (c *Client) upsertConversationDocuments(ctx context.Context, collectionID string, docs []SemDoc, manifest []Fingerprint, reexamine bool) (string, error) {
	if c == nil || c.daemon == nil {
		return "", fmt.Errorf("upsert semantic conversation documents: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return "", fmt.Errorf("upsert semantic conversation documents: collection id is empty")
	}
	stream, err := c.daemon.UpsertConversationDocumentsStream(ctx)
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.upsert_stream_open_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"documents", len(docs),
			"err", err,
		)
		return "", fmt.Errorf("open semantic conversation upsert stream for collection %q: %w", trimmedCollectionID, err)
	}
	if sendErr := sendUpsertStream(ctx, stream, trimmedCollectionID, docs, manifest, reexamine); sendErr != nil {
		return "", fmt.Errorf("send semantic conversation upsert stream for collection %q: %w", trimmedCollectionID, sendErr)
	}
	response, err := stream.CloseAndRecv()
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

// upsertClientInfo identifies clyde to the engine so an upsert job is
// attributable to the calling process. caller_cwd is left empty because it only
// resolves relative request paths, which a conversation upsert never carries.
func upsertClientInfo() *lmsemanticsearchv1.ClientInfo {
	pid := os.Getpid()
	var clientPID int32
	if pid >= 0 && pid <= math.MaxInt32 {
		clientPID = int32(pid)
	}
	return &lmsemanticsearchv1.ClientInfo{
		Name:      "clyde",
		Pid:       clientPID,
		CallerCwd: "",
	}
}

// sendUpsertStream sends the header, then the documents in bounded chunks, then
// the manifest as one chunk. The header's reconcile mode governs a conversation
// the manifest omits (clyde sends RETAIN, so it is kept); the manifest is sent
// whole rather than split.
func sendUpsertStream(
	ctx context.Context,
	stream grpc.ClientStreamingClient[lmsemanticsearchv1.UpsertConversationDocumentsChunk, lmsemanticsearchv1.UpsertConversationDocumentsResponse],
	collectionID string,
	docs []SemDoc,
	manifest []Fingerprint,
	reexamine bool,
) error {
	header := &lmsemanticsearchv1.UpsertConversationDocumentsChunk{
		Chunk: &lmsemanticsearchv1.UpsertConversationDocumentsChunk_Header{
			Header: &lmsemanticsearchv1.UpsertConversationDocumentsHeader{
				CollectionId: collectionID,
				Client:       upsertClientInfo(),
				// clyde declares RETAIN explicitly: a conversation absent from the
				// manifest is kept, never deleted. Only an explicit delete removes one,
				// so a transient short manifest cannot drop conversations from the index.
				ReconcileMode: lmsemanticsearchv1.ConversationReconcileMode_CONVERSATION_RECONCILE_MODE_RETAIN,
				// reexamine is set only by the operator-run backfill, so the engine
				// re-examines delivered conversations whose fingerprint is unchanged.
				// The normal sync leaves it false.
				ReexamineDelivered: reexamine,
			},
		},
	}
	if err := stream.Send(header); err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.upsert_stream_header_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", collectionID,
			"err", err,
		)
		return fmt.Errorf("send upsert header: %w", err)
	}
	if err := sendUpsertDocumentChunks(ctx, stream, docs); err != nil {
		return err
	}
	manifestChunk := &lmsemanticsearchv1.UpsertConversationDocumentsChunk{
		Chunk: &lmsemanticsearchv1.UpsertConversationDocumentsChunk_Manifest{
			Manifest: &lmsemanticsearchv1.UpsertConversationDocumentsManifest{
				Manifest: conversationFingerprints(manifest),
			},
		},
	}
	if err := stream.Send(manifestChunk); err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.upsert_stream_manifest_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", collectionID,
			"manifest", len(manifest),
			"err", err,
		)
		return fmt.Errorf("send upsert manifest: %w", err)
	}
	return nil
}

// sendUpsertDocumentChunks frames docs into chunks bounded by document count and
// approximate payload bytes, sending each as a documents chunk. An empty docs
// slice sends no documents chunk, which the engine reads as an empty document
// set still governed by the manifest.
func sendUpsertDocumentChunks(
	ctx context.Context,
	stream grpc.ClientStreamingClient[lmsemanticsearchv1.UpsertConversationDocumentsChunk, lmsemanticsearchv1.UpsertConversationDocumentsResponse],
	docs []SemDoc,
) error {
	batch := make([]SemDoc, 0, upsertStreamMaxDocsPerChunk)
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		chunk := &lmsemanticsearchv1.UpsertConversationDocumentsChunk{
			Chunk: &lmsemanticsearchv1.UpsertConversationDocumentsChunk_Documents{
				Documents: &lmsemanticsearchv1.UpsertConversationDocumentsDocuments{
					Documents: conversationDocuments(batch),
				},
			},
		}
		if err := stream.Send(chunk); err != nil {
			slog.WarnContext(ctx, "conversation.semsearch.upsert_stream_documents_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"documents", len(batch),
				"err", err,
			)
			return fmt.Errorf("send upsert documents chunk: %w", err)
		}
		batch = batch[:0]
		batchBytes = 0
		return nil
	}
	for _, doc := range docs {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "conversation.semsearch.upsert_stream_context_done",
				"concern", "conversation.semantic",
				"component", "conversation",
				"err", ctx.Err(),
			)
			return fmt.Errorf("send upsert documents chunk: %w", ctx.Err())
		}
		boundedDoc := truncateSemDocForUpsert(doc)
		docBytes := semDocByteSize(boundedDoc)
		if len(batch) > 0 && (len(batch) >= upsertStreamMaxDocsPerChunk || batchBytes+docBytes > upsertStreamMaxBytesPerChunk) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, boundedDoc)
		batchBytes += docBytes
	}
	return flush()
}

func truncateSemDocForUpsert(doc SemDoc) SemDoc {
	if semDocByteSize(doc) <= upsertStreamMaxBytesPerChunk {
		return doc
	}
	out := doc
	out.Tools = append([]SemToolCall(nil), doc.Tools...)
	out = truncateSemDocToolField(out, semToolStringOutput)
	out = truncateSemDocToolField(out, semToolStringInputJSON)
	out = truncateSemDocToolField(out, semToolStringCommand)
	if semDocByteSize(out) > upsertStreamMaxBytesPerChunk {
		// Non-shrinkable tool overhead can keep the document over budget after
		// field truncation: a very high tool-call count, or long tool names and
		// lang hints, which never shrink. Drop the tool calls before truncating
		// prose so searchable Text survives, rather than being cut to make room
		// for overhead that would still blow the gRPC message cap.
		out.Tools = nil
	}
	out.Text = truncateSemDocStringField(out.Text, semDocByteSize(out)-upsertStreamMaxBytesPerChunk)
	out.Thinking = truncateSemDocStringField(out.Thinking, semDocByteSize(out)-upsertStreamMaxBytesPerChunk)
	return out
}

type semToolStringField int

const (
	semToolStringOutput semToolStringField = iota
	semToolStringInputJSON
	semToolStringCommand
)

func truncateSemDocToolField(doc SemDoc, field semToolStringField) SemDoc {
	for semDocByteSize(doc) > upsertStreamMaxBytesPerChunk {
		index := largestShrinkableToolStringIndex(doc.Tools, field)
		if index < 0 {
			return doc
		}
		requiredReduction := semDocByteSize(doc) - upsertStreamMaxBytesPerChunk
		switch field {
		case semToolStringOutput:
			doc.Tools[index].Output = truncateSemDocStringField(doc.Tools[index].Output, requiredReduction)
		case semToolStringInputJSON:
			doc.Tools[index].InputJSON = truncateSemDocStringField(doc.Tools[index].InputJSON, requiredReduction)
		case semToolStringCommand:
			doc.Tools[index].Command = truncateSemDocStringField(doc.Tools[index].Command, requiredReduction)
		default:
			return doc
		}
	}
	return doc
}

func largestShrinkableToolStringIndex(tools []SemToolCall, field semToolStringField) int {
	index := -1
	largestBytes := 0
	for i, tool := range tools {
		var value string
		switch field {
		case semToolStringOutput:
			value = tool.Output
		case semToolStringInputJSON:
			value = tool.InputJSON
		case semToolStringCommand:
			value = tool.Command
		default:
			return -1
		}
		if len(value) <= largestBytes || !canShrinkSemDocString(value) {
			continue
		}
		index = i
		largestBytes = len(value)
	}
	return index
}

func truncateSemDocStringField(value string, requiredReduction int) string {
	if requiredReduction <= 0 || !canShrinkSemDocString(value) {
		return value
	}
	maxBytes := len(value) - requiredReduction
	return truncateSemDocStringToMaxBytes(value, maxBytes)
}

func canShrinkSemDocString(value string) bool {
	return len(value) > len(semDocTruncationMarker(len(value)))
}

func truncateSemDocStringToMaxBytes(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	for cut := min(maxBytes, len(value)); cut >= 0; cut-- {
		if cut > 0 && cut < len(value) && !utf8.RuneStart(value[cut]) {
			continue
		}
		marker := semDocTruncationMarker(len(value) - cut)
		if cut+len(marker) <= maxBytes {
			return value[:cut] + marker
		}
	}
	// Even the marker alone exceeds maxBytes. Return the marker clamped to a
	// UTF-8 boundary within maxBytes so this helper never returns more than
	// maxBytes bytes, which keeps the per-document size guard sound.
	marker := semDocTruncationMarker(len(value))
	if len(marker) <= maxBytes {
		return marker
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(marker[cut]) {
		cut--
	}
	return marker[:cut]
}

func semDocTruncationMarker(omittedBytes int) string {
	return fmt.Sprintf("\n…[truncated %d bytes]", omittedBytes)
}

// semDocByteSize approximates one document's wire size for chunk framing: the
// text bytes plus the scalar fields plus a fixed framing allowance. The Archived
// bool contributes its protobuf tag plus value regardless of true or false.
func semDocByteSize(doc SemDoc) int {
	const semDocFramingOverheadBytes = 256
	const semDocArchivedFieldBytes = 2
	size := len(doc.Text) + len(doc.Thinking) + len(doc.ConversationID) + len(doc.ParentConversationID) + len(doc.Role) + len(doc.WorkspaceRoot) + semDocArchivedFieldBytes + semDocFramingOverheadBytes
	for _, tool := range doc.Tools {
		size += len(tool.Name) + len(tool.InputJSON) + len(tool.Command) + len(tool.LangHint) + len(tool.Output)
	}
	return size
}

// backfillScalarEntriesPerChunk caps the entries in one stream chunk so a large
// corpus frames into bounded messages well under the gRPC max message size.
const backfillScalarEntriesPerChunk = 500

// BackfillConversationScalars streams clyde's per-conversation enrichment to the
// engine, which writes workspaceRoot and archived onto the rows whose
// workspaceRoot is empty, preserving each row's dense vector so nothing is
// re-embedded. When dryRun is true the engine counts the would-change and orphan
// rows and writes nothing. It opens the client stream, sends the header, then
// the entries in bounded chunks, and returns the engine's (changed, orphan)
// counts.
func (c *Client) BackfillConversationScalars(ctx context.Context, collectionID string, entries []BackfillScalarEntry, dryRun bool) (int64, int64, error) {
	if c == nil || c.daemon == nil {
		return 0, 0, fmt.Errorf("backfill conversation scalars: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return 0, 0, fmt.Errorf("backfill conversation scalars: collection id is empty")
	}
	stream, err := c.daemon.BackfillConversationScalars(ctx)
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.backfill_scalars_open_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"err", err,
		)
		return 0, 0, fmt.Errorf("open conversation scalar backfill stream for collection %q: %w", trimmedCollectionID, err)
	}
	header := &lmsemanticsearchv1.BackfillConversationScalarsChunk{
		Chunk: &lmsemanticsearchv1.BackfillConversationScalarsChunk_Header{
			Header: &lmsemanticsearchv1.BackfillConversationScalarsHeader{
				CollectionId: trimmedCollectionID,
				DryRun:       dryRun,
				Client:       upsertClientInfo(),
			},
		},
	}
	if sendErr := stream.Send(header); sendErr != nil {
		slog.WarnContext(ctx, "conversation.semsearch.backfill_scalars_header_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"err", sendErr,
		)
		return 0, 0, fmt.Errorf("send conversation scalar backfill header for collection %q: %w", trimmedCollectionID, sendErr)
	}
	for start := 0; start < len(entries); start += backfillScalarEntriesPerChunk {
		end := min(start+backfillScalarEntriesPerChunk, len(entries))
		chunk := &lmsemanticsearchv1.BackfillConversationScalarsChunk{
			Chunk: &lmsemanticsearchv1.BackfillConversationScalarsChunk_Entries{
				Entries: &lmsemanticsearchv1.BackfillConversationScalarsEntries{
					Entries: backfillScalarEntries(entries[start:end]),
				},
			},
		}
		if sendErr := stream.Send(chunk); sendErr != nil {
			slog.WarnContext(ctx, "conversation.semsearch.backfill_scalars_entries_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"collection_id", trimmedCollectionID,
				"entries", end-start,
				"err", sendErr,
			)
			return 0, 0, fmt.Errorf("send conversation scalar backfill entries for collection %q: %w", trimmedCollectionID, sendErr)
		}
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.backfill_scalars_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"entries", len(entries),
			"err", err,
		)
		return 0, 0, fmt.Errorf("backfill conversation scalars for collection %q: %w", trimmedCollectionID, err)
	}
	return response.GetChanged(), response.GetOrphan(), nil
}

func backfillScalarEntries(entries []BackfillScalarEntry) []*lmsemanticsearchv1.BackfillConversationScalarEntry {
	out := make([]*lmsemanticsearchv1.BackfillConversationScalarEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, &lmsemanticsearchv1.BackfillConversationScalarEntry{
			ConversationId: entry.ConversationID,
			WorkspaceRoot:  entry.WorkspaceRoot,
			Archived:       entry.Archived,
		})
	}
	return out
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
func (c *Client) SearchConversations(ctx context.Context, collectionID, query string, limit int32, filter SearchFilter, perConversationLimit int32) ([]SemHit, error) {
	if c == nil || c.daemon == nil {
		return nil, fmt.Errorf("search semantic conversations: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return nil, fmt.Errorf("search semantic conversations: collection id is empty")
	}
	response, err := c.daemon.SearchConversations(ctx, &lmsemanticsearchv1.SearchConversationsRequest{
		CollectionId:         trimmedCollectionID,
		Query:                query,
		Limit:                limit,
		Filter:               filter.wire(),
		PerConversationLimit: perConversationLimit,
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

// SearchWithinConversation retrieves one conversation's matching rows plus the
// content fingerprint the engine has embedded for it. An empty fingerprint
// means the conversation is not indexed; one differing from the conversation's
// current stamp means the index trails the transcript. The caller decides
// whether to literal-scan newer content.
func (c *Client) SearchWithinConversation(ctx context.Context, collectionID, conversationID, query string, limit int32, filter SearchFilter) ([]SemHit, string, error) {
	if c == nil || c.daemon == nil {
		return nil, "", fmt.Errorf("search within semantic conversation: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return nil, "", fmt.Errorf("search within semantic conversation: collection id is empty")
	}
	trimmedConversationID := strings.TrimSpace(conversationID)
	if trimmedConversationID == "" {
		return nil, "", fmt.Errorf("search within semantic conversation: conversation id is empty")
	}
	response, err := c.daemon.SearchWithinConversation(ctx, &lmsemanticsearchv1.SearchWithinConversationRequest{
		CollectionId:   trimmedCollectionID,
		ConversationId: trimmedConversationID,
		Query:          query,
		Limit:          limit,
		Filter:         filter.wire(),
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.semsearch.search_within_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection_id", trimmedCollectionID,
			"conversation_id", trimmedConversationID,
			"err", err,
		)
		return nil, "", fmt.Errorf("search within semantic conversation %q: %w", trimmedConversationID, err)
	}
	return conversationSearchHits(response.GetResults()), response.GetIndexedFingerprint(), nil
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
			Score:                result.GetScore(),
		})
	}
	return out
}

func conversationFingerprints(manifest []Fingerprint) []*lmsemanticsearchv1.ConversationFingerprint {
	out := make([]*lmsemanticsearchv1.ConversationFingerprint, 0, len(manifest))
	for _, fingerprint := range manifest {
		out = append(out, &lmsemanticsearchv1.ConversationFingerprint{
			ConversationId: fingerprint.ConversationID,
			Fingerprint:    fingerprint.Value,
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
			Tools:                conversationToolCalls(doc.Tools),
			Thinking:             strings.ToValidUTF8(doc.Thinking, ""),
			WorkspaceRoot:        doc.WorkspaceRoot,
			Archived:             doc.Archived,
		})
	}
	return out
}

func conversationToolCalls(tools []SemToolCall) []*lmsemanticsearchv1.ConversationToolCall {
	out := make([]*lmsemanticsearchv1.ConversationToolCall, 0, len(tools))
	for _, tool := range tools {
		out = append(out, &lmsemanticsearchv1.ConversationToolCall{
			Name:      strings.ToValidUTF8(tool.Name, ""),
			InputJson: strings.ToValidUTF8(tool.InputJSON, ""),
			Command:   strings.ToValidUTF8(tool.Command, ""),
			LangHint:  strings.ToValidUTF8(tool.LangHint, ""),
			Output:    strings.ToValidUTF8(tool.Output, ""),
			IsError:   tool.IsError,
		})
	}
	return out
}
