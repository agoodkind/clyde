package ui

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// polishDaemonError translates a gRPC-shaped daemon error into a
// user-facing one-line status string. Unknown errors fall through to
// the raw err.Error() so debug context is not lost.
func polishDaemonError(err error) string {
	if err == nil {
		return ""
	}
	st, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}
	// The exhaustive linter requires every codes.Code constant to be
	// listed explicitly. The four cases above carry the polished
	// user-facing strings; every remaining code falls through to
	// st.Message() unchanged. The default arm preserves that behavior
	// for any future codes.Code value that is added upstream.
	switch st.Code() {
	case codes.FailedPrecondition:
		message := st.Message()
		if strings.Contains(message, "session in use") || strings.Contains(message, "is currently open") {
			return "session in use; close it before continuing"
		}
		return message
	case codes.NotFound:
		return "session not found"
	case codes.Unavailable:
		return "daemon unavailable; check that clyde-daemon is running"
	case codes.DeadlineExceeded:
		return "request timed out; try again"
	case codes.OK,
		codes.Canceled,
		codes.Unknown,
		codes.InvalidArgument,
		codes.AlreadyExists,
		codes.PermissionDenied,
		codes.ResourceExhausted,
		codes.Aborted,
		codes.OutOfRange,
		codes.Unimplemented,
		codes.Internal,
		codes.DataLoss,
		codes.Unauthenticated:
		return st.Message()
	default:
		return st.Message()
	}
}
