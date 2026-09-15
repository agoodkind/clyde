package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRawResponsesCompactionMutatesOnlyFinalAssistantSSEItem(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	first, _ := rawCompactionSSEFramesForTest(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"first assistant"}]}`, 0, 8, 9)
	final, completed := rawCompactionSSEFramesForTest(`{"id":"msg-2","type":"message","role":"assistant","content":[{"type":"output_text","text":"final assistant"}]}`, 0, 10, 11)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(first + final + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Contains(body, []byte(first)) {
		t.Fatalf("first assistant item changed: %s", body)
	}
	if !bytes.Contains(body, []byte("<pre-compaction-transcript>")) {
		t.Fatalf("transcript tag count was not one: %s", body)
	}
	if !bytes.Contains(body, []byte(`"id":"msg-2"`)) || !bytes.Contains(body, []byte("final assistant")) {
		t.Fatalf("final assistant did not receive transcript: %s", body)
	}
}

func TestRawResponsesCompactionKeepsCompletedSSEOutputCoherent(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	itemDone, completed := rawCompactionSSEFramesForTest(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"No"}]}`, 0, 10, 11)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Contains(body, []byte("<pre-compaction-transcript>")) {
		t.Fatalf("transcript was not appended to both SSE results: %s", body)
	}
	for _, frame := range bytes.Split(bytes.TrimSpace(body), []byte("\n\n")) {
		_, data, dataCount := rawSSEFrameDataValue(frame)
		if dataCount != 1 || !json.Valid(data) {
			t.Fatalf("invalid SSE frame: %s", frame)
		}
	}
}

func TestRawResponsesCompactionStreamingEventsMatchSnapshots(t *testing.T) {
	tests := []struct {
		name          string
		item          string
		prefix        string
		firstSequence int
	}{
		{
			name:          "existing output text",
			item:          `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`,
			prefix:        "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\",\"sequence_number\":9}\n\n",
			firstSequence: 9,
		},
		{name: "new output text", item: `{"id":"msg-1","type":"message","role":"assistant"}`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			itemSequence := testCase.firstSequence
			if testCase.prefix != "" {
				itemSequence++
			}
			itemDone, completed := rawCompactionSSEFramesForTest(testCase.item, 0, itemSequence, itemSequence+1)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(testCase.prefix + itemDone + completed)),
			}
			body := readResponseBody(t, rawResponseTransformerForTest(t).TransformResponse(response))
			frames := rawCompactionSSEDecodedFramesForTest(t, body)
			var accumulated string
			var itemText string
			var completedText string
			for index, frame := range frames {
				if frame.SequenceNumber != testCase.firstSequence+index {
					t.Fatalf("frame %d sequence = %d", index, frame.SequenceNumber)
				}
				if frame.Type == string(rawCompactionSSEOutputTextDelta) {
					accumulated += frame.Delta
				}
				if frame.Type == string(rawCompactionSSEOutputItemDone) {
					itemText = rawCompactionSSEOutputTextForTest(t, frame.Item)
				}
				if frame.Type == string(rawCompactionSSECompleted) {
					var terminal struct {
						Output []json.RawMessage `json:"output"`
					}
					if json.Unmarshal(frame.Response, &terminal) != nil || len(terminal.Output) != 1 {
						t.Fatal("invalid completed snapshot")
					}
					completedText = rawCompactionSSEOutputTextForTest(t, terminal.Output[0])
				}
			}
			if accumulated != itemText || accumulated != completedText || !strings.Contains(accumulated, "<pre-compaction-transcript>") {
				t.Fatalf("deltas = %q item = %q completed = %q", accumulated, itemText, completedText)
			}
		})
	}
}

