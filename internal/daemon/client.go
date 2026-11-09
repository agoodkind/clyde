package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/loginventory"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitmshow"
)

// controlMaxMessageBytes is the message-size ceiling for the control leg, set on
// both the daemon server and the CLI/MCP client. Large reads and exports cross
// the wire as chunk streams, so this is a belt-and-suspenders bound for any
// remaining unary control RPC rather than the primary mechanism.
const controlMaxMessageBytes = 128 << 20

const (
	reloadClientOverallTimeout = 60 * time.Second
	reloadClientRPCTimeout     = 30 * time.Second
	queryClientRPCTimeout      = 30 * time.Second
	analysisClientRPCTimeout   = 10 * time.Minute
	daemonProbeTimeout         = 2 * time.Second
)

type daemonClient struct {
	conn *grpc.ClientConn
	rpc  clydev1.ClydeServiceClient
}

// ReloadDaemon asks the running worker to hand its live listeners to a
// supervisor-spawned replacement worker.
func ReloadDaemon(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
	unlock, err := lockDaemonReload(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	retryCtx, cancel := context.WithTimeout(ctx, reloadClientOverallTimeout)
	defer cancel()
	client, err := connectDaemon(retryCtx)
	if err != nil {
		return nil, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, rpcCancel := context.WithTimeout(retryCtx, reloadClientRPCTimeout)
	defer rpcCancel()
	resp, err := client.rpc.ReloadDaemon(rpcCtx, &clydev1.ReloadDaemonRequest{})
	if err != nil {
		slog.WarnContext(
			rpcCtx,
			"daemon.client.reload.rpc_failed",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"err",
			err,
		)

		return nil, fmt.Errorf("daemon reload rpc: %w", err)
	}

	return resp, nil
}

// RebindDaemon asks the running worker to replace itself with one that binds
// its config-driven listeners fresh, for a config edit that moves a listener
// address. It takes the same daemon.reload.lock flock as ReloadDaemon so a
// watcher-driven rebind serializes against any CLI-driven reload.
func RebindDaemon(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
	unlock, err := lockDaemonReload(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	retryCtx, cancel := context.WithTimeout(ctx, reloadClientOverallTimeout)
	defer cancel()
	client, err := connectDaemon(retryCtx)
	if err != nil {
		return nil, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, rpcCancel := context.WithTimeout(retryCtx, reloadClientRPCTimeout)
	defer rpcCancel()
	resp, err := client.rpc.RebindDaemon(rpcCtx, &clydev1.ReloadDaemonRequest{})
	if err != nil {
		slog.WarnContext(
			rpcCtx,
			"daemon.client.rebind.rpc_failed",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"err",
			err,
		)

		return nil, fmt.Errorf("daemon rebind rpc: %w", err)
	}

	return resp, nil
}

// ListConversations asks the running daemon for a filtered conversation page.
// When the daemon is not reachable it returns a clear error that names the rpc
// socket and points at `clyde daemon status`.
func ListConversations(
	ctx context.Context,
	options conversation.ListOptions,
) (conversation.ListResult, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.ListResult{}, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ListConversations(
		rpcCtx,
		&clydev1.ListConversationsRequest{
			Limit:           int64(options.Limit),
			Offset:          int64(options.Offset),
			Provider:        protoProvider(options.Provider),
			Workspace:       options.WorkspaceRoot,
			Query:           options.Query,
			IncludeArchived: options.IncludeArchived,
			All:             options.All,
		},
	)
	if err != nil {
		return conversation.ListResult{}, daemonRPCError(rpcCtx, "list conversations", err)
	}

	records := conversationRecordsFromProto(resp.GetConversations())

	return conversation.ListResult{
		Records:       records,
		TotalMatched:  int(resp.GetTotalMatched()),
		ReturnedCount: int(resp.GetReturnedCount()),
		Offset:        int(resp.GetOffset()),
		Limit:         int(resp.GetLimit()),
		NextOffset:    int(resp.GetNextOffset()),
		HasMore:       resp.GetHasMore(),
	}, nil
}

// GetConversation asks the daemon for one conversation transcript rendered as
// plain text.
func GetConversation(ctx context.Context, conversationID string, lastN int) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	stream, err := client.rpc.StreamConversation(
		rpcCtx,
		&clydev1.GetConversationRequest{
			ConversationId: conversationID,
			LastN:          int64(lastN),
		},
	)
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation", err)
	}

	text, err := reassembleConversationChunks(rpcCtx, stream)
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation", err)
	}

	return text, nil
}

// GetConversationInfo asks the daemon for one conversation's static metadata
// and compaction segment stack.
func GetConversationInfo(ctx context.Context, conversationID string) (conversation.Info, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.Info{}, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetConversationInfo(
		rpcCtx,
		&clydev1.GetConversationInfoRequest{
			ConversationId: conversationID,
		},
	)
	if err != nil {
		return conversation.Info{}, daemonRPCError(rpcCtx, "get conversation info", err)
	}

	return conversationInfoFromProto(resp), nil
}

// GetConversationContext asks the daemon for the messages around a center point
// rendered as plain text. loadRules is the loading-rules tag a search hit
// carried for the row that produced messageIndex; empty means the default
// rules.
func GetConversationContext(
	ctx context.Context,
	conversationID string,
	timestamp string,
	messageIndex int,
	before int,
	after int,
	loadRules string,
) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	stream, err := client.rpc.StreamConversationContext(
		rpcCtx,
		&clydev1.GetConversationContextRequest{
			ConversationId: conversationID,
			Timestamp:      timestamp,
			MessageIndex:   int64(messageIndex),
			Before:         int64(before),
			After:          int64(after),
			LoadRules:      loadRules,
		},
	)
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation context", err)
	}

	text, err := reassembleConversationChunks(rpcCtx, stream)
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation context", err)
	}

	return text, nil
}

