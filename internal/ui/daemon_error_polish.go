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
	default:
		return st.Message()
	}
}
