package conversation_test

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	"goodkind.io/clyde/internal/transcript"
)

func TestCompactionCheckpointsAttachLinkedSummary(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testBoundaryMessage(
			"boundary-1",
			transcript.CompactionKindBoundary,
			[]transcript.CompactedContextItem{testOrdinaryContextItem("boundary context")},
		),
		testSummaryMessage(
			"summary-1",
			"boundary-1",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary context")},
		),
	}

	checkpoints := conversation.CompactionCheckpoints(messages)
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(checkpoints))
	}
	checkpoint := checkpoints[0]
	if checkpoint.BoundaryIndex != 0 || checkpoint.SummaryIndex != 1 {
		t.Fatalf("checkpoint indexes = %#v", checkpoint)
	}
	if checkpoint.BoundaryUUID != "boundary-1" || checkpoint.SummaryUUID != "summary-1" {
		t.Fatalf("checkpoint uuids = %#v", checkpoint)
	}
	if len(checkpoint.ContextItems) != 2 {
		t.Fatalf("context items len = %d, want 2", len(checkpoint.ContextItems))
	}
	if checkpoint.ContextItems[0].Message == nil ||
		checkpoint.ContextItems[0].Message.Content[0].Text != "boundary context" {
		t.Fatalf("boundary context item = %#v", checkpoint.ContextItems[0])
	}
	if checkpoint.ContextItems[1].Message == nil ||
		checkpoint.ContextItems[1].Message.Content[0].Text != "summary context" {
		t.Fatalf("summary context item = %#v", checkpoint.ContextItems[1])
	}
	if !checkpoint.HasUsableCompactedContext() {
		t.Fatalf("HasUsableCompactedContext() = false, want true")
	}
}

func TestCompactionCheckpointsAttachImmediateUnlinkedSummary(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testBoundaryMessage("boundary-1", transcript.CompactionKindBoundary, nil),
		testSummaryMessage(
			"summary-1",
			"",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary context")},
		),
	}

	checkpoints := conversation.CompactionCheckpoints(messages)
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(checkpoints))
	}
	if checkpoints[0].SummaryIndex != 1 {
		t.Fatalf("summary index = %d, want 1", checkpoints[0].SummaryIndex)
	}
	if len(checkpoints[0].ContextItems) != 1 {
		t.Fatalf("context items len = %d, want 1", len(checkpoints[0].ContextItems))
	}
}

func TestCompactionCheckpointsDoNotAttachSummaryLinkedElsewhere(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testBoundaryMessage("boundary-1", transcript.CompactionKindBoundary, nil),
		testSummaryMessage(
			"summary-1",
			"boundary-2",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary context")},
		),
	}

	checkpoints := conversation.CompactionCheckpoints(messages)
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoints len = %d, want 2", len(checkpoints))
	}
	if checkpoints[0].SummaryIndex != -1 {
		t.Fatalf("boundary checkpoint unexpectedly attached summary: %#v", checkpoints[0])
	}
	if checkpoints[1].BoundaryIndex != -1 || checkpoints[1].SummaryIndex != 1 {
		t.Fatalf("summary-only checkpoint = %#v", checkpoints[1])
	}
}

func TestCompactionCheckpointsEmitStandaloneBoundary(t *testing.T) {
	t.Parallel()
	checkpoints := conversation.CompactionCheckpoints([]transcript.Message{
		testBoundaryMessage("boundary-1", transcript.CompactionKindBoundary, nil),
	})
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(checkpoints))
	}
	if checkpoints[0].BoundaryIndex != 0 || checkpoints[0].SummaryIndex != -1 {
		t.Fatalf("checkpoint = %#v", checkpoints[0])
	}
}

