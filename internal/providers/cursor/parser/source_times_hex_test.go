package parser_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/providers/cursor/parser"
)

// writeHexNativeStore writes each native message as hexadecimal text under the
// SHA-256 digest of referenceSuffix appended to its JSON bytes.
func writeHexNativeStore(
	t *testing.T,
	rootDir string,
	messages []fixtureNativeMessage,
	bubbles []fixtureBubble,
	referenceSuffix string,
) {
	t.Helper()
	rows := make([]kvRow, 0, len(messages)+len(bubbles)+1)
	var state []byte

	for _, message := range messages {
		data := mustMarshal(t)(json.Marshal(message))
		digest := sha256.Sum256(append(append([]byte{}, data...), referenceSuffix...))
		state = append(state, fixtureFieldReference, byte(len(digest)))
		state = append(state, digest[:]...)
		rows = append(
			rows,
			kvRow{
				key:  "agentKv:blob:" + hex.EncodeToString(digest[:]),
				text: hex.EncodeToString(data),
				blob: nil,
			},
		)
	}

	composer := fixtureComposer{
		ComposerID:        nativeComposerID,
		Name:              "Fixture",
		CreatedAt:         fixtureComposerCreated,
		LastUpdatedAt:     fixtureComposerUpdated,
		ConversationState: fixtureStatePrefix + base64.StdEncoding.EncodeToString(state),
		Headers:           make([]fixtureHeaderRef, 0, len(bubbles)),
	}
	for _, bubble := range bubbles {
		composer.Headers = append(
			composer.Headers,
			fixtureHeaderRef{BubbleID: bubble.BubbleID, Type: bubble.Type},
		)
		rows = append(
			rows,
			kvRow{
				key:  "bubbleId:" + nativeComposerID + ":" + bubble.BubbleID,
				text: string(mustMarshal(t)(json.Marshal(bubble))),
				blob: nil,
			},
		)
	}

	rows = append(
		rows,
		kvRow{
			key:  "composerData:" + nativeComposerID,
			text: string(mustMarshal(t)(json.Marshal(composer))),
			blob: nil,
		},
	)
	writeKVStore(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), rows)
}

func hexFixtureRecord() conversation.Record {
	return conversation.Record{
		ID:              "cursor:" + nativeComposerID,
		Provider:        providerid.ProviderCursor,
		NativeID:        nativeComposerID,
		Selector:        "",
		Lineage:         nil,
		Origin:          conversation.OriginUnspecified,
		Title:           "",
		TitleUncertain:  false,
		WorkspaceRoot:   "",
		ArtifactPath:    "",
		ArtifactKind:    "",
		Model:           "",
		CreatedAt:       time.Time{},
		UpdatedAt:       time.Time{},
		SizeBytes:       0,
		Archived:        false,
		LatestRequestID: "",
	}
}

// TestSourceStoreMessagesDecodesHexNativeRecords checks that source reads decode hexadecimal native records and match the tool call ID to its bubble timestamp.
func TestSourceStoreMessagesDecodesHexNativeRecords(t *testing.T) {
	rootDir, _ := boundaryRoots(t, []string{userLine(t, boundaryFirstRequest)})
	writeHexNativeStore(
		t,
		rootDir,
		[]fixtureNativeMessage{
			userMessage(boundaryFirstRequest),
			assistantMessage(callPart("call_a", commandArgs(t, nativeGitCommand, ""))),
		},
		boundaryBubbles()[:2],
		"",
	)
	source, found, err := parser.New().SourceStoreMessages(
		t.Context(),
		hexFixtureRecord(),
		sourceRootLoadOptions(),
	)
	if err != nil || !found {
		t.Fatalf("cannot read hexadecimal native records; found, error: %v, %v", found, err)
	}

	if !source.ToolCallsComplete || source.MissingNativeRecords != 0 || len(source.ToolCalls) != 1 {
		t.Fatalf(
			"decoded native state differs; complete, missing records, tool calls: %v, %d, %d",
			source.ToolCallsComplete,
			source.MissingNativeRecords,
			len(source.ToolCalls),
		)
	}

	call := source.ToolCalls[0]
	if call.Tool.ID != "call_a" || !call.Timestamp.Equal(minuteAfterTen(1)) {
		t.Fatalf(
			"decoded tool call differs from expected; call ID, timestamp: %q, %v",
			call.Tool.ID,
			call.Timestamp,
		)
	}
}

// TestSourceStoreMessagesRejectsHexTextWithAnotherDigest checks that source reads reject hexadecimal text when its decoded bytes do not match the SHA-256 reference.
func TestSourceStoreMessagesRejectsHexTextWithAnotherDigest(t *testing.T) {
	rootDir, _ := boundaryRoots(t, []string{userLine(t, boundaryFirstRequest)})
	writeHexNativeStore(
		t,
		rootDir,
		[]fixtureNativeMessage{userMessage(boundaryFirstRequest)},
		boundaryBubbles()[:1],
		"other",
	)
	if _, found, err := parser.New().SourceStoreMessages(
		t.Context(),
		hexFixtureRecord(),
		sourceRootLoadOptions(),
	); err == nil {
		t.Fatalf(
			"source store did not reject hexadecimal text with a mismatched SHA-256 reference; found: %v",
			found,
		)
	}
}
