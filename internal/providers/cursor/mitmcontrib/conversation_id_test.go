package mitmcontrib

import (
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

const testConversationID = "2e26c0f6-c33d-4b54-93a4-96c94b0b7b11"

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
		name string
		path string
		body []byte
	}{
		{
			name: "update conversation metadata",
			path: agentUpdateConversationMetadataPath,
			body: updateConversationMetadataBody(testConversationID),
		},
		{
			name: "agent run",
			path: agentRunPath,
			body: agentRunBody(testConversationID),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, ok := routeProvider{}.ConversationIDFromBody(exchangeFor(test.path, test.body))
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

	full := updateConversationMetadataBody(testConversationID)
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
			name: "truncated body",
			path: agentUpdateConversationMetadataPath,
			body: full[:len(full)/2],
		},
		{
			name: "empty body",
			path: agentRunPath,
			body: nil,
		},
		{
			name: "field holds a non-id string",
			path: agentUpdateConversationMetadataPath,
			body: updateConversationMetadataBody("not-a-conversation-id"),
		},
		{
			name: "run wrapper without the id field",
			path: agentRunPath,
			body: protoBytes(1, protoBytes(2, []byte("payload"))),
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
