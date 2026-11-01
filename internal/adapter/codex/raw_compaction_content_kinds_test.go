package codex_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/conversation"
	codexcompaction "goodkind.io/clyde/internal/providers/codex/compaction"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorientinject"
	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/tokencount"
	"goodkind.io/gklog/correlation"
)

const (
	rawCompactionMarkerItemID  = "msg-1"
	rawCompactionMarkerTurnID  = "turn-1"
	rawCompactionMarkerSummary = "summary"
	rawCompactionMarkerUnknown = "unknown"
	rawCompactionMarkerSession = "marker-session"
	rawCompactionMarkerCipher  = "marker-cipher"
	rawCompactionMarkerBudget  = 4096
	rawCompactionMarkerV1      = `{"session_id":"marker-session","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses"}}`
	rawCompactionMarkerV2      = `{"session_id":"marker-session","thread_source":"user","sandbox":"none","request_kind":"turn","compaction":{"phase":"final_answer"}}`
	rawCompactionMarkerV2Body  = `{"model":"gpt-marker","input":[{"type":"compaction","encrypted_content":"marker-cipher"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`
	rawCompactionMarkerV1Body  = `{"model":"gpt-marker","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"oldest message"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"recent message"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent assistant"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize the conversation"}]}]}`
)

type rawCompactionMarkerProducer struct {
	name      string
	injection string
	build     func(t *testing.T) *adaptercodex.RawResponsesCompactionTransformer
}

