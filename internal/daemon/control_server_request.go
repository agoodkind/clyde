package daemon

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

// ResolveConversationRequest maps one provider request id to the conversation
// that issued it and reports which path answered.
//
// An id no path resolves is a successful response carrying found=false and the
// reason, not an error: the caller asked a question the daemon answered, and the
// answer is that nothing in the corpus carries that id.
func (s *controlServer) ResolveConversationRequest(
	ctx context.Context,
	req *clydev1.ResolveConversationRequestRequest,
) (*clydev1.ResolveConversationRequestResponse, error) {
	client, _ := peer.FromContext(ctx)
	requestID := strings.TrimSpace(req.GetRequestId())
	if requestID == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required")
	}
	resolution, err := s.index.ResolveRequest(ctx, requestID, conversation.RequestLookupOptions{
		AllowFullScan: req.GetAllowFullScan(),
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.resolve_conversation_request.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"request_id", requestID,
			"allow_full_scan", req.GetAllowFullScan(),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "resolve conversation request: %v", err)
	}
	response := &clydev1.ResolveConversationRequestResponse{
		RequestId:      resolution.RequestID,
		Found:          resolution.Found,
		Origin:         protoRequestResolutionOrigin(resolution.Origin),
		NotFoundReason: protoRequestNotFoundReason(resolution.Reason),
		Conversation:   nil,
	}
	if resolution.Found {
		response.Conversation = protoConversationRecord(ctx, s.index, resolution.Record)
	}
	return response, nil
}

func protoRequestResolutionOrigin(origin conversation.RequestOrigin) clydev1.RequestResolutionOrigin {
	switch origin {
	case conversation.RequestOriginUnspecified:
		return clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_UNSPECIFIED
	case conversation.RequestOriginIndex:
		return clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_INDEX
	case conversation.RequestOriginLive:
		return clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_LIVE
	case conversation.RequestOriginFullScan:
		return clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_FULL_SCAN
	default:
		return clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_UNSPECIFIED
	}
}

func protoRequestNotFoundReason(reason conversation.RequestNotFoundReason) clydev1.RequestResolutionNotFoundReason {
	switch reason {
	case conversation.RequestNotFoundReasonUnspecified:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_UNSPECIFIED
	case conversation.RequestNotFoundReasonNoResolver:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_NO_RESOLVER
	case conversation.RequestNotFoundReasonNotRetained:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_NOT_RETAINED
	case conversation.RequestNotFoundReasonNoMatchingConversation:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_NO_MATCHING_CONVERSATION
	case conversation.RequestNotFoundReasonUnindexedConversation:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_UNINDEXED_CONVERSATION
	case conversation.RequestNotFoundReasonAmbiguousConversation:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_AMBIGUOUS_CONVERSATION
	case conversation.RequestNotFoundReasonInconclusive:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_INCONCLUSIVE
	default:
		return clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_UNSPECIFIED
	}
}
