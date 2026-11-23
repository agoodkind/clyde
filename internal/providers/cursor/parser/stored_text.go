package parser

import (
	"context"

	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
)

var _ conversation.StoredTextNormalizer = (*Parser)(nil)

// NormalizeStoredText projects stored Cursor rows onto transcript text.
//
// Source time matching uses this projection to compare a stored row with its
// transcript message. Both reads apply the same rules.
//
//   - User rows lose Cursor's prompt envelope.
//   - Assistant rows lose the replay reference and verified generated spans.
func (*Parser) NormalizeStoredText(role string, text string) string {
	switch role {
	case cursorjsonl.RoleUser:
		unwrapped, _ := cursorjsonl.UnwrapUserEnvelope(text)

		return unwrapped
	case cursorjsonl.RoleAssistant:
		visible, _ := projectAssistantText(context.Background(), text, "")

		return visible
	default:
		return text
	}
}
