package mitmcontrib

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/mitm/capture"
)

// The uuids below stand in for real values. They are shaped like the ones
// Cursor sends (a bare canonical uuid, no prefix) but carry no content and name
// nothing on this machine.
const (
	testConversationUUID = "11111111-2222-4333-8444-555555555555"
	testSubagentUUID     = "99999999-8888-4777-8666-555555555555"
	testUnknownChatUUID  = "abcdefab-cdef-4abc-8def-abcdefabcdef"
)

// encodeProtoBytes appends one length-delimited protobuf field.
func encodeProtoBytes(dst []byte, fieldNumber uint64, value []byte) []byte {
	dst = binary.AppendUvarint(dst, fieldNumber<<3|2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

// encodeProtoVarint appends one varint protobuf field, so a fixture can carry
// the non-string fields that sit between the ones under test.
func encodeProtoVarint(dst []byte, fieldNumber uint64, value uint64) []byte {
	dst = binary.AppendUvarint(dst, fieldNumber<<3|0)
	return binary.AppendUvarint(dst, value)
}

// gzipConnectFrame wraps one protobuf message in the compressed connect
// envelope Cursor uses for a Run request: a flag byte with the low bit set,
// then a four-byte big-endian payload length.
func gzipConnectFrame(t *testing.T, message []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(message); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	frame := []byte{0x01}
	frame = binary.BigEndian.AppendUint32(frame, uint32(compressed.Len()))
	return append(frame, compressed.Bytes()...)
}

// runRequestBody builds a Run request body. threadID is the run's own thread
// (field 5) and parentID, when non-empty, is the parent conversation (field 16)
// that a subagent run carries. Trailing frames are appended so the fixture has
// the multi-frame shape of a real turn and the extractor is shown to read only
// the first.
func runRequestBody(t *testing.T, threadID, parentID string) []byte {
	t.Helper()
	var run []byte
	run = encodeProtoVarint(run, 2, 7)
	run = encodeProtoBytes(run, runThreadField, []byte(threadID))
	if parentID != "" {
		run = encodeProtoBytes(run, runParentField, []byte(parentID))
	}
	var message []byte
	message = encodeProtoBytes(message, runWrapperField, run)

	body := gzipConnectFrame(t, message)
	var trailing []byte
	trailing = encodeProtoBytes(trailing, 2, []byte("trailing frame payload"))
	return append(body, gzipConnectFrame(t, trailing)...)
}

func runRequest(t *testing.T, threadID, parentID string) RequestCapture {
	t.Helper()
	return RequestCapture{
		Path:    "/agent.v1.AgentService/Run",
		Headers: http.Header{"Content-Type": []string{"application/connect+proto"}},
		Body:    runRequestBody(t, threadID, parentID),
	}
}

func metadataRequest(conversationID string) RequestCapture {
	var body []byte
	body = encodeProtoBytes(body, metadataConversationField, []byte(conversationID))
	body = encodeProtoVarint(body, 3, 1)
	return RequestCapture{
		Path:    "/agent.v1.AgentService/UpdateConversationMetadata",
		Headers: http.Header{"Content-Type": []string{"application/proto"}},
		Body:    body,
	}
}

func TestRunRequestResolvesToItsOwnThreadConversation(t *testing.T) {
	ref, supported := ExtractConversationRef(runRequest(t, testConversationUUID, ""))

	if !supported {
		t.Fatal("Run request reported as outside the Cursor wire contract")
	}
	if ref.ConversationID != "cursor:"+testConversationUUID {
		t.Fatalf("ConversationID = %q, want %q", ref.ConversationID, "cursor:"+testConversationUUID)
	}
	if ref.Source != capture.ConversationSourceCursorRunThread {
		t.Fatalf("Source = %q, want %q", ref.Source, capture.ConversationSourceCursorRunThread)
	}
}

// A subagent run carries the subagent in field 5 and the conversation in field
// 16. Clyde indexes no subagent as a conversation, so tagging the row with the
// field 5 value would attach it to an id that names nothing. The parent must
// win.
func TestSubagentRunResolvesToParentConversationNotSubagent(t *testing.T) {
	ref, supported := ExtractConversationRef(runRequest(t, testSubagentUUID, testConversationUUID))

	if !supported {
		t.Fatal("subagent Run request reported as outside the Cursor wire contract")
	}
	if ref.ConversationID != "cursor:"+testConversationUUID {
		t.Fatalf("ConversationID = %q, want the parent %q", ref.ConversationID, "cursor:"+testConversationUUID)
	}
	if ref.ConversationID == "cursor:"+testSubagentUUID {
		t.Fatal("subagent run resolved to the subagent thread instead of its parent conversation")
	}
	if ref.Source != capture.ConversationSourceCursorRunParent {
		t.Fatalf("Source = %q, want %q", ref.Source, capture.ConversationSourceCursorRunParent)
	}
}

// A metadata write for a chat this machine has no transcript for still resolves
// to that chat's own id and never to another chat. The id simply names nothing
// in the conversation index, which is a miss rather than a wrong attribution.
func TestMetadataForChatWithNoLocalTranscriptResolvesToItsOwnIDOnly(t *testing.T) {
	ref, supported := ExtractConversationRef(metadataRequest(testUnknownChatUUID))

	if !supported {
		t.Fatal("metadata request reported as outside the Cursor wire contract")
	}
	if ref.ConversationID != "cursor:"+testUnknownChatUUID {
		t.Fatalf("ConversationID = %q, want %q", ref.ConversationID, "cursor:"+testUnknownChatUUID)
	}
	if ref.ConversationID == "cursor:"+testConversationUUID {
		t.Fatal("unknown chat was attributed to a different conversation")
	}
	if ref.Source != capture.ConversationSourceCursorMetadata {
		t.Fatalf("Source = %q, want %q", ref.Source, capture.ConversationSourceCursorMetadata)
	}
}

// A field holding a longer id that merely contains a conversation uuid is a
// different value, and must not be read as that conversation.
func TestIdentifierContainingAConversationUUIDDoesNotMatch(t *testing.T) {
	embedded := []string{
		"composer-" + testConversationUUID,
		testConversationUUID + "-retry",
		testConversationUUID + "0",
	}
	for _, value := range embedded {
		ref, supported := ExtractConversationRef(metadataRequest(value))
		if !supported {
			t.Fatalf("metadata request with %q reported as outside the Cursor wire contract", value)
		}
		if ref.Resolved() {
			t.Fatalf("value %q resolved to %q, want no conversation", value, ref.ConversationID)
		}
	}
}

// A Run whose thread field holds a longer id is the same rejection on the
// streaming route.
func TestRunThreadContainingAConversationUUIDDoesNotMatch(t *testing.T) {
	ref, supported := ExtractConversationRef(runRequest(t, testConversationUUID+"-shadow", ""))

	if !supported {
		t.Fatal("Run request reported as outside the Cursor wire contract")
	}
	if ref.Resolved() {
		t.Fatalf("embedded thread id resolved to %q, want no conversation", ref.ConversationID)
	}
}

// Cursor's telemetry carries the same composerId values on high-volume routes.
// Those are not chat requests, so the resolver must decline them outright
// rather than tag them with a conversation.
func TestTelemetryRouteIsNotAChatRequest(t *testing.T) {
	telemetryPaths := []string{
		"/aiserver.v1.AnalyticsService/Batch",
		"/v1/traces",
		"/aiserver.v1.AiService/CppAppend",
	}
	for _, path := range telemetryPaths {
		request := metadataRequest(testConversationUUID)
		request.Path = path
		ref, supported := ExtractConversationRef(request)
		if supported {
			t.Fatalf("path %q was treated as a chat request", path)
		}
		if ref.Resolved() {
			t.Fatalf("path %q resolved to %q, want no conversation", path, ref.ConversationID)
		}
	}
}

func TestChatRouteWithNoIdentifierResolvesToNothing(t *testing.T) {
	var run []byte
	run = encodeProtoVarint(run, 2, 7)
	var message []byte
	message = encodeProtoBytes(message, runWrapperField, run)
	request := RequestCapture{
		Path:    "/agent.v1.AgentService/Run",
		Headers: http.Header{"Content-Type": []string{"application/connect+proto"}},
		Body:    gzipConnectFrame(t, message),
	}

	ref, supported := ExtractConversationRef(request)

	if !supported {
		t.Fatal("Run request reported as outside the Cursor wire contract")
	}
	if ref.Resolved() {
		t.Fatalf("resolved to %q, want no conversation", ref.ConversationID)
	}
}

func TestTruncatedRunBodyResolvesToNothing(t *testing.T) {
	full := runRequestBody(t, testConversationUUID, "")
	request := RequestCapture{
		Path:    "/agent.v1.AgentService/Run",
		Headers: http.Header{"Content-Type": []string{"application/connect+proto"}},
		Body:    full[:3],
	}

	ref, supported := ExtractConversationRef(request)

	if !supported {
		t.Fatal("Run request reported as outside the Cursor wire contract")
	}
	if ref.Resolved() {
		t.Fatalf("resolved to %q, want no conversation", ref.ConversationID)
	}
}

// The record-time path and the query-time path must agree, since a row read
// back out of the store is resolved by the same extractor that tagged it going
// in.
func TestStoredInputResolvesTheSameAsTheLiveRequest(t *testing.T) {
	live := runRequest(t, testSubagentUUID, testConversationUUID)
	liveRef, liveSupported := ExtractConversationRef(live)

	storedRef, storedSupported := ConversationResolver{}.ResolveConversation(capture.RequestConversationInput{
		Path:        live.Path,
		ContentType: "application/connect+proto",
		Headers:     nil,
		Body:        live.Body,
	})

	if liveSupported != storedSupported {
		t.Fatalf("supported live=%v stored=%v", liveSupported, storedSupported)
	}
	if liveRef != storedRef {
		t.Fatalf("stored ref %+v does not match live ref %+v", storedRef, liveRef)
	}
	if storedRef.ConversationID != "cursor:"+testConversationUUID {
		t.Fatalf("ConversationID = %q, want %q", storedRef.ConversationID, "cursor:"+testConversationUUID)
	}
}