// SearchConversations asks the daemon for bounded transcript text matches
// across candidate conversations.
//
// The request includes the backend selection and search filters. The result
// includes paging, source, and freshness details. Connection and RPC failures
// return an error instead of an empty successful search.
//
//	result, err := SearchConversations(
//		ctx,
//		conversation.SearchConversationsOptions{
//			Query:   "reload",
//			Backend: conversation.SearchBackendLocal,
//			Limit:   10,
//		},
//	)
func SearchConversations(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.SearchConversationsResult{}, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.SearchConversations(
		rpcCtx,
		&clydev1.SearchConversationsRequest{
			Query:                options.Query,
			Backend:              string(options.Backend),
			Limit:                int64(options.Limit),
			Offset:               int64(options.Offset),
			Provider:             protoProvider(options.Provider),
			Workspace:            options.WorkspaceRoot,
			IncludeArchived:      options.IncludeArchived,
			Roles:                options.Roles,
			FromUnix:             options.FromUnix,
			UntilUnix:            options.UntilUnix,
			MinScore:             options.MinScore,
			PerConversationLimit: int64(options.PerConversationLimit),
			ConversationId:       options.ConversationID,
			ContextWindow:        int64(options.ContextWindow),
			Cursor:               options.Cursor,
		},
	)
	if err != nil {
		return conversation.SearchConversationsResult{}, daemonRPCError(
			rpcCtx,
			"search conversations",
			err,
		)
	}

	matches := make([]conversation.SearchMatch, 0, len(resp.GetMatches()))
	for _, wire := range resp.GetMatches() {
		record := conversationRecordFromProto(wire.GetConversation())
		matches = append(
			matches,
			conversation.SearchMatch{
				Record:        record,
				MessageIndex:  int(wire.GetMessageIndex()),
				Role:          wire.GetRole(),
				Timestamp:     time.Unix(wire.GetTimestampUnix(), 0),
				Snippet:       wire.GetSnippet(),
				Score:         wire.GetScore(),
				ContextWindow: wire.GetContextWindow(),
				LoadRules:     wire.GetLoadRules(),
				ContextState:  searchContextStateFromProto(wire.GetContextState()),
			},
		)
	}

	return conversation.SearchConversationsResult{
		Matches:              matches,
		ConversationsScanned: int(resp.GetConversationsScanned()),
		ReturnedCount:        int(resp.GetReturnedCount()),
		Limit:                int(resp.GetLimit()),
		Offset:               int(resp.GetOffset()),
		NextOffset:           int(resp.GetNextOffset()),
		HasMore:              resp.GetHasMore(),
		NextCursor:           resp.GetNextCursor(),
		Source:               searchSourceFromProto(resp.GetSource()),
		Facets:               searchFacetsFromProto(resp.GetFacets()),
		Freshness:            searchFreshnessFromProto(resp.GetSemanticFreshness()),
		FilterAccounting:     filterAccountingFromProto(resp.GetFilterAccounting()),
	}, nil
}