func TestRawResponsesCompactionSyntheticEventsIncludeRequiredArrays(t *testing.T) {
	item := `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	itemDone, completed := rawCompactionSSEFramesForTest(item, 0, 10, 11)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + completed)),
	}
	body := readResponseBody(t, rawResponseTransformerForTest(t).TransformResponse(response))
	found := make(map[string]bool)
	for _, frame := range bytes.Split(bytes.TrimSpace(body), []byte("\n\n")) {
		_, data, dataCount := rawSSEFrameDataValue(frame)
		if dataCount != 1 {
			continue
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		var eventType string
		if json.Unmarshal(payload["type"], &eventType) != nil {
			continue
		}
		switch rawCompactionSSEEvent(eventType) {
		case rawCompactionSSEContentPartAdded, rawCompactionSSEContentPartDone:
			var part map[string]json.RawMessage
			var annotations []json.RawMessage
			if json.Unmarshal(payload["part"], &part) != nil || json.Unmarshal(part["annotations"], &annotations) != nil {
				t.Fatalf("%s omitted part.annotations: %s", eventType, frame)
			}
			found[eventType] = true
		case rawCompactionSSEOutputTextDelta, rawCompactionSSEOutputTextDone:
			var logprobs []json.RawMessage
			if json.Unmarshal(payload["logprobs"], &logprobs) != nil {
				t.Fatalf("%s omitted logprobs: %s", eventType, frame)
			}
			found[eventType] = true
		default:
		}
	}
	for _, eventType := range []rawCompactionSSEEvent{
		rawCompactionSSEContentPartAdded,
		rawCompactionSSEContentPartDone,
		rawCompactionSSEOutputTextDelta,
		rawCompactionSSEOutputTextDone,
	} {
		if !found[string(eventType)] {
			t.Fatalf("required event %s was absent", eventType)
		}
	}
}

func TestRawResponsesCompactionCandidateBufferFailsOpenAtCap(t *testing.T) {
	item := `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	itemDone, _ := rawCompactionSSEFramesForTest(item, 0, 10, 11)
	following := "event: response.future\ndata: {\"type\":\"response.future\",\"padding\":\"" +
		strings.Repeat("x", maxRawCompactionSSEPendingBytes) + "\"}\n\n"
	original := []byte(itemDone + following)
	upstream, writer := io.Pipe()
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       upstream,
	}
	transformer := rawResponseTransformerForTest(t)
	transformer.stream = true
	body := transformer.TransformResponse(response).Body
	t.Cleanup(func() { _ = body.Close() })
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Write(original)
		writeDone <- err
		<-release
		_ = writer.Close()
	}()
	readDone := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		got := make([]byte, len(original))
		_, err := io.ReadFull(body, got)
		if err != nil {
			readErr <- err
			return
		}
		readDone <- got
	}()
	select {
	case got := <-readDone:
		if !bytes.Equal(got, original) {
			t.Fatal("cap fail-open changed streamed bytes")
		}
	case err := <-readErr:
		t.Fatalf("read cap fail-open: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("cap fail-open waited for upstream EOF")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-writeDone; err != nil {
		t.Fatalf("write oversized stream: %v", err)
	}
}

func TestRawResponsesCompactionOversizedUnterminatedFrameStreamsBeforeEOF(t *testing.T) {
	for _, prefixSize := range []int{0, 64, 128, 4094, 4096} {
		t.Run(fmt.Sprintf("prefix-%d", prefixSize), func(t *testing.T) {
			prefix := ""
			if prefixSize > 0 {
				prefix = "id: " + strings.Repeat("i", prefixSize-5) + "\n"
			}
			expectedSize := maxRawCompactionSSEPendingBytes + 77
			original := []byte(prefix + "data: " + strings.Repeat("x", expectedSize-len(prefix)-len("data: ")))
			upstream, writer := io.Pipe()
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       upstream,
			}
			transformer := rawResponseTransformerForTest(t)
			transformer.stream = true
			body := transformer.TransformResponse(response).Body
			t.Cleanup(func() { _ = body.Close() })
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			writeDone := make(chan error, 1)
			go func() {
				_, err := writer.Write(original)
				writeDone <- err
				<-release
				_ = writer.Close()
			}()
			readDone := make(chan []byte, 1)
			readErr := make(chan error, 1)
			go func() {
				got := make([]byte, len(original))
				_, err := io.ReadFull(body, got)
				if err != nil {
					readErr <- err
					return
				}
				readDone <- got
			}()
			select {
			case got := <-readDone:
				if !bytes.Equal(got, original) {
					t.Fatal("oversized unterminated frame changed streamed bytes")
				}
			case err := <-readErr:
				t.Fatalf("read oversized unterminated frame: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("oversized unterminated frame waited for upstream EOF")
			}
			releaseOnce.Do(func() { close(release) })
			if err := <-writeDone; err != nil {
				t.Fatalf("write oversized unterminated frame: %v", err)
			}
		})
	}
}