func TestCompactionCheckpointsEmitSummaryOnlyCheckpoint(t *testing.T) {
	t.Parallel()
	checkpoints := conversation.CompactionCheckpoints([]transcript.Message{
		testSummaryMessage(
			"summary-1",
			"",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary context")},
		),
	})
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(checkpoints))
	}
	if checkpoints[0].BoundaryIndex != -1 || checkpoints[0].SummaryIndex != 0 {
		t.Fatalf("checkpoint = %#v", checkpoints[0])
	}
}

func TestCompactionCheckpointsEmitStandaloneMicroboundary(t *testing.T) {
	t.Parallel()
	checkpoints := conversation.CompactionCheckpoints([]transcript.Message{
		testBoundaryMessage("micro-1", transcript.CompactionKindMicroboundary, nil),
	})
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(checkpoints))
	}
	if checkpoints[0].BoundaryIndex != 0 || checkpoints[0].SummaryIndex != -1 {
		t.Fatalf("checkpoint = %#v", checkpoints[0])
	}
}

func TestCompactionCheckpointsDoNotAttachSummaryAfterMicroboundary(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testBoundaryMessage("micro-1", transcript.CompactionKindMicroboundary, nil),
		testSummaryMessage(
			"summary-1",
			"micro-1",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary context")},
		),
	}

	checkpoints := conversation.CompactionCheckpoints(messages)
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoints len = %d, want 2", len(checkpoints))
	}
	if checkpoints[0].SummaryIndex != -1 {
		t.Fatalf("microboundary unexpectedly attached summary: %#v", checkpoints[0])
	}
	if checkpoints[1].BoundaryIndex != -1 || checkpoints[1].SummaryIndex != 1 {
		t.Fatalf("summary-only checkpoint = %#v", checkpoints[1])
	}
}

func TestCompactionCheckpointsDoNotRebuildContextFromMessageText(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testSummaryMessage(
			"summary-1",
			"",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("normalized compacted context")},
		),
	}
	messages[0].Text = "visible transcript text"

	checkpoints := conversation.CompactionCheckpoints(messages)
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(checkpoints))
	}
	if checkpoints[0].ContextItems[0].Message == nil {
		t.Fatalf("summary context item was nil")
	}
	if checkpoints[0].ContextItems[0].Message.Content[0].Text != "normalized compacted context" {
		t.Fatalf("context text = %q", checkpoints[0].ContextItems[0].Message.Content[0].Text)
	}
}

func TestCompactionCheckpointsHasUsableContextOnlyWhenItemsExist(t *testing.T) {
	t.Parallel()
	checkpoints := conversation.CompactionCheckpoints([]transcript.Message{
		testBoundaryMessage("boundary-1", transcript.CompactionKindBoundary, nil),
		{
			UUID:              "plain-1",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "assistant",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction:        nil,
			Timestamp:         time.Unix(3, 0),
			Text:              "plain visible message",
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
		},
		testSummaryMessage(
			"summary-1",
			"",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary context")},
		),
	})
	if checkpoints[0].HasUsableCompactedContext() {
		t.Fatalf("boundary checkpoint should not have usable context")
	}
	if !checkpoints[1].HasUsableCompactedContext() {
		t.Fatalf("summary checkpoint should have usable context")
	}
}

func TestCompactionCheckpointsPreserveTranscriptOrder(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		testSummaryMessage(
			"summary-0",
			"",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary zero")},
		),
		testBoundaryMessage("boundary-1", transcript.CompactionKindBoundary, nil),
		testSummaryMessage(
			"summary-1",
			"boundary-1",
			"",
			[]transcript.CompactedContextItem{testSummaryContextItem("summary one")},
		),
		testBoundaryMessage("micro-1", transcript.CompactionKindMicroboundary, nil),
	}

	checkpoints := conversation.CompactionCheckpoints(messages)
	if len(checkpoints) != 3 {
		t.Fatalf("checkpoints len = %d, want 3", len(checkpoints))
	}
	if checkpoints[0].SummaryUUID != "summary-0" {
		t.Fatalf("checkpoint order[0] = %#v", checkpoints[0])
	}
	if checkpoints[1].BoundaryUUID != "boundary-1" || checkpoints[1].SummaryUUID != "summary-1" {
		t.Fatalf("checkpoint order[1] = %#v", checkpoints[1])
	}
	if checkpoints[2].BoundaryUUID != "micro-1" {
		t.Fatalf("checkpoint order[2] = %#v", checkpoints[2])
	}
}