// ReorientConversationForHook asks the daemon for one reorient page and lets
// hook callers recover the current conversation before the provider compacts.
// The recovered transcript is still capped by maxLines (or the daemon default).
func ReorientConversationForHook(
	ctx context.Context,
	conversationID string,
	workspace string,
	cursor string,
	maxLines int,
	pageBytes int,
	includeToolOutputs bool,
	syntheticPreCompact bool,
) (conversation.ReorientPage, error) {
	return reorientConversation(
		ctx,
		conversationID,
		workspace,
		cursor,
		maxLines,
		pageBytes,
		includeToolOutputs,
		syntheticPreCompact,
	)
}

func reorientConversation(
	ctx context.Context,
	conversationID string,
	workspace string,
	cursor string,
	maxLines int,
	pageBytes int,
	includeToolOutputs bool,
	syntheticPreCompact bool,
) (conversation.ReorientPage, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.ReorientPage{}, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ReorientConversation(
		rpcCtx,
		&clydev1.ReorientConversationRequest{
			ConversationId:      conversationID,
			Workspace:           workspace,
			Cursor:              cursor,
			MaxLines:            int64(maxLines),
			PageBytes:           int64(pageBytes),
			IncludeToolOutputs:  includeToolOutputs,
			SyntheticPrecompact: syntheticPreCompact,
		},
	)
	if err != nil {
		return conversation.ReorientPage{}, daemonRPCError(rpcCtx, "reorient conversation", err)
	}

	if resp.GetCurrentConversation() == nil {
		return conversation.ReorientPage{}, fmt.Errorf(
			"reorient conversation: daemon returned no current conversation; deploy matching daemon binary",
		)
	}

	return reorientPageFromProto(resp), nil
}

// ExportTranscript asks the daemon to export one conversation and returns the
// exported body.
func ExportTranscript(
	ctx context.Context,
	conversationID string,
	options conversation.ExportOptions,
) ([]byte, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return nil, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	stream, err := client.rpc.StreamExportTranscript(
		rpcCtx,
		exportTranscriptRequest(conversationID, options),
	)
	if err != nil {
		return nil, daemonRPCError(rpcCtx, "export transcript", err)
	}

	body, err := reassembleExportChunks(rpcCtx, stream)
	if err != nil {
		return nil, daemonRPCError(rpcCtx, "export transcript", err)
	}

	return body, nil
}

// reassembleConversationChunks reads a ConversationChunk stream to completion and
// returns the concatenated text. Chunk boundaries carry no semantics, so it
// stitches the raw bytes and converts once at the end, which keeps a boundary
// that falls inside a multibyte rune intact.
func reassembleConversationChunks(
	ctx context.Context,
	stream grpc.ServerStreamingClient[clydev1.ConversationChunk],
) (string, error) {
	var body []byte

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return string(body), nil
		}

		if err != nil {
			slog.WarnContext(
				ctx,
				"daemon.client.conversation_chunk.recv_failed",
				"concern",
				"process.daemon.lifecycle",
				"component",
				"daemon",
				"err",
				err,
			)

			return "", fmt.Errorf("receive conversation chunk: %w", err)
		}

		body = append(body, chunk.GetText()...)
	}
}

// reassembleExportChunks reads an ExportChunk stream to completion and returns
// the concatenated body bytes.
func reassembleExportChunks(
	ctx context.Context,
	stream grpc.ServerStreamingClient[clydev1.ExportChunk],
) ([]byte, error) {
	var body []byte

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return body, nil
		}

		if err != nil {
			slog.WarnContext(
				ctx,
				"daemon.client.export_chunk.recv_failed",
				"concern",
				"process.daemon.lifecycle",
				"component",
				"daemon",
				"err",
				err,
			)

			return nil, fmt.Errorf("receive export chunk: %w", err)
		}

		body = append(body, chunk.GetBody()...)
	}
}

