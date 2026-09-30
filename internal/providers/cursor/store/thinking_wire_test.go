package cursorstore

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"reflect"
	"testing"
)

//go:embed thinking_wire_test.sql
var insertThinkingBubbleSQL string

func TestCursorThinkingWireThroughRealStore(t *testing.T) {
	cases := []struct {
		name     string
		thinking json.RawMessage
		want     string
		invalid  bool
	}{
		{name: "string", thinking: json.RawMessage(`"reasoning from string"`), want: "reasoning from string"},
		{name: "object", thinking: json.RawMessage(`{"text":"reasoning from object"}`), want: "reasoning from object"},
		{name: "null", thinking: json.RawMessage(`null`)},
		{name: "absent"},
		{name: "number", thinking: json.RawMessage(`42`), invalid: true},
		{name: "array", thinking: json.RawMessage(`[]`), invalid: true},
		{name: "boolean", thinking: json.RawMessage(`true`), invalid: true},
		{name: "invalid_object", thinking: json.RawMessage(`{"text":42}`), invalid: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := createCursorStoreTestDatabase(t)
			insertThinkingBubble(t, path, "wire", "bubble", testCase.thinking, "")
			discovered := ReadGlobalDiscovery(t.Context(), path)
			if discovered.Err != nil {
				t.Fatalf("discover actual store: %v", discovered.Err)
			}
			if testCase.want != "" && !discovered.Stocks["wire"].HasContent {
				t.Fatal("discovery omitted thinking-only content")
			}
			db, err := OpenReadOnlyDatabase(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			header := ComposerHeader{ComposerID: "wire", FullConversationHeadersOnly: []ComposerBubbleRef{{BubbleID: "bubble", Type: BubbleTypeAssistant}}}
			var got []Bubble
			err = StreamComposerBubbles(t.Context(), db, "wire", header, func(bubble Bubble) bool { got = append(got, bubble); return true })
			if testCase.invalid {
				if err == nil {
					t.Fatal("store accepted an invalid thinking representation")
				}
				return
			}
			if err != nil {
				t.Fatalf("stream actual store: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("thinking result count = %d, want 1", len(got))
			}
			if got[0].Thinking.Text != testCase.want {
				t.Fatalf("thinking text = %q, want %q", got[0].Thinking.Text, testCase.want)
			}
		})
	}
}

func TestCursorThinkingFingerprintsPreserveDistinctUnreferencedRows(t *testing.T) {
	path := createCursorStoreTestDatabase(t)
	insertThinkingBubble(t, path, "wire", "accepted", json.RawMessage(`{"text":"accepted reasoning"}`), "server-accepted")
	insertThinkingBubble(t, path, "wire", "copy", json.RawMessage(`"accepted reasoning"`), "")
	insertThinkingBubble(t, path, "wire", "distinct", json.RawMessage(`"distinct reasoning"`), "")
	db, err := OpenReadOnlyDatabase(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	header := ComposerHeader{ComposerID: "wire", FullConversationHeadersOnly: []ComposerBubbleRef{{BubbleID: "accepted", Type: BubbleTypeAssistant}}}
	var got []string
	err = StreamComposerBubbles(t.Context(), db, "wire", header, func(bubble Bubble) bool { got = append(got, bubble.BubbleID); return true })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"accepted", "distinct"}) {
		t.Fatalf("stored thinking identities = %v", got)
	}
}

func insertThinkingBubble(t *testing.T, path, composerID, bubbleID string, thinking json.RawMessage, serverID string) {
	t.Helper()
	value := struct {
		Version   int             `json:"_v"`
		Type      int             `json:"type"`
		BubbleID  string          `json:"bubbleId"`
		Thinking  json.RawMessage `json:"thinking,omitempty"`
		ServerID  string          `json:"serverBubbleId,omitempty"`
		CreatedAt string          `json:"createdAt"`
	}{Version: 3, Type: BubbleTypeAssistant, BubbleID: bubbleID, Thinking: thinking, ServerID: serverID, CreatedAt: "2026-09-30T12:00:00Z"}
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := db.ExecContext(t.Context(), insertThinkingBubbleSQL, bubbleKey(composerID, bubbleID), string(content)); err != nil {
		t.Fatal(err)
	}
}