func TestRawResponsesCompactionPreservesFragmentedSSELines(t *testing.T) {
	for _, lineEnding := range []string{"\n", "\r\n"} {
		for _, lineSize := range []int{4094, 4095, 4096, 4097, 8191, 8192} {
			t.Run(fmt.Sprintf("ending%d-size%d", len(lineEnding), lineSize), func(t *testing.T) {
				template := `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}`
				emptyDone, _ := rawCompactionSSEFramesForTest(fmt.Sprintf(template, ""), 0, 10, 11)
				emptyLineSize := len(strings.Split(emptyDone, "\n")[1])
				padding := strings.Repeat("x", lineSize-emptyLineSize)
				itemDone, completed := rawCompactionSSEFramesForTest(
					fmt.Sprintf(template, padding),
					0,
					10,
					11,
				)
				if actual := len(strings.Split(itemDone, "\n")[1]); actual != lineSize {
					t.Fatalf("data line size = %d, want %d", actual, lineSize)
				}
				original := strings.ReplaceAll(itemDone+completed, "\n", lineEnding)
				response := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(original)),
				}
				body := readResponseBody(t, rawResponseTransformerForTest(t).TransformResponse(response))
				if !bytes.Contains(body, []byte("<pre-compaction-transcript>")) {
					t.Fatalf(
						"valid SSE lost compaction injection at %d-byte data line with %d-byte ending",
						lineSize,
						len(lineEnding),
					)
				}
			})
		}
	}
}

func TestRawResponsesCompactionSequenceOverflowFailsOpen(t *testing.T) {
	item := `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	tests := []struct {
		name              string
		itemSequence      int
		followingSequence int
		completedSequence int
	}{
		{name: "candidate", itemSequence: math.MaxInt - 3, completedSequence: math.MaxInt - 2},
		{name: "following", itemSequence: math.MaxInt - 6, followingSequence: math.MaxInt - 3, completedSequence: math.MaxInt - 2},
		{name: "completed", itemSequence: math.MaxInt - 5, completedSequence: math.MaxInt - 3},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			itemDone, completed := rawCompactionSuccessfulSSEFramesForTest(
				item,
				0,
				testCase.itemSequence,
				testCase.completedSequence,
			)
			following := ""
			if testCase.followingSequence != 0 {
				following = fmt.Sprintf(
					"event: response.future\ndata: {\"type\":\"response.future\",\"sequence_number\":%d}\n\n",
					testCase.followingSequence,
				)
			}
			original := []byte(itemDone + following + completed)
			transformer := rawResponseTransformerForTest(t)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(original)),
			}

			got := readResponseBody(t, transformer.TransformResponse(response))
			if !bytes.Equal(got, original) {
				t.Fatalf("overflow mutated response:\n got: %s\nwant: %s", got, original)
			}
		})
	}
}

func TestRawResponsesCompactionSequenceBoundaryMutates(t *testing.T) {
	item := `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	itemDone, completed := rawCompactionSuccessfulSSEFramesForTest(
		item,
		0,
		math.MaxInt-6,
		math.MaxInt-4,
	)
	following := fmt.Sprintf(
		"event: response.future\ndata: {\"type\":\"response.future\",\"sequence_number\":%d}\n\n",
		math.MaxInt-5,
	)
	transformer := rawResponseTransformerForTest(t)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + following + completed)),
	}

	frames := rawCompactionSSEDecodedFramesForTest(
		t,
		readResponseBody(t, transformer.TransformResponse(response)),
	)
	if len(frames) != 7 {
		t.Fatalf("boundary frame count = %d, want 7", len(frames))
	}
	for index, frame := range frames {
		want := math.MaxInt - 6 + index
		if frame.SequenceNumber != want {
			t.Fatalf("frame %d sequence = %d, want %d", index, frame.SequenceNumber, want)
		}
	}
}

func TestRawResponsesCompactionCandidateTypeFailuresPassThrough(t *testing.T) {
	for _, candidateType := range []string{"", "response.output_item.added"} {
		t.Run(candidateType, func(t *testing.T) {
			item := `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
			itemDone, completed := rawCompactionSSEFramesForTest(item, 0, 10, 11)
			if candidateType == "" {
				itemDone = strings.Replace(itemDone, `"type":"response.output_item.done",`, "", 1)
			} else {
				itemDone = strings.Replace(itemDone, "response.output_item.done\",", candidateType+"\",", 1)
			}
			original := []byte(itemDone + completed)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(original)),
			}
			got := readResponseBody(t, rawResponseTransformerForTest(t).TransformResponse(response))
			if !bytes.Equal(got, original) {
				t.Fatalf("invalid type mutated response:\n got: %s\nwant: %s", got, original)
			}
		})
	}
}