func exportTranscriptRequest(
	conversationID string,
	options conversation.ExportOptions,
) *clydev1.ExportTranscriptRequest {
	return &clydev1.ExportTranscriptRequest{
		ConversationId:         conversationID,
		Format:                 string(options.Format),
		Whitespace:             string(options.Whitespace),
		HistoryStart:           int64(options.HistoryStart),
		LastN:                  int64(options.LastN),
		MaxLines:               int64(options.MaxLines),
		MaxTokens:              options.MaxTokens,
		TokenModel:             options.TokenModel,
		IncludeSystemPrompts:   options.Content.Has(conversation.ContentKindSystemPrompts),
		IncludeSystemMessages:  options.Content.Has(conversation.ContentKindSystemMessages),
		IncludeToolOutputs:     options.Content.Has(conversation.ContentKindToolOutputs),
		IncludeRawJsonMetadata: options.Content.Has(conversation.ContentKindRawJSONMetadata),
		IncludeThinking:        options.Content.Has(conversation.ContentKindThinking),
		IncludeToolSummaries:   options.Content.Has(conversation.ContentKindToolSummaries),
		IncludeToolCalls:       options.Content.Has(conversation.ContentKindToolCalls),
		IncludeChat:            options.Content.Has(conversation.ContentKindChat),
		IncludeInjected:        options.Content.Has(conversation.ContentKindInjected),
		IncludeCompactions:     options.Compaction.IncludeSelector,
		FullHistory:            options.Compaction.FullHistory,
	}
}

