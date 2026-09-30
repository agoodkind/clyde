package searchacceptance

import (
	"errors"
	"fmt"
	"strings"

	"goodkind.io/clyde/internal/conversation/searchbackend"
)

// SourceIdentity identifies a selected source span without a storage row key.
type SourceIdentity struct {
	ConversationID  string `json:"conversation_id"`
	MessageIndex    int    `json:"message_index"`
	ContentKind     string `json:"content_kind"`
	ToolIndex       int    `json:"tool_index"`
	SourceByteStart int64  `json:"source_byte_start"`
	SourceByteEnd   int64  `json:"source_byte_end"`
}

// IdentityKey normalizes a public source identity for a frozen manifest.
func IdentityKey(identity SourceIdentity) (string, error) {
	provider, nativeID, found := strings.Cut(identity.ConversationID, ":")
	if !found || provider == "" || nativeID == "" || identity.MessageIndex < 0 ||
		identity.SourceByteStart < 0 || identity.SourceByteEnd <= identity.SourceByteStart {
		return "", errors.New("source identity requires a provider conversation, message index, and nonempty byte span")
	}
	switch searchbackend.FieldKind(identity.ContentKind) {
	case searchbackend.FieldKindChat, searchbackend.FieldKindThinking:
		if identity.ToolIndex != -1 {
			return "", errors.New("message source identity requires tool index -1")
		}
	case searchbackend.FieldKindToolName, searchbackend.FieldKindToolCall, searchbackend.FieldKindToolOutput:
		if identity.ToolIndex < 0 {
			return "", errors.New("tool source identity requires a nonnegative tool index")
		}
	default:
		return "", fmt.Errorf("unknown source content kind %q", identity.ContentKind)
	}
	return fmt.Sprintf("%q/m%d/%s/t%d/%d:%d", identity.ConversationID, identity.MessageIndex,
		identity.ContentKind, identity.ToolIndex, identity.SourceByteStart, identity.SourceByteEnd), nil
}
