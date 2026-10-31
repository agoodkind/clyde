package parser_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const (
	fixtureSchemaPath      = "testdata/source_times_schema.sql"
	fixtureInsertPath      = "testdata/source_times_insert_kv.sql"
	fixtureBubbleVersion   = 3
	fixtureUserType        = 1
	fixtureAssistantType   = 2
	fixtureComposerCreated = 1779267600000
	fixtureComposerUpdated = 1779268000000
	fixtureToolName        = "run_terminal_command_v2"
	fixtureToolStatus      = "completed"
	fixtureStatePrefix     = "~"
	fixtureFieldReference  = 0x0a
	fixtureBackslash       = rune(92)
)

type fixtureToolFormer struct {
	Name       string `json:"name"`
	RawArgs    string `json:"rawArgs"`
	Status     string `json:"status"`
	ToolCallID string `json:"toolCallId"`
}

// fixtureBubble stores a Cursor bubble for a native store fixture.
type fixtureBubble struct {
	Version        int                `json:"_v"`
	Type           int                `json:"type"`
	BubbleID       string             `json:"bubbleId"`
	Text           string             `json:"text"`
	CreatedAt      string             `json:"createdAt"`
	ServerBubbleID string             `json:"serverBubbleId,omitempty"`
	ToolFormerData *fixtureToolFormer `json:"toolFormerData,omitempty"`
	// Unreferenced excludes the bubble from the composer headers.
	Unreferenced bool `json:"-"`
}

type fixtureHeaderRef struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"`
}

type fixtureComposer struct {
	ComposerID        string             `json:"composerId"`
	Name              string             `json:"name"`
	CreatedAt         int64              `json:"createdAt"`
	LastUpdatedAt     int64              `json:"lastUpdatedAt"`
	ConversationState string             `json:"conversationState,omitempty"`
	Headers           []fixtureHeaderRef `json:"fullConversationHeadersOnly"`
}

type fixturePart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
}

type fixtureNativeMessage struct {
	Role    string        `json:"role"`
	Content []fixturePart `json:"content"`
}

type fixtureLinePart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type fixtureLineMessage struct {
	Content []fixtureLinePart `json:"content"`
}

type fixtureLine struct {
	Role    string             `json:"role"`
	Message fixtureLineMessage `json:"message"`
}

