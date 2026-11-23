// Package replay stores provider metadata for individual Cursor replies.
package replay

import (
	"encoding/base64"
	"strings"

	"github.com/google/uuid"
)

const (
	markerSeparator = "\n\n"
	markerPrefix    = "<!-- clyde-replay:v1:"
	markerSuffix    = " -->"
)

// Reference identifies one reply in one conversation.
type Reference struct {
	ConversationID string
	ReplyID        string
}

// NewID generates an independent identifier for a conversation or reply.
//
// Use a generated conversation ID when Cursor omits its native ID.
//
//	conversationID := replay.NewID()
func NewID() string {
	return uuid.NewString()
}

// Marker returns a trailing HTML comment that Cursor can echo on a later turn.
//
// The Base64 alphabet excludes hyphens, so an ID cannot close the HTML comment.
//
//	reply := answer + ref.Marker()
func (ref Reference) Marker() string {
	if ref.ConversationID == "" || ref.ReplyID == "" {
		return ""
	}

	conversation := base64.RawStdEncoding.EncodeToString([]byte(ref.ConversationID))
	reply := base64.RawStdEncoding.EncodeToString([]byte(ref.ReplyID))

	return markerSeparator + markerPrefix + conversation + "." + reply + markerSuffix
}

// StripReference removes one complete reference at the end of the reply.
//
// A malformed or embedded comment remains part of the original text.
//
//	visible, ref, found := replay.StripReference(reply)
func StripReference(text string) (string, Reference, bool) {
	var missing Reference

	trimmed := strings.TrimRight(text, " \t\r\n")
	body, hasSuffix := strings.CutSuffix(trimmed, markerSuffix)
	if !hasSuffix {
		return text, missing, false
	}

	start := strings.LastIndex(body, markerPrefix)
	if start < 0 {
		return text, missing, false
	}

	conversation, reply, hasSeparator := strings.Cut(body[start+len(markerPrefix):], ".")
	if !hasSeparator {
		return text, missing, false
	}

	conversationID, err := base64.RawStdEncoding.Strict().DecodeString(conversation)
	if err != nil {
		return text, missing, false
	}

	replyID, err := base64.RawStdEncoding.Strict().DecodeString(reply)
	if err != nil {
		return text, missing, false
	}

	ref := Reference{ConversationID: string(conversationID), ReplyID: string(replyID)}
	if ref.Marker() != markerSeparator+trimmed[start:] {
		return text, missing, false
	}

	if start == 0 {
		return "", ref, true
	}

	visible, separated := strings.CutSuffix(trimmed[:start], markerSeparator)
	if !separated {
		return text, missing, false
	}

	return visible, ref, true
}
