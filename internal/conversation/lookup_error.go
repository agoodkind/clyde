package conversation

import "fmt"

// NotFoundError records a conversation lookup miss and its diagnostic reason.
type NotFoundError struct {
	Selector string
	Reason   RequestNotFoundReason
	Fresh    bool
}

// Error returns the diagnostic message, including request lookup and refresh details.
func (err *NotFoundError) Error() string {
	message := fmt.Sprintf("conversation %q not found", err.Selector)
	if err.Reason != RequestNotFoundReasonUnspecified {
		return message + ": " + notFoundNote(err.Reason, err.Fresh)
	}

	if !err.Fresh {
		return message + ": " + staleIndexNote
	}

	return message
}

// UserMessage returns the concise message for CLI and MCP callers.
func (err *NotFoundError) UserMessage() string {
	return fmt.Sprintf("Conversation %s not found.", err.Selector)
}
