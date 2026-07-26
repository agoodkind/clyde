package mitmcontrib

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/providerid"
)

// Cursor's agent service carries the chat identity on two routes. Run is the
// chat turn itself and streams as connect frames; UpdateConversationMetadata is
// a small unary write that names its conversation directly.
const (
	agentRunPath          = "/agent.v1.agentservice/run"
	agentMetadataPath     = "/agent.v1.agentservice/updateconversationmetadata"
	connectContentTypeTag = "connect+"
)

// Field numbers on Cursor's agent-service wire messages. There is no generated
// schema for these, so they are recorded from observed traffic: Cursor names
// the value `composerId` in its own OTLP span attributes, and the same uuid is
// the name of the conversation's transcript directory under ~/.cursor.
const (
	// runWrapperField is the single top-level field of a Run request message;
	// the run itself is nested inside it.
	runWrapperField = 1
	// runThreadField carries the run's own thread id. On a top-level chat turn
	// this is the conversation. On a subagent run it is the subagent.
	runThreadField = 5
	// runParentField carries the root conversation and is present only when the
	// run is a subagent run.
	runParentField = 16
	// metadataConversationField carries the conversation id on an
	// UpdateConversationMetadata request.
	metadataConversationField = 1
)

const (
	// connectCompressedFlag is the low bit of a connect envelope's flag byte,
	// set when the frame payload is compressed with the request's declared
	// connect encoding.
	connectCompressedFlag = 0x01
	// connectEnvelopeHeaderBytes is the 1-byte flag plus the 4-byte big-endian
	// payload length that prefix every connect frame.
	connectEnvelopeHeaderBytes = 5
	// maxDecompressedFrameBytes bounds the first-frame gunzip so a hostile or
	// corrupt length cannot make the resolver allocate without limit. Observed
	// Run first frames decompress to roughly 300 KB.
	maxDecompressedFrameBytes = 16 << 20
	// uuidTextLength is the exact length of the canonical uuid text Cursor
	// sends. Requiring the exact length is what keeps a longer id that merely
	// contains a conversation uuid from matching.
	uuidTextLength = 36
)

// ConversationResolver recovers the Cursor chat a captured request belongs to.
// The zero value is ready to use. It satisfies [capture.ConversationRefResolver],
// so one extraction serves both the record-time path, where the MITM proxy
// resolves a request on its way into the store, and the query-time path, where
// a caller re-reads a row captured before the join column existed.
type ConversationResolver struct{}

// ResolveConversation implements [capture.ConversationRefResolver].
func (ConversationResolver) ResolveConversation(input capture.RequestConversationInput) (capture.ConversationRef, bool) {
	headers := input.Headers
	if headers == nil {
		headers = http.Header{}
	}
	if input.ContentType != "" && headers.Get("Content-Type") == "" {
		headers = headers.Clone()
		headers.Set("Content-Type", input.ContentType)
	}
	return ExtractConversationRef(RequestCapture{
		Path:    input.Path,
		Headers: headers,
		Body:    input.Body,
	})
}

// ExtractConversationRef returns the Cursor conversation a captured request
// belongs to, and true when the request is one of the agent-service routes that
// carries a chat identity. It returns the unresolved ref with true when the
// route matches but the body names no usable id, so a caller can tell "this was
// a chat request that named nothing" from "this was not a chat request".
//
// The read is bounded. For a Run request it decodes only the first connect
// frame and only that frame's top-level fields; it never walks the message tree
// and never touches the remaining frames, of which a single turn can carry
// thousands.
func ExtractConversationRef(req RequestCapture) (capture.ConversationRef, bool) {
	switch normalizeAgentPath(req.Path) {
	case agentRunPath:
		return conversationFromRun(req), true
	case agentMetadataPath:
		return conversationFromMetadata(req), true
	default:
		return capture.UnresolvedConversation(), false
	}
}

