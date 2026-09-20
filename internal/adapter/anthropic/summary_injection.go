package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/slogger"
)

const summaryCloseTag = "</summary>"

// missingContentBlockIndex is the starting maximum for a scan over content
// block indexes. A stream with no indexed event leaves it, and the appended
// block takes index 0.
const missingContentBlockIndex = -1

// InjectIntoSummary inserts content into an Anthropic summary response and
// returns the rewritten SSE body.
//
// Claude Code's formatCompactSummary keeps the text between <summary> and
// </summary> and drops a separately appended trailing content block. The
// injection therefore goes before </summary>, where the persisted
// isCompactSummary message preserves it.
//
// A response with no </summary> in any text block has a malformed or empty
// summary. Claude Code keeps the whole assistant text in that case, so the
// injection becomes a trailing content block.
func InjectIntoSummary(body []byte, content string) ([]byte, error) {
	events := parseSummarySSEEvents(string(body))
	injected, ok, err := injectIntoSummaryBlock(events, content)
	if err != nil {
		return nil, err
	}
	if ok {
		return buildSummarySSEBody(injected), nil
	}
	blockIndex := maxSummaryContentBlockIndex(events) + 1
	appendEvents, err := marshalSummaryAppendEvents(blockIndex, content)
	if err != nil {
		return nil, err
	}
	events = insertSummaryAppendEvents(events, appendEvents)
	return buildSummarySSEBody(events), nil
}

// ResponseText returns the text_delta content from an Anthropic SSE response.
func ResponseText(body []byte) string {
	events := parseSummarySSEEvents(string(body))
	var builder strings.Builder
	previousIndex := missingContentBlockIndex
	for _, event := range events {
		index, text, ok := summaryTextDeltaOf(event)
		if !ok || text == "" {
			continue
		}
		if builder.Len() > 0 && index != previousIndex {
			builder.WriteByte('\n')
		}
		builder.WriteString(text)
		previousIndex = index
	}
	return builder.String()
}

// AppendTextBlock adds one text block before the closing message events.
func AppendTextBlock(body []byte, content string) ([]byte, error) {
	events := parseSummarySSEEvents(string(body))
	blockIndex := maxSummaryContentBlockIndex(events) + 1
	appendEvents, err := marshalTextAppendEvents(blockIndex, content)
	if err != nil {
		return nil, err
	}
	events = insertSummaryAppendEvents(events, appendEvents)
	return buildSummarySSEBody(events), nil
}

// injectIntoSummaryBlock rebuilds the assistant text block that contains
// </summary> as a single delta with content inserted before that tag, and
// leaves every other event unchanged. The second return is false when no text
// block contains </summary>.
func injectIntoSummaryBlock(events []summarySSEEvent, content string) ([]summarySSEEvent, bool, error) {
	textByIndex := map[int]string{}
	order := make([]int, 0)
	for _, event := range events {
		index, text, ok := summaryTextDeltaOf(event)
		if !ok {
			continue
		}
		if _, seen := textByIndex[index]; !seen {
			order = append(order, index)
		}
		textByIndex[index] += text
	}
	target := -1
	for _, index := range order {
		if strings.Contains(textByIndex[index], summaryCloseTag) {
			target = index
			break
		}
	}
	if target == -1 {
		return nil, false, nil
	}
	original := textByIndex[target]
	cut := strings.Index(original, summaryCloseTag)
	injection := escapeSummaryCloseTag(wrappedTranscriptContent(content))
	modified := original[:cut] + injection + original[cut:]

	output := make([]summarySSEEvent, 0, len(events))
	replaced := false
	for _, event := range events {
		index, _, ok := summaryTextDeltaOf(event)
		if ok && index == target {
			if replaced {
				continue
			}
			data, err := marshalSummarySSEData(newSummaryBlockDeltaPayload(target, modified))
			if err != nil {
				return nil, false, err
			}
			output = append(output, summarySSEEvent{Name: "content_block_delta", Data: data})
			replaced = true
			continue
		}
		output = append(output, event)
	}
	return output, true, nil
}

// summaryTextDeltaOf returns the block index and text of a text_delta content
// block event. The final bool is false for every other event.
func summaryTextDeltaOf(event summarySSEEvent) (int, string, bool) {
	if event.Name != "content_block_delta" {
		return 0, "", false
	}
	var payload summaryDeltaTextPayload
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		return 0, "", false
	}
	if payload.Index == nil || payload.Delta.Type != "text_delta" {
		return 0, "", false
	}
	return *payload.Index, payload.Delta.Text, true
}

// escapeSummaryCloseTag neutralizes a literal </summary> inside the injected
// content. A conversation that discussed the tag would otherwise close the
// summary span early.
func escapeSummaryCloseTag(s string) string {
	return strings.ReplaceAll(s, summaryCloseTag, `<\/summary>`)
}

func parseSummarySSEEvents(body string) []summarySSEEvent {
	records := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	events := make([]summarySSEEvent, 0, len(records))
	for _, record := range records {
		event := parseSummarySSERecord(record)
		if event.Name == "" && event.Data == "" {
			continue
		}
		events = append(events, event)
	}
	return events
}

