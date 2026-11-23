package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/clyde/codexwire"
	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
	"goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
)

func TestCursorChatReplaysCodexReasoningOutsideReplyText(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	upstream.reply = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		const item = `{"type":"reasoning","id":"rs_private","summary":[{"type":"summary_text","text":"Checked constraints."}],"encrypted_content":"opaque-test-payload"}`
		const event = "event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","item":` + item + "}\n\n"
		body := strings.Replace(
			codexConformanceSSEBody(conformanceUsageWithReasoning),
			"event: response.output_text.delta",
			event+"event: response.output_text.delta",
			1,
		)
		_, _ = w.Write([]byte(body))
	}

	listeners := startConformanceServer(t, upstream)
	url := listeners.cursor + "/v1/chat/completions"

	for _, stream := range []bool{true, false} {
		t.Run(
			"stream="+strconv.FormatBool(stream),
			func(t *testing.T) {
				const user = `{"role":"user","content":"inspect"}`
				first := postConformance(
					t,
					url,
					fmt.Sprintf(`{"model":"gpt-future","stream":%t,"messages":[%s]}`, stream, user),
				)
				if first.status != http.StatusOK {
					t.Fatalf("initial request returned %d: %s", first.status, first.body)
				}

				drainConformanceRequest(t, upstream)
				text := cursorReplyText(t, first.body, stream)
				visible, _, found := replay.StripReference(text)
				if !found || !strings.Contains(visible, "> Checked constraints.") ||
					!strings.HasSuffix(visible, "ok") {
					t.Fatalf(
						"reply did not include visible thinking, the answer and a reference: %q",
						text,
					)
				}

				for _, hidden := range []string{
					"opaque-test-payload",
					"rs_private",
					"clyde-thinking",
				} {
					if strings.Contains(text, hidden) {
						t.Fatalf("reply contains hidden provider metadata %q", hidden)
					}
				}

				quoted, err := json.Marshal(text)
				if err != nil {
					t.Fatal(err)
				}

				next := postConformance(
					t,
					url,
					fmt.Sprintf(
						`{"model":"gpt-future","messages":[%s,{"role":"assistant","content":%s},{"role":"user","content":"continue"}]}`,
						user,
						quoted,
					),
				)
				if next.status != http.StatusOK {
					t.Fatalf("continuation returned %d: %s", next.status, next.body)
				}

				for _, item := range drainConformanceRequest(t, upstream).Input {
					if item.Type == codexwire.ItemTypeReasoning && item.ID == "rs_private" &&
						item.EncryptedContent == "opaque-test-payload" && len(item.Summary) == 1 &&
						item.Summary[0].Text == "Checked constraints." {
						return
					}
				}

				t.Fatal("continuation did not replay the stored reasoning item")
			},
		)
	}
}

func TestCursorReplyDoesNotPublishReferenceWhenStorageFails(t *testing.T) {
	state := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(state, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := replay.Store{Path: filepath.Join(state, "replay.db")}
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	listeners := startConformanceServerWithStore(t, upstream, baseConfig(), store)

	for _, stream := range []bool{true, false} {
		response := postConformance(
			t,
			listeners.cursor+"/v1/chat/completions",
			fmt.Sprintf(
				`{"model":"gpt-future","stream":%t,"messages":[{"role":"user","content":"inspect"}]}`,
				stream,
			),
		)
		drainConformanceRequest(t, upstream)
		body := string(response.body)
		if !strings.Contains(body, `"error"`) || strings.Contains(body, `"finish_reason":"stop"`) ||
			strings.Contains(body, "clyde-replay") {
			t.Fatalf(
				"storage failure returned an unexpected response for stream=%t: %s",
				stream,
				body,
			)
		}
	}
}

func TestCursorToolResultContinuesReplyWithoutMarker(t *testing.T) {
	for _, test := range []struct {
		name      string
		answer    string
		continues bool
	}{
		{"tool call and generated thinking", "  ", true},
		{"tool call and model text", "> **💭 Thinking...**", false},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				upstream := newConformanceUpstream(conformanceUsageWithReasoning)
				upstream.reply = func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(toolReplySSE(test.answer)))
				}

				url := startConformanceServer(t, upstream).cursor + "/v1/chat/completions"
				const user = `{"role":"user","content":"inspect the file"}`
				first := postConformance(
					t,
					url,
					`{"model":"gpt-future","stream":true,"messages":[`+user+`]}`,
				)
				drainConformanceRequest(t, upstream)
				if first.status != http.StatusOK ||
					!strings.Contains(string(first.body), `"id":"call_read"`) ||
					!strings.Contains(string(first.body), `"name":"read_file"`) ||
					!strings.Contains(string(first.body), `"arguments":"{}"`) {
					t.Fatalf(
						"initial stream did not include the expected tool call: %s",
						first.body,
					)
				}

				visible, before, found := replay.StripReference(
					cursorReplyText(t, first.body, true),
				)
				if !found || !strings.Contains(visible, "> Inspecting the file.") {
					t.Fatalf(
						"initial stream omitted the reasoning or reply reference: %s",
						first.body,
					)
				}

				const assistant = `{"role":"assistant","tool_calls":[{"id":"call_read","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`
				const result = `{"role":"tool","tool_call_id":"call_read","content":"read complete"}`
				next := postConformance(
					t,
					url,
					`{"model":"gpt-future","messages":[`+user+","+assistant+","+result+`]}`,
				)
				drainConformanceRequest(t, upstream)
				if next.status != http.StatusOK {
					t.Fatalf("tool continuation returned %d: %s", next.status, next.body)
				}

				_, after, found := replay.StripReference(cursorReplyText(t, next.body, false))
				if !found || before.ReplyID == after.ReplyID ||
					(before.ConversationID == after.ConversationID) != test.continues {
					t.Fatalf("incorrect tool continuation: before=%+v after=%+v", before, after)
				}
			},
		)
	}
}

