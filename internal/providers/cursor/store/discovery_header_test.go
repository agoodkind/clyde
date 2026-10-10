package cursorstore_test

import (
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

const sameLengthHeader = `{"composerId":"composer-a","name":"Alpha","createdAt":1710000000000,"lastUpdatedAt":1710000000100,` +
	`"status":"none","unifiedMode":"agent","forceMode":"chat","latestChatGenerationUUID":"request-one",` +
	`"fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":1},{"bubbleId":"bubble-2","type":2},{"bubbleId":"bubble-3","type":1}]}`

func writeComposerData(t *testing.T, path string, composerID string, value string) {
	t.Helper()
	db := openDiscoveryWriter(t, path)
	result, err := db.Exec(`UPDATE cursorDiskKV SET value = ? WHERE key = ?`, value, "composerData:"+composerID)
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		t.Fatalf("The database update changed %d composerData rows for %s and returned %v.", updated, composerID, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func decodedComposerHeader(t *testing.T, composerID string, value string) cursorstore.ComposerHeader {
	t.Helper()
	header, err := cursorstore.DecodeComposerHeaderJSON([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	header.ComposerID = composerID
	return header
}

func TestReadGlobalDiscoveryDetectsSameLengthHeaderEdits(t *testing.T) {
	root, _, _ := discoveryFixture(t)
	writeComposerData(t, root.GlobalDBPath, "composer-a", sameLengthHeader)
	initial := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	want := cursorstore.ComposerHeader{
		ComposerID: "composer-a", Name: "Alpha", CreatedAt: 1710000000000, LastUpdatedAt: 1710000000100,
		Status: "none", UnifiedMode: "agent", ForceMode: "chat", LatestChatGenerationUUID: "request-one",
		FullConversationHeadersOnly: []cursorstore.ComposerBubbleRef{
			{BubbleID: "bubble-1", Type: 1}, {BubbleID: "bubble-2", Type: 2}, {BubbleID: "bubble-3", Type: 1},
		},
	}
	if initial.Err != nil || !reflect.DeepEqual(initial.Headers["composer-a"], want) {
		t.Fatalf("The initial header is %+v. The read returned %v.", initial.Headers["composer-a"], initial.Err)
	}
	edits := []struct{ name, before, after string }{
		{"leading bubble order", `{"bubbleId":"bubble-1","type":1},{"bubbleId":"bubble-2","type":2}`, `{"bubbleId":"bubble-2","type":2},{"bubbleId":"bubble-1","type":1}`},
		{"first bubble id", `[{"bubbleId":"bubble-2"`, `[{"bubbleId":"bubble-9"`},
		{"middle bubble type", `"bubble-1","type":1`, `"bubble-1","type":2`},
		{"name", `"name":"Alpha"`, `"name":"Bravo"`},
		{"created time", `"createdAt":1710000000000`, `"createdAt":1710000000001`},
		{"updated time", `"lastUpdatedAt":1710000000100`, `"lastUpdatedAt":1710000000200`},
		{"status", `"status":"none"`, `"status":"done"`},
		{"unified mode", `"unifiedMode":"agent"`, `"unifiedMode":"debug"`},
		{"force mode", `"forceMode":"chat"`, `"forceMode":"edit"`},
		{"request identity", `"request-one"`, `"request-two"`},
	}
	value := sameLengthHeader
	for _, edit := range edits {
		next := strings.Replace(value, edit.before, edit.after, 1)
		if next == value || len(next) != len(value) {
			t.Fatalf("The %s edit did not produce a different value of the same length.", edit.name)
		}
		value = next
		writeComposerData(t, root.GlobalDBPath, "composer-a", value)
		got := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
		if got.Err != nil || !reflect.DeepEqual(got.Headers["composer-a"], decodedComposerHeader(t, "composer-a", value)) {
			t.Errorf("The %s edit produced header %+v and error %v.", edit.name, got.Headers["composer-a"], got.Err)
		}
		if !reflect.DeepEqual(got.Stocks["composer-a"], initial.Stocks["composer-a"]) {
			t.Errorf("The %s edit changed the bubble stock to %+v.", edit.name, got.Stocks["composer-a"])
		}
	}
}

func TestReadGlobalDiscoveryAppliesDecoderTypeRules(t *testing.T) {
	root, _, _ := discoveryFixture(t)
	numericStatus := `{"composerId":"composer-typed","name":"Typed","status":1.0e0,"fullConversationHeadersOnly":[]}`
	textStatus := `{"composerId":"composer-typed","name":"Typed","status":"1.0","fullConversationHeadersOnly":[]}`
	if len(numericStatus) != len(textStatus) {
		t.Fatal("Status fixtures differ in length.")
	}
	if _, err := cursorstore.DecodeComposerHeaderJSON([]byte(numericStatus)); err == nil {
		t.Fatal("The decoder accepted a numeric status.")
	}
	execDiscoveryStatements(t, root.GlobalDBPath, `INSERT INTO cursorDiskKV VALUES ('composerData:composer-typed', '{}')`)
	writeComposerData(t, root.GlobalDBPath, "composer-typed", numericStatus)
	rejected := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if _, found := rejected.Headers["composer-typed"]; found || rejected.Err != nil {
		t.Fatalf("The read returned header %+v and error %v for a numeric status.", rejected.Headers["composer-typed"], rejected.Err)
	}
	writeComposerData(t, root.GlobalDBPath, "composer-typed", textStatus)
	accepted := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if !reflect.DeepEqual(accepted.Headers["composer-typed"], decodedComposerHeader(t, "composer-typed", textStatus)) {
		t.Fatalf("The same-length type correction did not produce the expected header: %+v.", accepted.Headers["composer-typed"])
	}
	for _, value := range []string{
		`{"name":null,"createdAt":null,"status":null,"fullConversationHeadersOnly":null}`,
		`{"name":"first","name":"second","Status":"mixed case key","fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":1}]}`,
	} {
		writeComposerData(t, root.GlobalDBPath, "composer-typed", value)
		got := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
		if !reflect.DeepEqual(got.Headers["composer-typed"], decodedComposerHeader(t, "composer-typed", value)) {
			t.Fatalf("The header for %s is %+v.", value, got.Headers["composer-typed"])
		}
	}
	previous := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath).Headers["composer-typed"]
	for _, value := range []string{
		`{"name":"wrong","createdAt":"1710000000000","fullConversationHeadersOnly":[]}`,
		`{"name":"wrong","fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":"1"}]}`,
	} {
		writeComposerData(t, root.GlobalDBPath, "composer-typed", value)
		got := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
		if !reflect.DeepEqual(got.Headers["composer-typed"], previous) {
			t.Fatalf("The value %s with the wrong field type replaced the earlier header with %+v.", value, got.Headers["composer-typed"])
		}
	}
}

func TestParserExportFollowsSameLengthHeaderReorder(t *testing.T) {
	root, _, parser := discoveryFixture(t)
	execDiscoveryStatements(t, root.GlobalDBPath,
		`UPDATE cursorDiskKV SET value = json_remove(value, '$.createdAt') WHERE key LIKE 'bubbleId:composer-a:%'`)
	_, records := discoverRecords(t, parser, nil)
	ordered := `{"composerId":"composer-a","fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":1},{"bubbleId":"bubble-2","type":2}]}`
	reordered := `{"composerId":"composer-a","fullConversationHeadersOnly":[{"bubbleId":"bubble-2","type":2},{"bubbleId":"bubble-1","type":1}]}`
	writeComposerData(t, root.GlobalDBPath, "composer-a", ordered)
	before, records := discoverRecords(t, parser, records)
	writeComposerData(t, root.GlobalDBPath, "composer-a", reordered)
	after, _ := discoverRecords(t, parser, records)
	if before["composer-a"].Stamp.Equal(after["composer-a"].Stamp) {
		t.Fatal("Header reorder did not change the candidate stamp.")
	}
	messages, err := conversation.CollectMessages(parser.Stream(after["composer-a"].Path, conversation.LoadOptions{}))
	if err != nil || len(messages) != 2 || messages[0].Text != "last" || messages[1].Text != "first" {
		t.Fatalf("The export returned messages %+v and error %v after the header reorder.", messages, err)
	}
}
