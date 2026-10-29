package parser

import (
	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
)

var _ conversation.StoredTextNormalizer = (*Parser)(nil)

// NormalizeStoredText removes Cursor user and assistant envelopes from stored
// row text using the same rules as the parser.
func (*Parser) NormalizeStoredText(role string, text string) string {
	switch role {
	case cursorjsonl.RoleUser:
		unwrapped, _ := cursorjsonl.UnwrapUserEnvelope(text)
		return unwrapped
	case cursorjsonl.RoleAssistant:
		return stripClydeSyntheticEnvelopes(text)
	default:
		return text
	}
}