func TestCompactionCheckpointsLiveClaudeSmoke(t *testing.T) {
	t.Parallel()
	const path = "/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites/71545485-56c8-46a3-8cb7-1807f572ece1.jsonl"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("live Claude transcript unavailable: %v", err)
	}

	messages, err := conversation.CollectMessages(claudeparser.New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect live Claude transcript: %v", err)
	}

	checkpoints := conversation.CompactionCheckpoints(messages)
	var found bool
	for _, checkpoint := range checkpoints {
		if checkpoint.BoundaryIndex == -1 || checkpoint.SummaryIndex == -1 {
			continue
		}
		found = true
		if !checkpoint.HasUsableCompactedContext() {
			t.Fatalf("checkpoint should have usable context: %#v", checkpoint)
		}
		if len(checkpoint.ContextItems) != 1 {
			t.Fatalf("context items len = %d, want 1", len(checkpoint.ContextItems))
		}
		if checkpoint.ContextItems[0].Kind != transcript.CompactedContextItemKindMessage ||
			checkpoint.ContextItems[0].Message == nil {
			t.Fatalf("context item = %#v", checkpoint.ContextItems[0])
		}
		if checkpoint.ContextItems[0].Message.MessageClass != transcript.CompactedMessageClassSummary {
			t.Fatalf("message class = %q", checkpoint.ContextItems[0].Message.MessageClass)
		}
	}
	if !found {
		t.Fatalf("did not find a full Claude compaction checkpoint")
	}
}

func TestCompactionCheckpointsLiveCodexSmoke(t *testing.T) {
	t.Parallel()
	const path = "/Users/agoodkind/.codex/sessions/2026/04/27/rollout-2026-04-27T15-49-07-019dd121-d1ff-7ad3-862d-79b33d5fa300.jsonl"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("live Codex rollout unavailable: %v", err)
	}

	rawReplacementHistoryCount, err := firstCompactedReplacementHistoryCount(path)
	if err != nil {
		t.Fatalf("read raw replacement_history count: %v", err)
	}

	messages, err := conversation.CollectMessages(codexparser.New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect live Codex rollout: %v", err)
	}

	checkpoints := conversation.CompactionCheckpoints(messages)
	var found bool
	for _, checkpoint := range checkpoints {
		if checkpoint.BoundaryIndex == -1 || checkpoint.ReplacementHistoryCount == 0 {
			continue
		}
		found = true
		if checkpoint.ReplacementHistoryCount != rawReplacementHistoryCount {
			t.Fatalf(
				"replacement history count = %d, want %d",
				checkpoint.ReplacementHistoryCount,
				rawReplacementHistoryCount,
			)
		}
		roundTrip, roundTripErr := json.Marshal(checkpoint.ContextItems)
		if roundTripErr != nil {
			t.Fatalf("marshal checkpoint context items: %v", roundTripErr)
		}
		var decoded []transcript.CompactedContextItem
		if err := json.Unmarshal(roundTrip, &decoded); err != nil {
			t.Fatalf("unmarshal checkpoint context items: %v", err)
		}
		if len(decoded) != len(checkpoint.ContextItems) {
			t.Fatalf("decoded context items len = %d, want %d", len(decoded), len(checkpoint.ContextItems))
		}
		break
	}
	if !found {
		t.Fatalf("did not find a Codex boundary checkpoint with replacement history")
	}
}

