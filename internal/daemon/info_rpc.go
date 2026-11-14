package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

// GetConversationInfo resolves one conversation and returns static metadata
// plus its compaction segment stack.
//
// The handler resolves the selector before loading message counts and
// compaction segments from the provider artifact.
//
// Example:
//
//	response, err := s.GetConversationInfo(ctx, request)
func (s *controlServer) GetConversationInfo(
	ctx context.Context,
	req *clydev1.GetConversationInfoRequest,
) (*clydev1.GetConversationInfoResponse, error) {
	if strings.TrimSpace(req.GetConversationId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "conversation selector is required")
	}

	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		level := slog.LevelWarn
		code := codes.Internal
		message := fmt.Sprintf("resolve conversation: %v", err)
		if missing, ok := errors.AsType[*conversation.NotFoundError](err); ok {
			level = slog.LevelDebug
			code = codes.NotFound
			message = missing.UserMessage()
		}

		slog.Log(
			ctx,
			level,
			"daemon.get_conversation_info.resolve_failed",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"peer",
			peerString(client),
			"conversation_id",
			req.GetConversationId(),
			"err",
			err,
		)

		return nil, status.Error(code, message)
	}

	info, err := s.index.ConversationInfo(record)
	if err != nil {
		slog.WarnContext(
			ctx,
			"daemon.get_conversation_info.load_failed",
			"concern",
			"process.daemon.lifecycle",
			"component",
			"daemon",
			"peer",
			peerString(client),
			"conversation_id",
			record.ID,
			"err",
			err,
		)

		return nil, status.Errorf(codes.Internal, "get conversation info: %v", err)
	}

	return protoConversationInfo(ctx, s.index, info), nil
}

func protoConversationInfo(
	ctx context.Context,
	idx *conversation.Index,
	info conversation.Info,
) *clydev1.GetConversationInfoResponse {
	return &clydev1.GetConversationInfoResponse{
		Conversation:    protoConversationRecord(ctx, idx, info.Record),
		Stats:           protoConversationStats(info.Stats),
		CompactionCount: int64(info.CompactionCount),
		Segments:        protoConversationSegments(info.Segments),
	}
}

func protoConversationStats(stats conversation.Stats) *clydev1.ConversationInfoStats {
	return &clydev1.ConversationInfoStats{
		TotalMessages:     int64(stats.TotalMessages),
		VisibleMessages:   int64(stats.VisibleMessages),
		UserMessages:      int64(stats.UserMessages),
		AssistantMessages: int64(stats.AssistantMessages),
		SystemMessages:    int64(stats.SystemMessages),
		ToolCallCount:     int64(stats.ToolCallCount),
		ToolOutputCount:   int64(stats.ToolOutputCount),
	}
}

func protoConversationSegments(
	segments []conversation.CompactionSegment,
) []*clydev1.ConversationCompactionSegment {
	wireSegments := make([]*clydev1.ConversationCompactionSegment, 0, len(segments))
	for _, segment := range segments {
		summaryUnix := int64(0)
		if segment.HasStartingSummary {
			summaryUnix = segment.SummaryTimestamp.Unix()
		}

		wireSegments = append(
			wireSegments,
			&clydev1.ConversationCompactionSegment{
				Index:                int64(segment.Index),
				StartMessageIndex:    int64(segment.StartMessageIndex),
				EndMessageIndex:      int64(segment.EndMessageIndex),
				HasStartingSummary:   segment.HasStartingSummary,
				SummaryMessageIndex:  int64(segment.SummaryMessageIndex),
				SummaryUuid:          segment.SummaryUUID,
				SummaryTimestampUnix: summaryUnix,
				VisibleMessageCount:  int64(segment.VisibleMessageCount),
				ToolCallCount:        int64(segment.ToolCallCount),
				ExportSelector:       strconv.Itoa(segment.Index),
			},
		)
	}

	return wireSegments
}
