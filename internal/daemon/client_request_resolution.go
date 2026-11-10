package daemon

import (
	"context"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

// ResolveConversationRequest asks the running daemon which conversation issued
// one provider request id, and how it found out.
//
// allowFullScan permits the exhaustive provider-store scan when the bounded
// paths miss. It costs tens of seconds, so only pass it for a caller that asked
// for it knowingly. The RPC deadline is widened to the analysis timeout in that
// case, since the default query timeout is shorter than the scan.
func ResolveConversationRequest(
	ctx context.Context,
	requestID string,
	allowFullScan bool,
) (conversation.RequestResolution, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.RequestResolution{}, err
	}
	defer func() { _ = client.conn.Close() }()

	timeout := queryClientRPCTimeout
	if allowFullScan {
		timeout = analysisClientRPCTimeout
	}
	rpcCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := client.rpc.ResolveConversationRequest(rpcCtx, &clydev1.ResolveConversationRequestRequest{
		RequestId:     requestID,
		AllowFullScan: allowFullScan,
	})
	if err != nil {
		return conversation.RequestResolution{}, daemonRPCError(rpcCtx, "resolve conversation request", err)
	}
	return conversation.RequestResolution{
		RequestID: resp.GetRequestId(),
		Found:     resp.GetFound(),
		Origin:    requestOriginFromProto(resp.GetOrigin()),
		Reason:    requestNotFoundReasonFromProto(resp.GetNotFoundReason()),
		Record:    conversationRecordFromProto(resp.GetConversation()),
	}, nil
}

func requestOriginFromProto(origin clydev1.RequestResolutionOrigin) conversation.RequestOrigin {
	switch origin {
	case clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_UNSPECIFIED:
		return conversation.RequestOriginUnspecified
	case clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_INDEX:
		return conversation.RequestOriginIndex
	case clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_LIVE:
		return conversation.RequestOriginLive
	case clydev1.RequestResolutionOrigin_REQUEST_RESOLUTION_ORIGIN_FULL_SCAN:
		return conversation.RequestOriginFullScan
	default:
		return conversation.RequestOriginUnspecified
	}
}

func requestNotFoundReasonFromProto(reason clydev1.RequestResolutionNotFoundReason) conversation.RequestNotFoundReason {
	switch reason {
	case clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_UNSPECIFIED:
		return conversation.RequestNotFoundReasonUnspecified
	case clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_NO_RESOLVER:
		return conversation.RequestNotFoundReasonNoResolver
	case clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_NOT_RETAINED:
		return conversation.RequestNotFoundReasonNotRetained
	case clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_NO_MATCHING_CONVERSATION:
		return conversation.RequestNotFoundReasonNoMatchingConversation
	case clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_UNINDEXED_CONVERSATION:
		return conversation.RequestNotFoundReasonUnindexedConversation
	case clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_AMBIGUOUS_CONVERSATION:
		return conversation.RequestNotFoundReasonAmbiguousConversation
	case clydev1.RequestResolutionNotFoundReason_REQUEST_RESOLUTION_NOT_FOUND_REASON_INCONCLUSIVE:
		return conversation.RequestNotFoundReasonInconclusive
	default:
		return conversation.RequestNotFoundReasonUnspecified
	}
}
