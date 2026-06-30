package cursorstore

import (
	"errors"
	"testing"
)

func TestDecodeComposerHeaderJSONDecodesTypedHeader(t *testing.T) {
	header, err := DecodeComposerHeaderJSON([]byte(`{"composerId":"composer-a","name":"Investigate Cursor","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"status":"none","unifiedMode":"agent","forceMode":"","fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":1}]}`))
	if err != nil {
		t.Fatalf("DecodeComposerHeaderJSON returned error: %v", err)
	}
	if header.ComposerID != "composer-a" {
		t.Fatalf("ComposerID = %q, want composer-a", header.ComposerID)
	}
	if header.Name != "Investigate Cursor" {
		t.Fatalf("Name = %q, want Investigate Cursor", header.Name)
	}
	if header.CreatedAt != 1710000000000 {
		t.Fatalf("CreatedAt = %d", header.CreatedAt)
	}
	if header.LastUpdatedAt != 1710000000100 {
		t.Fatalf("LastUpdatedAt = %d", header.LastUpdatedAt)
	}
	if header.UnifiedMode != "agent" {
		t.Fatalf("UnifiedMode = %q", header.UnifiedMode)
	}
	if len(header.FullConversationHeadersOnly) != 1 {
		t.Fatalf("FullConversationHeadersOnly len = %d, want 1", len(header.FullConversationHeadersOnly))
	}
	if header.FullConversationHeadersOnly[0].BubbleID != "bubble-1" {
		t.Fatalf("BubbleID = %q", header.FullConversationHeadersOnly[0].BubbleID)
	}
}

func TestDecodeComposerHeaderJSONWrapsInvalidJSON(t *testing.T) {
	_, err := DecodeComposerHeaderJSON([]byte(`{"composerId":`))
	if err == nil {
		t.Fatal("DecodeComposerHeaderJSON returned nil error, want decode error")
	}
	var typedErr CursorJSONDecodeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %T, want CursorJSONDecodeError", err)
	}
}