func toolReplySSE(answer string) string {
	return strings.Join(
		[]string{
			"event: response.output_item.done",
			`data: {"item":{"type":"reasoning","id":"rs_tool","summary":[{"type":"summary_text","text":"Inspecting the file."}],"encrypted_content":"opaque-tool-reasoning"}}`,
			"",
			"event: response.output_text.delta",
			`data: {"delta":` + strconv.Quote(answer) + `}`,
			"",
			"event: response.output_item.added",
			`data: {"item":{"id":"fc_read","type":"function_call","call_id":"call_read","name":"read_file","arguments":""}}`,
			"",
			"event: response.function_call_arguments.delta",
			`data: {"item_id":"fc_read","delta":"{}"}`,
			"",
			"event: response.output_item.done",
			`data: {"item":{"id":"fc_read","type":"function_call","call_id":"call_read","name":"read_file","arguments":"{}"}}`,
			"",
			"event: response.completed",
			`data: {"type":"response.completed","response":{"id":"resp-tool","usage":` + conformanceUsageWithReasoning + `}}`,
			"",
			"",
		},
		"\n",
	)
}

func TestCursorReplyDoesNotSaveInterruptedStream(t *testing.T) {
	store := testReplyStore(t)
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	upstream.reply = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		const stream = "event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"partial answer"}` + "\n\n" +
			"event: response.failed\n" +
			`data: {"type":"response.failed","error":{"message":"upstream failed"}}` + "\n\n"
		_, _ = w.Write([]byte(stream))
	}

	listeners := startConformanceServerWithStore(t, upstream, baseConfig(), store)
	response := postConformance(
		t,
		listeners.cursor+"/v1/chat/completions",
		`{"model":"gpt-future","stream":true,"messages":[{"role":"user","content":"inspect"}]}`,
	)
	drainConformanceRequest(t, upstream)
	body := string(response.body)
	if !strings.Contains(body, "partial answer") || !strings.Contains(body, `"error"`) ||
		strings.Contains(body, "clyde-replay") {
		t.Fatalf("unexpected interrupted stream: %s", body)
	}

	if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
		t.Fatalf("interrupted stream created a replay database: %v", err)
	}
}

func TestCollectedReplySavesSignedBlocksAndGeneratedNotices(t *testing.T) {
	t.Parallel()
	pending := &pendingReply{
		ref:   replay.Reference{ConversationID: "chat", ReplyID: "reply"},
		store: replay.Store{Path: filepath.Join(t.TempDir(), "replay.db")},
	}

	ctx := context.WithValue(t.Context(), pendingReplyKey{}, pending)
	server, _ := newLoggingServer(t, config.LoggingConfig{})
	const thinking = " first\n>quoted \n"
	const answer = "> **💭 Thinking...**\n> answer quoted by the model"
	source, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}

	response := openai.ChatResponse{
		Choices: []openai.ChatChoice{
			{
				Message: openai.ChatMessage{
					Role:    "assistant",
					Content: source,
				},
			},
		},
	}
	events := []render.Event{
		render.ReasoningDelta{Text: thinking},
		render.ReasoningDelta{Signature: "signed-body"},
		render.ReasoningFinished{},
		render.ReasoningSignaled{ReasoningKind: "redacted"},
		render.ReasoningDelta{
			ReasoningKind: "redacted",
			Text:          "[redacted]",
			RedactedData:  "redacted-body",
		},
		render.ReasoningFinished{},
		render.TextDelta{Text: answer},
	}

	if err := server.saveCollectedReply(
		ctx,
		"anthropic",
		&response,
		events,
		[]runtime.UsageNotice{{Text: "quota warning"}},
	); err != nil {
		t.Fatal(err)
	}

	var reply string
	if err := json.Unmarshal(response.Choices[0].Message.Content, &reply); err != nil {
		t.Fatal(err)
	}

	visible, ref, found := replay.StripReference(reply)
	if !found || !strings.Contains(visible, "> quota warning") ||
		strings.Contains(visible, "signed-body") || strings.Contains(visible, "redacted-body") {
		t.Fatalf("unexpected collected reply: %q", reply)
	}

	record, found, err := pending.store.Load(ctx, ref)
	if err != nil || !found {
		t.Fatalf("read stored reply: found=%t err=%v", found, err)
	}

	parts, ok := record.Parts(visible)
	if !ok || len(parts) != 4 || parts[0].Body != thinking || parts[0].Signature != "signed-body" ||
		parts[1].Kind != render.SyntheticRedactedThinking || parts[1].Encrypted != "redacted-body" ||
		parts[2].Kind != render.SyntheticKindText || parts[2].Body != answer ||
		parts[3].Kind != render.SyntheticNotice {
		t.Fatalf("incorrect replay parts: %+v", parts)
	}
}

func cursorReplyText(t *testing.T, body []byte, stream bool) string {
	t.Helper()
	if stream {
		var text strings.Builder

		for _, frame := range requireSingleTrailingDone(t, sseDataFrames(t, body)) {
			var chunk openai.StreamChunk
			if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
				t.Fatal(err)
			}

			for _, choice := range chunk.Choices {
				text.WriteString(choice.Delta.Content)
			}
		}

		return text.String()
	}

	var response openai.ChatResponse
	if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) != 1 {
		t.Fatalf("decode collected reply: %v: %s", err, body)
	}

	var text string
	if err := json.Unmarshal(response.Choices[0].Message.Content, &text); err != nil {
		t.Fatal(err)
	}

	return text
}
