package daemon

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type conversationSearchFailureCode string

const (
	conversationSearchSourceUnavailable conversationSearchFailureCode = "conversation_search_source_unavailable"
	conversationSearchSourceRefused     conversationSearchFailureCode = "conversation_search_source_refused"
	conversationSearchSourceFailed      conversationSearchFailureCode = "conversation_search_source_failed"
)

// conversationSearchSourceError carries a stable caller-safe classification
// while retaining the complete source cause for structured logs.
type conversationSearchSourceError struct {
	code    conversationSearchFailureCode
	rpcCode codes.Code
	cause   error
}

func (e conversationSearchSourceError) Error() string {
	switch e.code {
	case conversationSearchSourceUnavailable:
		return string(e.code) + ": conversation search is unavailable"
	case conversationSearchSourceRefused:
		return string(e.code) + ": conversation search source refused the query"
	case conversationSearchSourceFailed:
		return string(e.code) + ": conversation search failed"
	default:
		return string(conversationSearchSourceFailed) + ": conversation search failed"
	}
}

func (e conversationSearchSourceError) Unwrap() error {
	return e.cause
}

func (e conversationSearchSourceError) grpcCode() codes.Code {
	if e.rpcCode == codes.OK {
		return codes.Internal
	}
	return e.rpcCode
}

func unavailableConversationSearchSourceError(cause error) conversationSearchSourceError {
	return conversationSearchSourceError{
		code:    conversationSearchSourceUnavailable,
		rpcCode: codes.FailedPrecondition,
		cause:   cause,
	}
}

func refusedConversationSearchSourceError(cause error) conversationSearchSourceError {
	rpcCode := sourceRPCCode(cause)
	if rpcCode == codes.Unknown || rpcCode == codes.OK {
		rpcCode = codes.InvalidArgument
	}
	return conversationSearchSourceError{
		code:    conversationSearchSourceRefused,
		rpcCode: rpcCode,
		cause:   cause,
	}
}

func failedConversationSearchSourceError(cause error) conversationSearchSourceError {
	return conversationSearchSourceError{
		code:    conversationSearchSourceFailed,
		rpcCode: codes.Internal,
		cause:   cause,
	}
}

// grpcStatusError is the interface a gRPC status error implements. Matching it
// through [errors.As] walks wrapped errors without inspecting caller-facing text.
type grpcStatusError interface {
	error
	GRPCStatus() *status.Status
}

func sourceRPCCode(err error) codes.Code {
	var statusErr grpcStatusError
	if !errors.As(err, &statusErr) {
		return codes.Unknown
	}
	return statusErr.GRPCStatus().Code()
}

func isConversationSearchSourceUnavailable(err error) bool {
	code := sourceRPCCode(err)
	return code == codes.Unavailable || code == codes.DeadlineExceeded
}