// GetMITMStatus asks the daemon for each MITM listener address, whether it is
// bound, and the CA paths.
func GetMITMStatus(ctx context.Context) (MITMStatus, error) {
	empty := MITMStatus{Listeners: nil, CACertPath: "", CAKeyPath: ""}
	client, err := connectDaemon(ctx)
	if err != nil {
		return empty, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetMITMStatus(rpcCtx, &clydev1.GetMITMStatusRequest{})
	if err != nil {
		return empty, daemonRPCError(rpcCtx, "get mitm status", err)
	}

	wire := resp.GetListeners()

	listeners := make([]MITMListenerStatus, 0, len(wire))
	for _, item := range wire {
		listeners = append(
			listeners,
			MITMListenerStatus{
				ID:      item.GetId(),
				Address: item.GetAddress(),
				Up:      item.GetUp(),
			},
		)
	}

	return MITMStatus{
		Listeners:  listeners,
		CACertPath: resp.GetCaCertPath(),
		CAKeyPath:  resp.GetCaKeyPath(),
	}, nil
}

// ShowCapture asks the daemon to correlate one request id across its logs and
// the SQLite capture store and returns the typed lookup result.
func ShowCapture(ctx context.Context, id string) (mitmshow.ShowOutput, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return mitmshow.ShowOutput{}, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ShowCapture(rpcCtx, &clydev1.ShowCaptureRequest{Id: id})
	if err != nil {
		return mitmshow.ShowOutput{}, daemonRPCError(rpcCtx, "show capture", err)
	}

	if len(resp.GetPasses()) == 0 && legacyShowCaptureOutput(resp) != "" {
		return mitmshow.ShowOutput{}, fmt.Errorf(
			"show capture: daemon returned legacy text-only response; deploy matching daemon binary",
		)
	}

	return showCaptureOutputFromProto(resp), nil
}

// SeedBaseline asks the daemon to build a wire baseline from the capture
// store's deduped shape corpus and write it as the current baseline for the
// given upstream.
func SeedBaseline(
	ctx context.Context,
	upstream string,
	includeUA []string,
	excludeUA []string,
) (mitm.SeedBaselineResult, error) {
	empty := mitm.SeedBaselineResult{Upstream: "", Flavors: 0}
	client, err := connectDaemon(ctx)
	if err != nil {
		return empty, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.SeedBaseline(
		rpcCtx,
		&clydev1.SeedBaselineRequest{
			Upstream:  upstream,
			IncludeUa: includeUA,
			ExcludeUa: excludeUA,
		},
	)
	if err != nil {
		return empty, daemonRPCError(rpcCtx, "seed baseline", err)
	}

	return mitm.SeedBaselineResult{
		Upstream: resp.GetUpstream(),
		Flavors:  int(resp.GetFlavors()),
	}, nil
}

// LogsInventory asks the daemon to build a metadata-only inventory of its log
// files and return it as typed data.
func LogsInventory(
	ctx context.Context,
	stateRoot string,
	largestFileLimit int,
	deep bool,
) (loginventory.Inventory, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return loginventory.Inventory{}, err
	}

	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.LogsInventory(
		rpcCtx,
		&clydev1.LogsInventoryRequest{
			StateRoot:        stateRoot,
			LargestFileLimit: int64(largestFileLimit),
			Deep:             deep,
		},
	)
	if err != nil {
		return loginventory.Inventory{}, daemonRPCError(rpcCtx, "logs inventory", err)
	}

	return logsInventoryFromProto(resp), nil
}

// daemonRPCError translates a failed control-plane rpc into a caller-facing
// error. An Unavailable code means the daemon is not running, so the message
// names the rpc target and how to check the daemon.
func daemonRPCError(ctx context.Context, operation string, err error) error {
	target := daemonGRPCAddress()
	if status.Code(err) == codes.Unavailable {
		slog.WarnContext(
			ctx,
			"daemon.client.rpc.unavailable",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"operation",
			operation,
			"grpc_address",
			target,
			"err",
			err,
		)

		return fmt.Errorf(
			"clyde daemon is not running at %s; check `clyde daemon status` (launchd starts the daemon): %w",
			target,
			err,
		)
	}

	slog.WarnContext(
		ctx,
		"daemon.client.rpc.failed",
		"concern",
		"process.daemon.lifecycle",
		"component",
		"daemon",
		"operation",
		operation,
		"err",
		err,
	)

	return fmt.Errorf("daemon rpc: %w", err)
}

func connectDaemon(ctx context.Context) (*daemonClient, error) {
	target := daemonGRPCAddress()
	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(controlMaxMessageBytes),
			grpc.MaxCallSendMsgSize(controlMaxMessageBytes),
		),
	)
	if err != nil {
		slog.WarnContext(
			ctx,
			"daemon.client.connect.new_client_failed",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"grpc_address",
			target,
			"err",
			err,
		)

		return nil, fmt.Errorf("connect daemon at %s: %w", target, err)
	}

	return &daemonClient{
		conn: conn,
		rpc:  clydev1.NewClydeServiceClient(conn),
	}, nil
}

func daemonGRPCAddress() string {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		// Surface the config error before falling back so an operator sees why
		// the CLI targeted the default socket instead of their configured one.
		slog.Warn(
			"daemon.client.grpc_address.load_failed",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"fallback",
			config.DefaultDaemonGRPCAddress(),
			"err",
			err,
		)

		return config.DefaultDaemonGRPCAddress()
	}

	return cfg.Daemon.GRPCAddress
}

func lockDaemonReload(ctx context.Context) (func(), error) {
	if err := config.EnsureRuntimeDir(); err != nil {
		return nil, fmt.Errorf("ensure runtime dir for reload lock: %w", err)
	}

	lockPath := daemonReloadLockPath()
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.WarnContext(
			ctx,
			"daemon.client.reload_lock.open_failed",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"lock_path",
			lockPath,
			"err",
			err,
		)

		return nil, fmt.Errorf("open reload lock: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("reload lock panic: %v", recovered)
			}
		}()
		done <- syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
	}()
	select {
	case <-ctx.Done():
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock daemon reload: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			_ = lockFile.Close()
			slog.WarnContext(
				ctx,
				"daemon.client.reload_lock.lock_failed",
				"concern",
				"process.daemon.lifecycle",
				"component",
				"daemon",
				"lock_path",
				lockPath,
				"err",
				err,
			)

			return nil, fmt.Errorf("lock reload: %w", err)
		}
	}

	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}
