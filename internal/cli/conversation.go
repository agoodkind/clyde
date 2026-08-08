package cli

const (
	// ConversationGroupName is the terminal parent for conversation commands.
	ConversationGroupName = "conversation"
	// ConversationSearchName is the terminal conversation search command.
	ConversationSearchName = "search"
)

// ConversationBrowseCommand returns the canonical terminal command that
// browses conversation metadata.
func ConversationBrowseCommand() string {
	return "clyde " + ConversationGroupName + " " + ConversationSearchName
}
