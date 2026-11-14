package mitmcontrib

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/providers/claude/compaction"
)

// requestClassHeader names the header Claude Code sets on every request it
// issues. The value states which kind of turn produced the request.
const requestClassHeader = "X-Claude-Code-Request-Class"

// requestClassCompaction is the value Claude Code sets on its
// conversation-summarization turn. A manual `/compact` and an automatic
// compaction both set it.
const requestClassCompaction = "compaction"

// classifyRequestPurpose converts the request-class header into a purpose. An
// unrecognized or absent class returns [mitm.RequestPurposeUnspecified].
func classifyRequestPurpose(headers http.Header) mitm.RequestPurpose {
	class := strings.ToLower(strings.TrimSpace(firstHeader(headers, requestClassHeader)))
	if class == requestClassCompaction {
		return mitm.RequestPurposeCompaction
	}

	return mitm.RequestPurposeUnspecified
}

// ClassifyRequestBodyPurpose consumes a matching compaction declaration.
//
// An explicit compaction consumes its declaration after session and prompt
// checks. Headerless plain HTTP compaction requires that declaration.
//
// Example:
//
//	purpose := provider.ClassifyRequestBodyPurpose(request)
func (p routeProvider) ClassifyRequestBodyPurpose(
	request mitm.RequestResponseHookRequest,
) mitm.RequestPurpose {
	if p.declarations == nil || request.Method != http.MethodPost ||
		request.Path != "/v1/messages" ||
		request.Body == nil {
		return mitm.RequestPurposeUnspecified
	}

	if request.Purpose != mitm.RequestPurposeCompaction &&
		(!request.PlainHTTP || len(request.Header.Values(requestClassHeader)) != 0) {
		return mitm.RequestPurposeUnspecified
	}

	sessionID := extractIdentity(request.Header).SessionID
	if sessionID == "" {
		return mitm.RequestPurposeUnspecified
	}

	conversationID := conversation.DerivedID(providerid.ProviderClaude, sessionID, "")
	if !p.declarations.Pending(conversationID) {
		return mitm.RequestPurposeUnspecified
	}

	body, err := request.Body.Bytes()
	if err != nil {
		return mitm.RequestPurposeUnspecified
	}

	parsed, ok := compaction.NewProvider().ParseCompaction(body)
	if !ok {
		return mitm.RequestPurposeUnspecified
	}

	if parsed.SessionID != "" && sessionID != parsed.SessionID {
		return mitm.RequestPurposeUnspecified
	}

	if !p.declarations.Consume(conversationID) {
		return mitm.RequestPurposeUnspecified
	}

	return mitm.RequestPurposeCompaction
}