func parseSummarySSERecord(record string) summarySSEEvent {
	lines := strings.Split(record, "\n")
	dataLines := make([]string, 0, len(lines))
	event := summarySSEEvent{Name: "", Data: ""}
	for _, line := range lines {
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event.Name = strings.TrimSpace(value)
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		}
	}
	event.Data = strings.Join(dataLines, "\n")
	return event
}

func maxSummaryContentBlockIndex(events []summarySSEEvent) int {
	maxIndex := missingContentBlockIndex
	for _, event := range events {
		var payload summaryIndexedPayload
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			continue
		}
		if payload.Index == nil {
			continue
		}
		if *payload.Index > maxIndex {
			maxIndex = *payload.Index
		}
	}
	return maxIndex
}

func marshalSummaryAppendEvents(blockIndex int, content string) ([]summarySSEEvent, error) {
	return marshalTextAppendEvents(blockIndex, wrappedTranscriptContent(content))
}

func marshalTextAppendEvents(blockIndex int, content string) ([]summarySSEEvent, error) {
	start, err := marshalSummarySSEData(newSummaryBlockStartPayload(blockIndex))
	if err != nil {
		return nil, err
	}
	delta, err := marshalSummarySSEData(newSummaryBlockDeltaPayload(blockIndex, content))
	if err != nil {
		return nil, err
	}
	stop, err := marshalSummarySSEData(newSummaryBlockStopPayload(blockIndex))
	if err != nil {
		return nil, err
	}
	return []summarySSEEvent{
		{Name: "content_block_start", Data: start},
		{Name: "content_block_delta", Data: delta},
		{Name: "content_block_stop", Data: stop},
	}, nil
}

// insertSummaryAppendEvents places the appended block before message_delta or
// message_stop. Anthropic closes the message with those two events, and a
// content block after either one is invalid.
func insertSummaryAppendEvents(events []summarySSEEvent, appendEvents []summarySSEEvent) []summarySSEEvent {
	insertAt := len(events)
	for index, event := range events {
		if event.Name == "message_delta" || event.Name == "message_stop" {
			insertAt = index
			break
		}
	}
	output := make([]summarySSEEvent, 0, len(events)+len(appendEvents))
	output = append(output, events[:insertAt]...)
	output = append(output, appendEvents...)
	output = append(output, events[insertAt:]...)
	return output
}

func buildSummarySSEBody(events []summarySSEEvent) []byte {
	var builder strings.Builder
	for _, event := range events {
		if event.Name != "" {
			builder.WriteString("event: ")
			builder.WriteString(event.Name)
			builder.WriteByte('\n')
		}
		writeSummarySSEDataLines(&builder, event.Data)
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func writeSummarySSEDataLines(builder *strings.Builder, data string) {
	if data == "" {
		return
	}
	for line := range strings.SplitSeq(data, "\n") {
		builder.WriteString("data: ")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
}

func marshalSummarySSEData[T summaryPayload](payload T) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		slog.Warn(
			"adapter.anthropic.summary_injection_encode_failed",
			"concern", string(slogger.ConcernAdapterProviderAnthReq),
			"err", err,
		)
		return "", fmt.Errorf("encode summary injection SSE JSON: %w", err)
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

func wrappedTranscriptContent(content string) string {
	var builder strings.Builder
	builder.WriteString("\n\n")
	builder.WriteString(reorienttag.PreCompactionTranscriptOpen)
	builder.WriteByte('\n')
	builder.WriteString(content)
	builder.WriteByte('\n')
	builder.WriteString(reorienttag.PreCompactionTranscriptClose)
	builder.WriteByte('\n')
	return builder.String()
}

type summarySSEEvent struct {
	Name string
	Data string
}

type summaryIndexedPayload struct {
	Index *int `json:"index"`
}

// summaryDeltaTextPayload decodes a content_block_delta event down to its block
// index and streamed text. injectIntoSummaryBlock reassembles a text block from
// those two fields.
type summaryDeltaTextPayload struct {
	Index *int `json:"index"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type summaryPayload interface {
	summaryBlockStartPayload | summaryBlockDeltaPayload | summaryBlockStopPayload
}

type summaryTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type summaryTextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type summaryBlockStartPayload struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	ContentBlock summaryTextBlock `json:"content_block"`
}

type summaryBlockDeltaPayload struct {
	Type  string           `json:"type"`
	Index int              `json:"index"`
	Delta summaryTextDelta `json:"delta"`
}

type summaryBlockStopPayload struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

func newSummaryBlockStartPayload(blockIndex int) summaryBlockStartPayload {
	return summaryBlockStartPayload{
		Type:  "content_block_start",
		Index: blockIndex,
		ContentBlock: summaryTextBlock{
			Type: "text",
			Text: "",
		},
	}
}

func newSummaryBlockDeltaPayload(blockIndex int, content string) summaryBlockDeltaPayload {
	return summaryBlockDeltaPayload{
		Type:  "content_block_delta",
		Index: blockIndex,
		Delta: summaryTextDelta{
			Type: "text_delta",
			Text: content,
		},
	}
}

func newSummaryBlockStopPayload(blockIndex int) summaryBlockStopPayload {
	return summaryBlockStopPayload{
		Type:  "content_block_stop",
		Index: blockIndex,
	}
}
