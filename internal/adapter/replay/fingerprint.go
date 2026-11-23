package replay

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
	"strings"

	"goodkind.io/clyde/internal/adapter/openai"
)

// TextFingerprint identifies the bytes used to calculate stored span offsets.
//
// Compare this digest before projecting the stored spans onto exported text.
//
//	digest := replay.TextFingerprint(visible)
func TextFingerprint(visible string) string {
	digest := sha256.Sum256([]byte(visible))

	return hex.EncodeToString(digest[:])
}

// Fingerprint identifies the visible message and its ordered function calls.
//
// Cursor need not echo the SSE delta index in its next chat request.
//
//	digest := replay.Fingerprint(visible, calls)
func Fingerprint(visible string, calls []openai.ToolCall) string {
	digest := sha256.New()
	writeField(digest, visible)

	for _, call := range calls {
		writeField(digest, call.ID)
		writeField(digest, call.Function.Name)
		writeField(digest, call.Function.Arguments)
	}

	return hex.EncodeToString(digest.Sum(nil))
}

func writeField(digest hash.Hash, value string) {
	_, _ = digest.Write(binary.AppendUvarint(nil, uint64(len(value))))
	_, _ = io.WriteString(digest, value)
}

// ToolFingerprint identifies the ordered calls in a tool-only Cursor reply.
//
// Every call must include its ID before the reply qualifies for lookup.
//
//	digest := replay.ToolFingerprint(calls)
func ToolFingerprint(calls []openai.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}

	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return ""
		}
	}

	return Fingerprint("", calls)
}