func normalizeAgentPath(path string) string {
	trimmed := strings.ToLower(strings.TrimSpace(path))
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// conversationFromRun reads the run message's parent conversation when the run
// is a subagent run, and the run's own thread otherwise. Clyde does not index
// subagent transcripts as conversations, so on a subagent run the parent is the
// only id that names something the conversation index holds.
func conversationFromRun(req RequestCapture) capture.ConversationRef {
	message, ok := firstConnectMessage(req)
	if !ok {
		return capture.UnresolvedConversation()
	}
	runMessage, ok := lengthDelimitedField(message, runWrapperField)
	if !ok {
		return capture.UnresolvedConversation()
	}
	if parent, ok := uuidField(runMessage, runParentField); ok {
		return cursorConversationRef(parent, capture.ConversationSourceCursorRunParent)
	}
	if thread, ok := uuidField(runMessage, runThreadField); ok {
		return cursorConversationRef(thread, capture.ConversationSourceCursorRunThread)
	}
	return capture.UnresolvedConversation()
}

func conversationFromMetadata(req RequestCapture) capture.ConversationRef {
	body, ok := firstConnectMessage(req)
	if !ok {
		return capture.UnresolvedConversation()
	}
	conversationID, ok := uuidField(body, metadataConversationField)
	if !ok {
		return capture.UnresolvedConversation()
	}
	return cursorConversationRef(conversationID, capture.ConversationSourceCursorMetadata)
}

func cursorConversationRef(nativeID string, source capture.ConversationSource) capture.ConversationRef {
	return capture.ConversationRef{
		ConversationID: conversation.DerivedID(providerid.ProviderCursor, nativeID, ""),
		Source:         source,
	}
}

func isConnectStreaming(headers http.Header) bool {
	if headers == nil {
		return false
	}
	return strings.Contains(strings.ToLower(headers.Get("Content-Type")), connectContentTypeTag)
}

// firstConnectMessage returns the first protobuf message in a request body. A
// connect streaming body wraps each message in a flag-and-length envelope and
// may compress it; a unary body is the message itself. Extraction is
// best-effort diagnostics, so an unreadable body reports false rather than
// raising an error the caller would only discard.
func firstConnectMessage(req RequestCapture) ([]byte, bool) {
	if !isConnectStreaming(req.Headers) {
		return req.Body, true
	}
	if len(req.Body) < connectEnvelopeHeaderBytes {
		return nil, false
	}
	flag := req.Body[0]
	remaining := req.Body[connectEnvelopeHeaderBytes:]
	// A stored body is truncated at the capture store's per-body cap, so a
	// frame longer than what was retained is expected rather than corrupt.
	// Read what is there.
	declaredLength := int64(binary.BigEndian.Uint32(req.Body[1:connectEnvelopeHeaderBytes]))
	readableLength := min(declaredLength, int64(len(remaining)))
	payload := remaining[:readableLength]
	if flag&connectCompressedFlag == 0 {
		return payload, true
	}
	return gunzipBounded(payload)
}

// gunzipBounded decompresses one connect frame, stopping at
// maxDecompressedFrameBytes so a corrupt or hostile stream cannot make the
// resolver allocate without limit. A truncated stream that still yielded bytes
// counts as success, since the fields being read sit at the start of the
// message.
func gunzipBounded(payload []byte) ([]byte, bool) {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, false
	}
	defer func() { _ = reader.Close() }()
	decompressed, err := io.ReadAll(io.LimitReader(reader, maxDecompressedFrameBytes))
	if err != nil && len(decompressed) == 0 {
		return nil, false
	}
	return decompressed, true
}

// lengthDelimitedField returns the bytes of the first length-delimited field
// with the given number, scanning only the message's own top level.
func lengthDelimitedField(message []byte, fieldNumber uint64) ([]byte, bool) {
	for offset := 0; offset < len(message); {
		number, wireType, next, ok := readProtoKey(message, offset)
		if !ok {
			return nil, false
		}
		value, afterValue, ok := skipOrReadProtoValue(message, next, wireType)
		if !ok {
			return nil, false
		}
		if number == fieldNumber && wireType == protoWireBytes {
			return value, true
		}
		offset = afterValue
	}
	return nil, false
}

// uuidField returns the value of the first length-delimited field with the
// given number whose bytes are exactly one canonical uuid. A field holding a
// longer string that merely contains a uuid does not match.
func uuidField(message []byte, fieldNumber uint64) (string, bool) {
	value, ok := lengthDelimitedField(message, fieldNumber)
	if !ok {
		return "", false
	}
	if !isCanonicalUUID(value) {
		return "", false
	}
	return string(value), true
}

const (
	protoWireVarint  uint64 = 0
	protoWireFixed64 uint64 = 1
	protoWireBytes   uint64 = 2
	protoWireFixed32 uint64 = 5
)

// skipOrReadProtoValue advances past one field value, returning the value bytes
// for length-delimited fields and nil for the rest.
func skipOrReadProtoValue(message []byte, offset int, wireType uint64) ([]byte, int, bool) {
	switch wireType {
	case protoWireVarint:
		_, next, ok := readProtoVarint(message, offset)
		return nil, next, ok
	case protoWireFixed64:
		if offset+8 > len(message) {
			return nil, offset, false
		}
		return nil, offset + 8, true
	case protoWireBytes:
		value, next, ok := readProtoBytes(message, offset)
		return value, next, ok
	case protoWireFixed32:
		if offset+4 > len(message) {
			return nil, offset, false
		}
		return nil, offset + 4, true
	default:
		return nil, offset, false
	}
}

func isCanonicalUUID(value []byte) bool {
	if len(value) != uuidTextLength {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !isHexDigit(char) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(char byte) bool {
	switch {
	case char >= '0' && char <= '9':
		return true
	case char >= 'a' && char <= 'f':
		return true
	case char >= 'A' && char <= 'F':
		return true
	default:
		return false
	}
}
