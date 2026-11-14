package conversation

import "fmt"

// NotFoundError records a conversation lookup miss and its diagnostic reason.
type NotFoundError struct {
	Selector string
	Reason   RequestNotFoundReason
	Fresh    bool
}

// Error returns the lookup diagnostic.
//
// Request lookup reasons and refresh failures extend the selector message.
//
// Example:
//
//	conversation "missing-id" not found
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
//
// The message preserves the selector and omits lookup diagnostics.
//
// Example:
//
//	Conversation missing-id not found.
func (err *NotFoundError) UserMessage() string {
	return fmt.Sprintf("Conversation %s not found.", err.Selector)
}
