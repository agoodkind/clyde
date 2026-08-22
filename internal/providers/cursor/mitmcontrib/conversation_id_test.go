package mitmcontrib

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

const testConversationID = "2e26c0f6-c33d-4b54-93a4-96c94b0b7b11"

// connectFrame wraps a message the way Cursor's agent routes do: one flag byte,
// a four-byte big-endian length, then the message, gzipped when compressed is
// set. Live traffic is always compressed; the uncompressed form is covered so a
// future variant still parses.
func connectFrame(t *testing.T, message []byte, compressed bool) []byte {
	t.Helper()
	payload := message
	flag := byte(0)
	if compressed {
		flag = 1
		var buf bytes.Buffer
		writer := gzip.NewWriter(&buf)
		if _, err := writer.Write(message); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		payload = buf.Bytes()
	}
	framed := make([]byte, 5, 5+len(payload))
	framed[0] = flag
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(payload)))
	return append(framed, payload...)
}

// protoBytes encodes one length-delimited protobuf field. Field numbers here
// stay under 16 so the tag is a single byte, which every fixture below relies on.
func protoBytes(fieldNumber byte, payload []byte) []byte {
	out := []byte{fieldNumber<<3 | 2, byte(len(payload))}
	return append(out, payload...)
}

// updateConversationMetadataBody mirrors the live wire shape: the conversation
// id is the top-level field 1, followed by the title and workspace root.
func updateConversationMetadataBody(id string) []byte {
	body := protoBytes(1, []byte(id))
	body = append(body, protoBytes(2, []byte("a title"))...)
	return append(body, protoBytes(4, []byte("/repo"))...)
}

// agentRunBody mirrors the live wire shape: field 1 wraps the request and
// carries the conversation id at field 5 inside it.
func agentRunBody(id string) []byte {
	inner := protoBytes(2, []byte("payload"))
	inner = append(inner, protoBytes(5, []byte(id))...)
	return protoBytes(1, inner)
}

func exchangeFor(path string, body []byte) mitm.ExchangeDiagnostic {
	return mitm.ExchangeDiagnostic{
		RequestHeader:      nil,
		DecodedRequestBody: body,
		Method:             "POST",
		Path:               path,
		Host:               "api2.cursor.sh",
		Concern:            "providers.mitm.wire",
		HookName:           "",
	}
}

func TestConversationIDFromBodyReadsBothAgentRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		message    []byte
		compressed bool
	}{
		{
			name:       "update conversation metadata, gzipped as sent",
			path:       agentUpdateConversationMetadataPath,
			message:    updateConversationMetadataBody(testConversationID),
			compressed: true,
		},
		{
			name:       "agent run, gzipped as sent",
			path:       agentRunPath,
			message:    agentRunBody(testConversationID),
			compressed: true,
		},
		{
			name:       "agent run, uncompressed frame",
			path:       agentRunPath,
			message:    agentRunBody(testConversationID),
			compressed: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			body := connectFrame(t, test.message, test.compressed)
			got, ok := routeProvider{}.ConversationIDFromBody(exchangeFor(test.path, body))
			if !ok {
				t.Fatalf("ConversationIDFromBody returned ok=false")
			}
			if got != testConversationID {
				t.Fatalf("conversation id = %q, want %q", got, testConversationID)
			}
		})
	}
}

// TestConversationIDFromBodyRejectsNonConversationInput pins the cases that
// must yield nothing rather than a wrong id: a route that carries no
// conversation, a truncated body, and a field holding something that is not an
// id. A wrong conversation id is worse than none, because it would attribute
// captured traffic to another conversation.
func TestConversationIDFromBodyRejectsNonConversationInput(t *testing.T) {
	t.Parallel()

	full := connectFrame(t, updateConversationMetadataBody(testConversationID), true)
	tests := []struct {
		name string
		path string
		body []byte
	}{
		{
			name: "unrelated route",
			path: "/aiserver.v1.AnalyticsService/Batch",
			body: full,
		},
		{
			name: "truncated frame",
			path: agentUpdateConversationMetadataPath,
			body: full[:len(full)/2],
		},
		{
			name: "empty body",
			path: agentRunPath,
			body: nil,
		},
		{
			name: "frame header only",
			path: agentRunPath,
			body: full[:5],
		},
		{
			name: "compressed flag set on bytes that are not gzip",
			path: agentRunPath,
			body: append([]byte{1, 0, 0, 0, 4}, []byte("junk")...),
		},
		{
			name: "field holds a non-id string",
			path: agentUpdateConversationMetadataPath,
			body: connectFrame(t, updateConversationMetadataBody("not-a-conversation-id"), true),
		},
		{
			name: "run wrapper without the id field",
			path: agentRunPath,
			body: connectFrame(t, protoBytes(1, protoBytes(2, []byte("payload"))), true),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, ok := routeProvider{}.ConversationIDFromBody(exchangeFor(test.path, test.body))
			if ok || got != "" {
				t.Fatalf("ConversationIDFromBody = (%q, %t), want empty and false", got, ok)
			}
		})
	}
}

func TestLooksLikeConversationID(t *testing.T) {
	t.Parallel()

	valid := []string{
		testConversationID,
		"A3287144-5A8E-4FA1-A69F-E4E1A8275F85",
	}
	for _, candidate := range valid {
		if !looksLikeConversationID(candidate) {
			t.Fatalf("looksLikeConversationID(%q) = false, want true", candidate)
		}
	}
	invalid := []string{
		"",
		"2e26c0f6c33d4b5493a496c94b0b7b11",
		"2e26c0f6-c33d-4b54-93a4-96c94b0b7b1",
		"2e26c0f6-c33d-4b54-93a4-96c94b0b7b1z",
		"2e26c0f6xc33d-4b54-93a4-96c94b0b7b11",
	}
	for _, candidate := range invalid {
		if looksLikeConversationID(candidate) {
			t.Fatalf("looksLikeConversationID(%q) = true, want false", candidate)
		}
	}
}
