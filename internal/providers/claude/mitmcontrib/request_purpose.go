package mitmcontrib

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

// requestClassHeader names the header Claude Code sets on every request it
// issues. The value states which kind of turn produced the request.
const requestClassHeader = "x-claude-code-request-class"

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
