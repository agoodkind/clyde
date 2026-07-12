package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/tokencount"
	"goodkind.io/clyde/internal/util"
)

// controlStreamChunkBytes is the payload size of one streamed control-leg chunk.
// It sits well under the gRPC default max message size so a large transcript or
// export crosses the wire as many small frames instead of one oversized message.
const controlStreamChunkBytes = 256 << 10

// StreamConversation resolves one conversation, renders its transcript as plain
// text, and streams that text in bounded chunks so a large transcript is not
// capped by the gRPC max message size. The payload is identical to the unary
// GetConversation text; only the transport differs.
func (s *controlServer) StreamConversation(req *clydev1.GetConversationRequest, stream grpc.ServerStreamingServer[clydev1.ConversationChunk]) error {
	ctx := stream.Context()
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_conversation.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	text, err := s.index.RenderPlainText(record, int(req.GetLastN()))
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_conversation.render_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return status.Errorf(codes.Internal, "render conversation: %v", err)
	}
	return streamTextChunks(stream, text)
}

// StreamConversationContext resolves one conversation, renders the messages
// around a center point as plain text, and streams that text in bounded chunks.
// The payload is identical to the unary GetConversationContext text.
func (s *controlServer) StreamConversationContext(req *clydev1.GetConversationContextRequest, stream grpc.ServerStreamingServer[clydev1.ConversationChunk]) error {
	ctx := stream.Context()
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_context.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	text, err := s.index.ContextWindowText(record, req.GetTimestamp(), int(req.GetMessageIndex()), int(req.GetBefore()), int(req.GetAfter()))
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_context.render_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return status.Errorf(codes.Internal, "render context: %v", err)
	}
	return streamTextChunks(stream, text)
}

// StreamExportTranscript resolves one conversation, exports its body, and streams
// the body in bounded byte chunks. The payload is identical to the unary
// ExportTranscript body.
func (s *controlServer) StreamExportTranscript(req *clydev1.ExportTranscriptRequest, stream grpc.ServerStreamingServer[clydev1.ExportChunk]) error {
	ctx := stream.Context()
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_export.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	options := conversation.ExportOptions{
		Format:       conversation.ExportFormat(req.GetFormat()),
		HistoryStart: int(req.GetHistoryStart()),
		LastN:        int(req.GetLastN()),
		MaxLines:     int(req.GetMaxLines()),
		MaxTokens:    req.GetMaxTokens(),
		TokenModel:   req.GetTokenModel(),
		Whitespace:   conversation.WhitespaceMode(req.GetWhitespace()),
		Content:      contentKindSetFromExportRequest(req),
		Compaction: conversation.CompactionExportOptions{
			IncludeSelector: req.GetIncludeCompactions(),
			FullHistory:     req.GetFullHistory(),
		},
	}
	body, err := s.index.Export(record, options)
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_export.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return status.Errorf(codes.Internal, "export transcript: %v", err)
	}
	body, err = capExportBodyTokens(ctx, body, req.GetMaxTokens(), req.GetTokenModel(), record)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "max tokens: %v", err)
	}
	return streamExportChunks(stream, body)
}

// capExportBodyTokens caps a rendered export body to the max_tokens budget,
// keeping the tail. The tokenizer family and model come from the conversation
// record; a non-empty tokenModel overrides the model. An empty maxTokens or a
// zero budget leaves the body unchanged. It returns an error only when the
// max_tokens string cannot be parsed.
func capExportBodyTokens(ctx context.Context, body []byte, maxTokens, tokenModel string, record conversation.Record) ([]byte, error) {
	if maxTokens == "" {
		return body, nil
	}
	budget, err := util.ParseHumanCount(maxTokens)
	if err != nil {
		slog.WarnContext(ctx, "daemon.stream_export.max_tokens_invalid", "concern", "process.daemon.lifecycle", "component", "daemon",
			"conversation_id", record.ID,
			"max_tokens", maxTokens,
			"err", err,
		)
		return nil, fmt.Errorf("parse max tokens: %w", err)
	}
	if budget <= 0 {
		return body, nil
	}
	family := tokenFamilyForProvider(record.Provider)
	model := record.Model
	if tokenModel != "" {
		family = tokencount.FamilyUnknown
		model = tokenModel
	}
	settings := tokencount.Settings{
		SafetyFactor:  tokencount.DefaultSafetyFactor,
		CharsPerToken: tokencount.DefaultCharsPerToken,
	}
	counter := tokencount.LocalCounter(family, model, settings)
	capped, tokens, truncated := tokencount.CapToLastTokens(string(body), budget, counter)
	slog.DebugContext(ctx, "daemon.stream_export.token_cap", "concern", "process.daemon.lifecycle", "component", "daemon",
		"conversation_id", record.ID,
		"budget", budget,
		"family", int(family),
		"model", model,
		"tokens", tokens,
		"truncated", truncated,
	)
	return []byte(capped), nil
}

// tokenFamilyForProvider maps a conversation provider to the tokenizer family
// used to count its transcript. Unmapped providers fall back to model inference.
func tokenFamilyForProvider(provider conversation.Provider) tokencount.Family {
	switch provider {
	case conversation.ProviderClaude:
		return tokencount.FamilyClaude
	case conversation.ProviderCodex:
		return tokencount.FamilyGPT
	default:
		return tokencount.FamilyUnknown
	}
}

// streamTextChunks sends text as a sequence of ConversationChunk frames, each at
// most controlStreamChunkBytes bytes. An empty payload sends no frames, which the
// reassembler reads as an empty string.
func streamTextChunks(stream grpc.ServerStreamingServer[clydev1.ConversationChunk], text string) error {
	body := []byte(text)
	for offset := 0; offset < len(body); offset += controlStreamChunkBytes {
		end := min(offset+controlStreamChunkBytes, len(body))
		if err := stream.Send(&clydev1.ConversationChunk{Text: body[offset:end]}); err != nil {
			slog.WarnContext(stream.Context(), "daemon.stream_text.send_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", err,
			)
			return status.Errorf(codes.Internal, "stream conversation chunk: %v", err)
		}
	}
	return nil
}

// streamExportChunks sends body as a sequence of ExportChunk frames, each at most
// controlStreamChunkBytes bytes. An empty body sends no frames, which the
// reassembler reads as an empty byte slice.
func streamExportChunks(stream grpc.ServerStreamingServer[clydev1.ExportChunk], body []byte) error {
	for offset := 0; offset < len(body); offset += controlStreamChunkBytes {
		end := min(offset+controlStreamChunkBytes, len(body))
		if err := stream.Send(&clydev1.ExportChunk{Body: body[offset:end]}); err != nil {
			slog.WarnContext(stream.Context(), "daemon.stream_export.send_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", err,
			)
			return status.Errorf(codes.Internal, "stream export chunk: %v", err)
		}
	}
	return nil
}