func testBoundaryMessage(
	uuid string,
	kind transcript.CompactionKind,
	contextItems []transcript.CompactedContextItem,
) transcript.Message {
	return transcript.Message{
		UUID:              uuid,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "system",
		Visibility:        transcript.MessageVisibilityMetaOnly,
		Compaction: &transcript.CompactionMetadata{
			Kind:                      kind,
			Trigger:                   transcript.CompactionTriggerManual,
			PreTokens:                 100,
			PostTokens:                25,
			TokensSaved:               75,
			MessagesSummarized:        3,
			ReplacementHistoryCount:   len(contextItems),
			HeadUUID:                  "head-" + uuid,
			AnchorUUID:                "anchor-" + uuid,
			TailUUID:                  "tail-" + uuid,
			ContextItems:              contextItems,
			UserContext:               "",
			Direction:                 "",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        nil,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		},
		Timestamp: time.Unix(1, 0),
		Text:      "visible boundary text",
		Thinking:  "",
		HasTools:  false,
		Tools:     nil,
	}
}

func testSummaryMessage(
	uuid string,
	parentUUID string,
	logicalParentUUID string,
	contextItems []transcript.CompactedContextItem,
) transcript.Message {
	return transcript.Message{
		UUID:              uuid,
		ParentUUID:        parentUUID,
		LogicalParentUUID: logicalParentUUID,
		Role:              "user",
		Visibility:        transcript.MessageVisibilityTranscriptOnly,
		Compaction: &transcript.CompactionMetadata{
			Kind:                      transcript.CompactionKindSummary,
			Trigger:                   transcript.CompactionTriggerUnknown,
			PreTokens:                 0,
			PostTokens:                0,
			TokensSaved:               0,
			MessagesSummarized:        2,
			ReplacementHistoryCount:   0,
			HeadUUID:                  "",
			AnchorUUID:                "",
			TailUUID:                  "",
			ContextItems:              contextItems,
			UserContext:               "user context",
			Direction:                 "backward",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        nil,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		},
		Timestamp: time.Unix(2, 0),
		Text:      "visible transcript text",
		Thinking:  "",
		HasTools:  false,
		Tools:     nil,
	}
}

func testSummaryContextItem(text string) transcript.CompactedContextItem {
	messageItem := transcript.CompactedMessageItem{
		Role:         "user",
		Phase:        "",
		Content:      []transcript.CompactedMessageContentItem{{Type: "text", Text: text, Raw: json.RawMessage(`"summary"`)}},
		ContentRaw:   json.RawMessage(`[{"type":"text","text":"summary"}]`),
		MessageClass: transcript.CompactedMessageClassSummary,
		Raw:          json.RawMessage(`{"type":"message","role":"user"}`),
	}
	return transcript.CompactedContextItem{
		Kind:    transcript.CompactedContextItemKindMessage,
		Message: &messageItem,
	}
}

func testOrdinaryContextItem(text string) transcript.CompactedContextItem {
	messageItem := transcript.CompactedMessageItem{
		Role:         "assistant",
		Phase:        "commentary",
		Content:      []transcript.CompactedMessageContentItem{{Type: "output_text", Text: text, Raw: json.RawMessage(`"ordinary"`)}},
		ContentRaw:   json.RawMessage(`[{"type":"output_text","text":"ordinary"}]`),
		MessageClass: transcript.CompactedMessageClassOrdinary,
		Raw:          json.RawMessage(`{"type":"message","role":"assistant"}`),
	}
	return transcript.CompactedContextItem{
		Kind:    transcript.CompactedContextItemKindMessage,
		Message: &messageItem,
	}
}

type liveHistoryLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type liveCompactedPayload struct {
	ReplacementHistory []json.RawMessage `json:"replacement_history"`
}

func firstCompactedReplacementHistoryCount(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var line liveHistoryLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "compacted" {
			continue
		}
		var payload liveCompactedPayload
		if err := json.Unmarshal(line.Payload, &payload); err != nil {
			return 0, err
		}
		return len(payload.ReplacementHistory), nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}