type rawCompactionMarkerItem struct {
	ID      string `json:"id"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Metadata struct {
		TurnID           string   `json:"turn_id"`
		ContentItemKinds []string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

type rawCompactionMarkerFrame struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Item     json.RawMessage `json:"item"`
	Response json.RawMessage `json:"response"`
}

func rawCompactionMarkerRequest(body string, metadata string) adaptercodex.RawResponsesRequest {
	return adaptercodex.RawResponsesRequest{
		Body:        []byte(body),
		Header:      http.Header{adaptercodex.CodexTurnMetadataHeader: {metadata}},
		RequestID:   "",
		Correlation: correlation.Context{},
		Stream:      false,
	}
}

func rawCompactionMarkerSplitter() adaptercodex.CompactionSplitter {
	return codexcompaction.NewSplitter(reorientinject.Settings{
		DefaultBudget:    rawCompactionMarkerBudget,
		DefaultContent:   conversation.NewContentKindSet(),
		MaxRetainedBytes: adaptercodex.MaxCompactionInjectionBytes,
		InstructionsFile: "",
		Counter:          tokencount.LocalCounter(tokencount.FamilyClaude, "", tokencount.Settings{}),
	})
}

func rawCompactionMarkerV1Transformer(t *testing.T) *adaptercodex.RawResponsesCompactionTransformer {
	t.Helper()
	request := rawCompactionMarkerRequest(rawCompactionMarkerV1Body, rawCompactionMarkerV1)
	_, transformer := adaptercodex.PrepareRawResponsesCompaction(request, rawCompactionMarkerSplitter())
	if transformer == nil {
		t.Fatal("PrepareRawResponsesCompaction did not return a response transformer for the probe request.")
	}
	return transformer
}

func rawCompactionMarkerV2Transformer(t *testing.T, injection string) *adaptercodex.RawResponsesCompactionTransformer {
	t.Helper()
	registry := adaptercodex.NewRawResponsesCompactionV2Registry(nil)
	if !registry.Arm(rawCompactionMarkerSession, rawCompactionMarkerCipher, injection) {
		t.Fatal("Registry Arm rejected the test injection.")
	}
	request := rawCompactionMarkerRequest(rawCompactionMarkerV2Body, rawCompactionMarkerV2)
	injected, recovery, ok := adaptercodex.InjectRawResponsesCompactionV2Recovery(request, registry)
	if !ok {
		t.Fatal("InjectRawResponsesCompactionV2Recovery rejected the test request.")
	}
	return adaptercodex.NewRawResponsesCompactionV2FinalAnswerTransformer(injected, recovery)
}

func rawCompactionMarkerProducers(t *testing.T) []rawCompactionMarkerProducer {
	t.Helper()
	split, ok := rawCompactionMarkerSplitter().Plan([]byte(rawCompactionMarkerV1Body))
	if !ok {
		t.Fatal("The splitter did not plan a split for the probe request.")
	}
	v2Injection := reorienttag.WrapInjection("recovered transcript", "")
	return []rawCompactionMarkerProducer{
		{name: "v1", injection: split.Injection, build: rawCompactionMarkerV1Transformer},
		{
			name:      "v2",
			injection: v2Injection,
			build: func(t *testing.T) *adaptercodex.RawResponsesCompactionTransformer {
				t.Helper()
				return rawCompactionMarkerV2Transformer(t, v2Injection)
			},
		},
	}
}

func rawCompactionMarkerJSON(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("The test could not encode the transcript: %v", err)
	}
	return string(encoded)
}

func rawCompactionMarkerItemJSON(t *testing.T, text string) string {
	t.Helper()
	return `{"id":"` + rawCompactionMarkerItemID + `","type":"message","role":"assistant","phase":"final_answer",` +
		`"content":[{"type":"output_text","text":` + rawCompactionMarkerJSON(t, text) + `}],` +
		`"internal_chat_message_metadata_passthrough":{"turn_id":"` + rawCompactionMarkerTurnID + `"}}`
}

func rawCompactionMarkerStream(item string) string {
	return fmt.Sprintf(
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"sequence_number\":10,\"item\":%s}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":11,\"response\":{\"status\":\"completed\",\"id\":\"resp-1\",\"output\":[%s]}}\n\n",
		item,
		item,
	)
}

func rawCompactionMarkerTransform(t *testing.T, transformer *adaptercodex.RawResponsesCompactionTransformer, contentType string, body string) []byte {
	t.Helper()
	response := transformer.TransformResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	})
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("The test could not read the transformed response body: %v", err)
	}
	return got
}

func assertRawCompactionMarkedItem(t *testing.T, raw json.RawMessage, originalText string, injection string) {
	t.Helper()
	var item rawCompactionMarkerItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("The test could not decode the transformed item: %v\nThe transformed item contains: %s", err, raw)
	}
	if item.ID != rawCompactionMarkerItemID || item.Metadata.TurnID != rawCompactionMarkerTurnID {
		t.Fatalf("The transformation changed the native identity: %s", raw)
	}
	if len(item.Content) != 2 || item.Content[0].Text != originalText || item.Content[1].Text != injection {
		t.Fatalf("The transformed content did not match the expected parts: %#v", item.Content)
	}
	wantKinds := []string{rawCompactionMarkerUnknown, codexstore.InjectedTranscriptContentKind}
	if !slices.Equal(item.Metadata.ContentItemKinds, wantKinds) {
		t.Fatalf("The content_item_kinds array did not match the expected kinds: %v", item.Metadata.ContentItemKinds)
	}
	if indexes := codexstore.InjectedTranscriptIndexes(raw); !slices.Equal(indexes, []int{1}) {
		t.Fatalf("The injected transcript indexes did not match the expected indexes: %v", indexes)
	}
}

func TestRawResponsesCompactionMarksSeparateTranscriptPart(t *testing.T) {
	for _, producer := range rawCompactionMarkerProducers(t) {
		texts := map[string]string{
			"plain":              rawCompactionMarkerSummary,
			"opening tag":        "quoted " + reorienttag.PreCompactionTranscriptOpen,
			"closing tag":        "quoted " + reorienttag.PreCompactionTranscriptClose,
			"older wrapper":      rawCompactionMarkerSummary + reorienttag.WrapInjection("older", ""),
			"intended injection": rawCompactionMarkerSummary + producer.injection,
		}
		for name, text := range texts {
			t.Run(producer.name+" json "+name, func(t *testing.T) {
				body := `{"status":"completed","id":"resp-1","output":[` + rawCompactionMarkerItemJSON(t, text) + `]}`
				got := rawCompactionMarkerTransform(t, producer.build(t), "application/json", body)
				var decoded struct {
					Output []json.RawMessage `json:"output"`
				}
				if err := json.Unmarshal(got, &decoded); err != nil || len(decoded.Output) != 1 {
					t.Fatalf("The test could not decode the transformed response: %v\nThe transformed response contains: %s", err, got)
				}
				assertRawCompactionMarkedItem(t, decoded.Output[0], text, producer.injection)
				repeated := rawCompactionMarkerTransform(t, producer.build(t), "application/json", string(got))
				if !bytes.Equal(repeated, got) {
					t.Fatalf("The repeated transformation changed the response.\nThe response contains: %s\nThe test expected: %s", repeated, got)
				}
			})
			t.Run(producer.name+" sse "+name, func(t *testing.T) {
				stream := rawCompactionMarkerStream(rawCompactionMarkerItemJSON(t, text))
				got := rawCompactionMarkerTransform(t, producer.build(t), "text/event-stream", stream)
				assertRawCompactionMarkedStream(t, got, text, producer.injection)
				repeated := rawCompactionMarkerTransform(t, producer.build(t), "text/event-stream", string(got))
				if !bytes.Equal(repeated, got) {
					t.Fatalf("The repeated transformation changed the stream.\nThe stream contains: %s\nThe test expected: %s", repeated, got)
				}
			})
		}
	}
}

func rawCompactionMarkerFrames(t *testing.T, body []byte) []rawCompactionMarkerFrame {
	t.Helper()
	frames := make([]rawCompactionMarkerFrame, 0)
	for rawFrame := range bytes.SplitSeq(bytes.TrimSpace(body), []byte("\n\n")) {
		for line := range bytes.SplitSeq(rawFrame, []byte("\n")) {
			data, isData := bytes.CutPrefix(line, []byte("data: "))
			if !isData {
				continue
			}
			var frame rawCompactionMarkerFrame
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Fatalf("The test could not decode the stream frame: %v\nThe frame contains %s", err, rawFrame)
			}
			frames = append(frames, frame)
		}
	}
	return frames
}

func assertRawCompactionMarkedStream(t *testing.T, body []byte, originalText string, injection string) {
	t.Helper()
	var itemDone json.RawMessage
	var completedItem json.RawMessage
	var deltas string
	for _, frame := range rawCompactionMarkerFrames(t, body) {
		switch frame.Type {
		case "response.output_text.delta":
			deltas += frame.Delta
		case "response.output_item.done":
			itemDone = frame.Item
		case "response.completed":
			var response struct {
				Output []json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(frame.Response, &response); err != nil || len(response.Output) != 1 {
				t.Fatalf("The test could not decode the completed response: %v\nThe completed response contains: %s", err, frame.Response)
			}
			completedItem = response.Output[0]
		}
	}
	if deltas != injection {
		t.Fatalf("The stream emitted deltas %q instead of %q", deltas, injection)
	}
	assertRawCompactionMarkedItem(t, itemDone, originalText, injection)
	assertRawCompactionMarkedItem(t, completedItem, originalText, injection)
	if !bytes.Equal(itemDone, completedItem) {
		t.Fatalf("The stream snapshots differ.\nThe item contains: %s\nThe completed response contains: %s", itemDone, completedItem)
	}
}

func TestRawResponsesCompactionFillsContentKindsThroughNewPart(t *testing.T) {
	body := `{"status":"completed","id":"resp-1","output":[{"id":"msg-1","type":"message","role":"assistant",` +
		`"content":[{"type":"refusal","refusal":"No"},{"type":"output_text","text":"summary"}],` +
		`"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1","content_item_kinds":["native"]}}]}`
	got := rawCompactionMarkerTransform(t, rawCompactionMarkerV1Transformer(t), "application/json", body)
	var decoded struct {
		Output []rawCompactionMarkerItem `json:"output"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil || len(decoded.Output) != 1 {
		t.Fatalf("The test could not decode the transformed response: %v\nThe transformed response contains: %s", err, got)
	}
	want := []string{"native", rawCompactionMarkerUnknown, codexstore.InjectedTranscriptContentKind}
	if !slices.Equal(decoded.Output[0].Metadata.ContentItemKinds, want) || decoded.Output[0].Metadata.TurnID != rawCompactionMarkerTurnID {
		t.Fatalf("The metadata did not match the expected fields: %+v", decoded.Output[0].Metadata)
	}
}

func TestRawResponsesCompactionPreservesResponseWithUnusableContentKinds(t *testing.T) {
	for name, metadata := range map[string]string{
		"kinds cover new part": `{"content_item_kinds":["native","native"]}`,
		"kinds not an array":   `{"content_item_kinds":"native"}`,
		"metadata not object":  `"native"`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"status":"completed","id":"resp-1","output":[{"id":"msg-1","type":"message","role":"assistant",` +
				`"content":[{"type":"output_text","text":"summary"}],"internal_chat_message_metadata_passthrough":` + metadata + `}]}`
			got := rawCompactionMarkerTransform(t, rawCompactionMarkerV1Transformer(t), "application/json", body)
			if string(got) != body {
				t.Fatalf("The transformation changed the response.\nThe response contains: %s\nThe test expected: %s", got, body)
			}
		})
	}
}