type fixtureCommandArgs struct {
	Command          string `json:"command"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

// nativeStoreFixture contains messages and bubbles for a Cursor native store.
type nativeStoreFixture struct {
	composerID string
	messages   []fixtureNativeMessage
	omitted    map[int]bool
	bubbles    []fixtureBubble
}

type kvRow struct {
	key  string
	text string
	blob []byte
}

// mustMarshal returns a function that fails the test on a marshal error.
func mustMarshal(t *testing.T) func([]byte, error) []byte {
	t.Helper()
	return func(data []byte, err error) []byte {
		if err != nil {
			t.Fatalf("cannot marshal fixture JSON: %v", err)
		}
		return data
	}
}

// writeKVStore creates a SQLite store and inserts the fixture rows.
func writeKVStore(t *testing.T, dbPath string, rows []kvRow) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("cannot create fixture store directory: %v", err)
	}
	schema, err := os.ReadFile(fixtureSchemaPath)
	if err != nil {
		t.Fatalf("cannot read fixture schema: %v", err)
	}
	insert, err := os.ReadFile(fixtureInsertPath)
	if err != nil {
		t.Fatalf("cannot read fixture insert statement: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("cannot open fixture store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("cannot create fixture schema: %v", err)
	}
	for _, row := range rows {
		if row.blob != nil {
			_, err = db.Exec(string(insert), row.key, row.blob)
		} else {
			_, err = db.Exec(string(insert), row.key, row.text)
		}
		if err != nil {
			t.Fatalf("cannot insert fixture row; key: %s: %v", row.key, err)
		}
	}
}

// writeNativeStore writes native messages, bubbles, and composer data to a store.
func writeNativeStore(t *testing.T, rootDir string, fixture nativeStoreFixture) {
	t.Helper()
	rows := make([]kvRow, 0, len(fixture.messages)+len(fixture.bubbles)+1)
	var state []byte
	written := map[string]bool{}
	for index, message := range fixture.messages {
		data := mustMarshal(t)(json.Marshal(message))
		digest := sha256.Sum256(data)
		state = append(state, fixtureFieldReference, byte(len(digest)))
		state = append(state, digest[:]...)
		key := hex.EncodeToString(digest[:])
		if fixture.omitted[index] || written[key] {
			continue
		}
		written[key] = true
		rows = append(rows, kvRow{key: "agentKv:blob:" + key, text: "", blob: data})
	}
	composer := fixtureComposer{
		ComposerID: fixture.composerID, Name: "Fixture", CreatedAt: fixtureComposerCreated, LastUpdatedAt: fixtureComposerUpdated,
		ConversationState: "", Headers: make([]fixtureHeaderRef, 0, len(fixture.bubbles)),
	}
	if len(fixture.messages) > 0 {
		composer.ConversationState = fixtureStatePrefix + base64.StdEncoding.EncodeToString(state)
	}
	for _, bubble := range fixture.bubbles {
		if !bubble.Unreferenced {
			composer.Headers = append(composer.Headers, fixtureHeaderRef{BubbleID: bubble.BubbleID, Type: bubble.Type})
		}
		key := "bubbleId:" + fixture.composerID + ":" + bubble.BubbleID
		rows = append(rows, kvRow{key: key, text: string(mustMarshal(t)(json.Marshal(bubble))), blob: nil})
	}
	rows = append(rows, kvRow{key: "composerData:" + fixture.composerID, text: string(mustMarshal(t)(json.Marshal(composer))), blob: nil})
	writeKVStore(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), rows)
}

func userBubble(bubbleID string, text string, created string) fixtureBubble {
	return fixtureBubble{Version: fixtureBubbleVersion, Type: fixtureUserType, BubbleID: bubbleID, Text: text, CreatedAt: created, ServerBubbleID: "", ToolFormerData: nil, Unreferenced: false}
}

func assistantBubble(bubbleID string, text string, created string) fixtureBubble {
	return fixtureBubble{Version: fixtureBubbleVersion, Type: fixtureAssistantType, BubbleID: bubbleID, Text: text, CreatedAt: created, ServerBubbleID: "", ToolFormerData: nil, Unreferenced: false}
}

func toolBubble(bubbleID string, created string, callID string) fixtureBubble {
	bubble := assistantBubble(bubbleID, "", created)
	bubble.ToolFormerData = &fixtureToolFormer{Name: fixtureToolName, RawArgs: "", Status: fixtureToolStatus, ToolCallID: callID}
	return bubble
}

func userMessage(text string) fixtureNativeMessage {
	return fixtureNativeMessage{Role: "user", Content: []fixturePart{{Type: "text", Text: text, ToolCallID: "", ToolName: "", Args: nil}}}
}

func assistantMessage(parts ...fixturePart) fixtureNativeMessage {
	return fixtureNativeMessage{Role: "assistant", Content: parts}
}

func callPart(callID string, args json.RawMessage) fixturePart {
	return fixturePart{Type: "tool-call", Text: "", ToolCallID: callID, ToolName: "Shell", Args: args}
}

func commandArgs(t *testing.T, command string, workingDirectory string) json.RawMessage {
	t.Helper()
	return mustMarshal(t)(json.Marshal(fixtureCommandArgs{Command: command, WorkingDirectory: workingDirectory}))
}

func fixtureLineText(t *testing.T, line fixtureLine) string {
	t.Helper()
	return string(mustMarshal(t)(json.Marshal(line)))
}

func userLine(t *testing.T, text string) string {
	t.Helper()
	return fixtureLineText(t, fixtureLine{Role: "user", Message: fixtureLineMessage{Content: []fixtureLinePart{{Type: "text", Text: text, Name: "", Input: nil}}}})
}

func assistantTextLine(t *testing.T, text string) string {
	t.Helper()
	return fixtureLineText(t, fixtureLine{Role: "assistant", Message: fixtureLineMessage{Content: []fixtureLinePart{{Type: "text", Text: text, Name: "", Input: nil}}}})
}

func toolLine(t *testing.T, inputs ...json.RawMessage) string {
	t.Helper()
	parts := make([]fixtureLinePart, 0, len(inputs))
	for _, input := range inputs {
		parts = append(parts, fixtureLinePart{Type: "tool_use", Text: "", Name: "Shell", Input: input})
	}
	return fixtureLineText(t, fixtureLine{Role: "assistant", Message: fixtureLineMessage{Content: parts}})
}

// jsonUnicodeEscape returns a Unicode escape for a rune.
func jsonUnicodeEscape(value rune) string {
	return string(fixtureBackslash) + fmt.Sprintf("u%04x", value)
}
