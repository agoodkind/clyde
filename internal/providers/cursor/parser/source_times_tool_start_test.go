package parser_test

import (
	"encoding/base64"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	toolStartCallID = "call_a"
	toolStartCopyID = "aaaaaaaa-0000-4000-8000-00000000000a"
)

type toolCallRecordFixture struct {
	id        string
	shellID   string
	startedAt time.Time
}

func toolCallBinary(record toolCallRecordFixture) string {
	var encoded []byte
	if record.shellID != "" {
		var shellArgs []byte
		shellArgs = protowire.AppendTag(shellArgs, 4, protowire.BytesType)
		shellArgs = protowire.AppendString(shellArgs, record.shellID)
		var shellCall []byte
		shellCall = protowire.AppendTag(shellCall, 1, protowire.BytesType)
		shellCall = protowire.AppendBytes(shellCall, shellArgs)
		encoded = protowire.AppendTag(encoded, 1, protowire.BytesType)
		encoded = protowire.AppendBytes(encoded, shellCall)
	}
	if record.id != "" {
		encoded = protowire.AppendTag(encoded, 57, protowire.BytesType)
		encoded = protowire.AppendString(encoded, record.id)
	}
	if !record.startedAt.IsZero() {
		encoded = protowire.AppendTag(encoded, 59, protowire.VarintType)
		encoded = protowire.AppendVarint(encoded, uint64(record.startedAt.UnixMilli()))
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

func toolBubbleWithRecord(bubbleID string, created string, outerID string, record toolCallRecordFixture) fixtureBubble {
	bubble := toolBubble(bubbleID, created, outerID)
	bubble.ToolFormerData.ToolCallBinary = toolCallBinary(record)
	bubble.Unreferenced = bubbleID != "t1"
	return bubble
}

func TestTranscriptToolCallsPreferApplicableExplicitStarts(t *testing.T) {
	early := time.Date(2026, time.May, 20, 10, 0, 30, 0, time.UTC)
	later := time.Date(2026, time.May, 20, 10, 0, 40, 0, time.UTC)
	original := time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC)
	copied := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	const originalText = "2026-05-20T10:01:00.000Z"
	const copiedText = "2026-05-20T12:00:00.000Z"
	cases := []struct {
		name    string
		bubbles []fixtureBubble
		want    time.Time
	}{
		{
			name: "explicit start on a copy with the typed identity",
			bubbles: []fixtureBubble{
				toolBubble("t1", originalText, toolStartCallID),
				toolBubbleWithRecord("copy", copiedText, toolStartCopyID, toolCallRecordFixture{id: toolStartCallID, shellID: "", startedAt: early}),
			},
			want: early,
		},
		{
			name: "explicit start on a copy with the shell identity",
			bubbles: []fixtureBubble{
				toolBubble("t1", originalText, toolStartCallID),
				toolBubbleWithRecord("copy", copiedText, toolStartCopyID, toolCallRecordFixture{id: "", shellID: toolStartCallID, startedAt: early}),
			},
			want: early,
		},
		{
			name: "original date without an explicit start ignores a typed copy",
			bubbles: []fixtureBubble{
				toolBubble("t1", originalText, toolStartCallID),
				toolBubbleWithRecord("copy", copiedText, toolStartCopyID, toolCallRecordFixture{id: "", shellID: toolStartCallID, startedAt: time.Time{}}),
			},
			want: original,
		},
		{
			name: "typed identity alone with one date",
			bubbles: []fixtureBubble{
				toolBubbleWithRecord("copy", copiedText, toolStartCopyID, toolCallRecordFixture{id: "", shellID: toolStartCallID, startedAt: time.Time{}}),
			},
			want: copied,
		},
		{
			name: "typed identity alone with conflicting dates",
			bubbles: []fixtureBubble{
				toolBubbleWithRecord("copy", copiedText, toolStartCopyID, toolCallRecordFixture{id: "", shellID: toolStartCallID, startedAt: time.Time{}}),
				toolBubbleWithRecord("copy2", originalText, toolStartCopyID+"b", toolCallRecordFixture{id: "", shellID: toolStartCallID, startedAt: time.Time{}}),
			},
			want: time.Time{},
		},
		{
			name: "conflicting explicit starts",
			bubbles: []fixtureBubble{
				toolBubbleWithRecord("t1", originalText, toolStartCallID, toolCallRecordFixture{id: toolStartCallID, shellID: "", startedAt: early}),
				toolBubbleWithRecord("copy", copiedText, toolStartCopyID, toolCallRecordFixture{id: toolStartCallID, shellID: "", startedAt: later}),
			},
			want: time.Time{},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rootDir, transcriptPath := boundaryRoots(t, []string{
				userLine(t, boundaryFirstRequest),
				toolLine(t, commandArgs(t, nativeGitCommand, "")),
			})
			writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
				userMessage(boundaryFirstRequest),
				assistantMessage(callPart(toolStartCallID, commandArgs(t, nativeGitCommand, ""))),
			}, nil, append([]fixtureBubble{userBubble("u1", boundaryFirstRequest, "2026-05-20T10:00:00.000Z")}, testCase.bubbles...))
			requireToolTimes(t, boundaryToolTimes(t, transcriptPath), [][]time.Time{{testCase.want}})
		})
	}
}
