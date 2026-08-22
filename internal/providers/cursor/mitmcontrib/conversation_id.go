package mitmcontrib

import (
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

// Cursor agent routes that carry a conversation id in the request body. Cursor
// sends no conversation header on these, so the body is the only source.
const (
	agentRunPath                        = "/agent.v1.AgentService/Run"
	agentUpdateConversationMetadataPath = "/agent.v1.AgentService/UpdateConversationMetadata"
)

// Protobuf field numbers holding the conversation id on those two routes.
// UpdateConversationMetadata carries it at the top level; Run wraps its request
// in field 1 and carries the id at field 5 inside that wrapper.
const (
	conversationIDTopLevelField = 1
	agentRunRequestField        = 1
	agentRunConversationField   = 5
)

// conversationIDMaxLen bounds how much of a length-delimited field is inspected
// as a candidate id, so a multi-megabyte field is rejected on its length rather
// than copied.
const conversationIDMaxLen = 64

// ConversationIDFromBody implements [mitm.BodyConversationIdentifier]. It
// returns the native Cursor conversation id carried in an agent request body.
//
// Cursor's agent routes send only X-Request-Id, X-Original-Request-Id, and
// X-Session-Id, and that session id is a client-wide value shared with
// telemetry requests, so it cannot stand in for a conversation. The id lives in
// the protobuf body instead.
//
// The scan reads two length-delimited fields at most and never walks the whole
// message, so a large agent request costs the same as a small one.
func (routeProvider) ConversationIDFromBody(exchange mitm.ExchangeDiagnostic) (string, bool) {
	switch strings.TrimSpace(exchange.Path) {
	case agentUpdateConversationMetadataPath:
		return conversationIDField(exchange.DecodedRequestBody, conversationIDTopLevelField)
	case agentRunPath:
		request, ok := protoBytesField(exchange.DecodedRequestBody, agentRunRequestField)
		if !ok {
			return "", false
		}
		return conversationIDField(request, agentRunConversationField)
	default:
		return "", false
	}
}

// conversationIDField returns the named field when it holds a plausible
// conversation id.
func conversationIDField(raw []byte, fieldNumber uint64) (string, bool) {
	value, ok := protoBytesField(raw, fieldNumber)
	if !ok || len(value) > conversationIDMaxLen {
		return "", false
	}
	id := strings.TrimSpace(string(value))
	if !looksLikeConversationID(id) {
		return "", false
	}
	return id, true
}

// protoBytesField returns the first length-delimited field with the given
// number. It walks only the top level of raw and stops at the first parse
// error, so a truncated capture body yields no id rather than a wrong one.
func protoBytesField(raw []byte, fieldNumber uint64) ([]byte, bool) {
	for offset := 0; offset < len(raw); {
		number, wireType, next, ok := readProtoKey(raw, offset)
		if !ok {
			return nil, false
		}
		switch wireType {
		case 0:
			_, nextOffset, valid := readProtoVarint(raw, next)
			if !valid {
				return nil, false
			}
			offset = nextOffset
		case 1:
			if next+8 > len(raw) {
				return nil, false
			}
			offset = next + 8
		case 2:
			value, nextOffset, valid := readProtoBytes(raw, next)
			if !valid {
				return nil, false
			}
			if number == fieldNumber {
				return value, true
			}
			offset = nextOffset
		case 5:
			if next+4 > len(raw) {
				return nil, false
			}
			offset = next + 4
		default:
			return nil, false
		}
	}
	return nil, false
}

// looksLikeConversationID reports whether the candidate is a canonical UUID,
// which is the shape Cursor uses for conversation ids. Requiring the shape
// keeps an unrelated string field from being stored as a conversation id.
func looksLikeConversationID(candidate string) bool {
	const uuidLen = 36
	if len(candidate) != uuidLen {
		return false
	}
	for i := range uuidLen {
		char := candidate[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		isDigit := char >= '0' && char <= '9'
		isLowerHex := char >= 'a' && char <= 'f'
		isUpperHex := char >= 'A' && char <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return false
		}
	}
	return true
}
