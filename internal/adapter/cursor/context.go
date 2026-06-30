package cursor

import "strings"

// Context is part of Clyde's typed adapter surface.
type Context struct {
	User           string
	RequestID      string
	ConversationID string
	WorkspacePath  string
}

// StrongConversationKey is part of Clyde's typed adapter surface.
func (c Context) StrongConversationKey() string {
	if strings.TrimSpace(c.ConversationID) == "" {
		return ""
	}
	return "cursor:" + strings.TrimSpace(c.ConversationID)
}
