package parser_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providers/claude/parser"
	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/transcript"
)

// This environment variable supplies the path of the native transcript
// containing summary 350af2ea. The test skips without this variable.
const legacyRealTranscriptEnvironment = "CLYDE_LEGACY_TRANSCRIPT_PATH"

const (
	legacyRealTargetSummary   = "350af2ea-f49a-42f9-beed-0d4416ae8853"
	legacyRealResolvedEvent   = "providers.claude.parser.legacy_transcript_resolved"
	legacyRealCutSuffixSHA256 = "c0bd9c8d412a0bfebf8a1d113e881a2fc83fbc01501e8aac8021c6e656b0ec48"
	legacyRealCutSuffixBytes  = 4642
	legacyRealSummaries       = 3
	legacyRealPositions       = 939
	legacyRealDirectBlobs     = 448
	legacyRealNormalizedBlobs = 12
	legacyRealRetained        = 875
	legacyRealOuterUTF16      = 1534250
	legacyRealOuterRunes      = 1534247
	legacyRealProbeRunes      = 200
	legacyRealProbeMinimum    = 80
)

type legacyRealWrapper struct {
	summary string
	bytes   int
	sha256  string
}

var legacyRealWrappers = []legacyRealWrapper{
	{summary: "7a81bdc0-3281-4595-bbd0-9850eb1a7f18", bytes: 1521589, sha256: "ae4557b0c1400ab6062f236b988a5d16f9e825b15b9b8379ead357f92bb99f97"},
	{summary: "4d37ca0b-c5f2-4747-a636-f0e2ab207a20", bytes: 1511122, sha256: "acf1439db68158fc2fa5940e3e077af504bc03f37376fd1669a0def962dd0175"},
	{summary: legacyRealTargetSummary, bytes: 1535022, sha256: "e176228df4d45a773637353178ee694c476ac7833b8ac730d3874666762d9954"},
}

// legacyRealEvent stores the fields of the resolution event the parser logs
// for one summary.
type legacyRealEvent struct {
	Message           string `json:"msg"`
	SummaryUUID       string `json:"summary_uuid"`
	Summaries         int    `json:"summaries"`
	RequestPositions  int    `json:"request_positions"`
	DirectBlobs       int    `json:"direct_blobs"`
	NormalizedBlobs   int    `json:"normalized_blobs"`
	RetainedPositions int    `json:"retained_positions"`
}

// TestLegacyResolverRecoversRealTranscript loads the native transcript with
// typed span resolution and checks three reconstructed wrapper bodies. It
// checks the outer body length in UTF-16 units and runes, the partial first
// tool result, and logged request statistics. It also checks that a chat
// export of summary 350af2ea omits non-chat segment text.
func TestLegacyResolverRecoversRealTranscript(t *testing.T) {
	path := os.Getenv(legacyRealTranscriptEnvironment)
	if path == "" {
		t.Skip("set " + legacyRealTranscriptEnvironment + " to the native Claude transcript that contains summary " + legacyRealTargetSummary)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	index, record := legacyRealIndex(t, path)
	messages, err := index.LoadMessagesWithOptions(record, conversation.LoadOptions{
		IncludeSystemPrompts:       false,
		IncludeSystemMessages:      true,
		IncludeToolOutputs:         false,
		IncludeInjected:            false,
		HarnessTally:               nil,
		ResolveInjectedTranscripts: true,
	})
	if err != nil {
		t.Fatalf("load messages: %v", err)
	}
	spans := legacyRealSpans(messages)
	for _, wrapper := range legacyRealWrappers {
		body := spans[wrapper.summary].body
		if len(body) != wrapper.bytes || legacyRealDigest(body) != wrapper.sha256 {
			t.Errorf("summary %s body = %d bytes %s, want %d bytes %s", wrapper.summary, len(body), legacyRealDigest(body), wrapper.bytes, wrapper.sha256)
		}
		t.Logf("summary %s body: %d UTF-8 bytes, SHA256 %s", wrapper.summary, len(body), legacyRealDigest(body))
	}
	target := spans[legacyRealTargetSummary]
	utf16Units := len(utf16.Encode([]rune(target.body)))
	runes := utf8.RuneCountInString(target.body)
	if utf16Units != legacyRealOuterUTF16 || runes != legacyRealOuterRunes {
		t.Errorf("outer body = %d UTF-16 units and %d runes, want %d and %d", utf16Units, runes, legacyRealOuterUTF16, legacyRealOuterRunes)
	}
	t.Logf("outer body: %d UTF-16 units, %d runes", utf16Units, runes)
	assertLegacyRealCut(t, target.span)
	assertLegacyRealEvent(t, logs.Bytes())
	assertLegacyRealChatExport(t, index, record, messages, target.span)
}

func legacyRealIndex(t *testing.T, path string) (*conversation.Index, conversation.Record) {
	t.Helper()
	registry := conversation.NewRegistry()
	registry.Register(parser.New())
	index := conversation.NewIndex(registry, config.ConversationConfig{})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	record, ok := parser.New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !ok {
		t.Fatalf("scan record %s failed", path)
	}
	return index, record
}

type legacyRealSpan struct {
	span reorienttag.TranscriptSpan
	body string
}

// legacyRealSpans returns the first attached span and its wrapper body for
// each summary message.
func legacyRealSpans(messages []transcript.Message) map[string]legacyRealSpan {
	spans := make(map[string]legacyRealSpan)
	open := reorienttag.PreCompactionTranscriptOpen + "\n"
	closing := "\n" + reorienttag.PreCompactionTranscriptClose
	for _, message := range messages {
		if message.Compaction == nil || message.Compaction.Kind != transcript.CompactionKindSummary {
			continue
		}
		for _, item := range message.Compaction.ContextItems {
			if item.Message == nil || len(item.Message.Content) == 0 || len(item.Message.Content[0].TranscriptSpans) == 0 {
				continue
			}
			content := item.Message.Content[0]
			span := content.TranscriptSpans[0]
			spans[message.UUID] = legacyRealSpan{span: span, body: content.Text[span.Start+len(open) : span.End-len(closing)]}
		}
	}
	return spans
}

func assertLegacyRealCut(t *testing.T, span reorienttag.TranscriptSpan) {
	t.Helper()
	if len(span.Segments) == 0 {
		t.Fatal("target span has no segments")
	}
	first := span.Segments[0]
	if first.Kind != reorienttag.SegmentKindToolOutput || len(first.Text) != legacyRealCutSuffixBytes || legacyRealDigest(first.Text) != legacyRealCutSuffixSHA256 {
		t.Errorf("first segment = %s %d bytes %s, want %s %d bytes %s", first.Kind, len(first.Text), legacyRealDigest(first.Text), reorienttag.SegmentKindToolOutput, legacyRealCutSuffixBytes, legacyRealCutSuffixSHA256)
	}
	t.Logf("cut suffix: %s %d bytes, SHA256 %s", first.Kind, len(first.Text), legacyRealDigest(first.Text))
}

func assertLegacyRealEvent(t *testing.T, logs []byte) {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(logs))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event legacyRealEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Message != legacyRealResolvedEvent || event.SummaryUUID != legacyRealTargetSummary {
			continue
		}
		if event.Summaries != legacyRealSummaries || event.RequestPositions != legacyRealPositions || event.DirectBlobs != legacyRealDirectBlobs || event.NormalizedBlobs != legacyRealNormalizedBlobs || event.RetainedPositions != legacyRealRetained {
			t.Errorf("resolution event = %+v, want %d summaries, %d positions, %d direct blobs with %d normalized, %d retained positions", event, legacyRealSummaries, legacyRealPositions, legacyRealDirectBlobs, legacyRealNormalizedBlobs, legacyRealRetained)
		}
		t.Logf("resolution event: %+v", event)
		return
	}
	t.Fatalf("the parser did not log %s for summary %s", legacyRealResolvedEvent, legacyRealTargetSummary)
}

// assertLegacyRealChatExport exports the compaction segment starting with the
// target summary and selects only chat. For each non-chat segment of at least
// 80 bytes that does not occur in any chat segment, it checks that the export
// omits the first 200 runes.
func assertLegacyRealChatExport(t *testing.T, index *conversation.Index, record conversation.Record, messages []transcript.Message, span reorienttag.TranscriptSpan) {
	t.Helper()
	selector := ""
	for _, segment := range conversation.CompactionSegments(messages) {
		if segment.SummaryUUID == legacyRealTargetSummary {
			selector = strconv.Itoa(segment.Index)
		}
	}
	if selector == "" {
		t.Fatalf("no compaction segment starts with summary %s", legacyRealTargetSummary)
	}
	body, err := index.Export(record, conversation.ExportOptions{
		Format:       conversation.ExportFormatPlainText,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		MaxTokens:    "",
		TokenModel:   "",
		Whitespace:   conversation.WhitespacePreserve,
		Content:      conversation.NewContentKindSet(conversation.ContentKindChat),
		Compaction:   conversation.CompactionExportOptions{IncludeSelector: selector, FullHistory: false},
	})
	if err != nil {
		t.Fatalf("chat export of summary %s: %v", legacyRealTargetSummary, err)
	}
	var chat strings.Builder
	for _, segment := range span.Segments {
		if segment.Kind == reorienttag.SegmentKindChat {
			chat.WriteString(segment.Text)
		}
	}
	checked := 0
	for _, segment := range span.Segments {
		if segment.Kind == reorienttag.SegmentKindChat || len(segment.Text) < legacyRealProbeMinimum {
			continue
		}
		probe := string([]rune(segment.Text)[:min(legacyRealProbeRunes, utf8.RuneCountInString(segment.Text))])
		if strings.Contains(chat.String(), probe) {
			continue
		}
		checked++
		if strings.Contains(string(body), probe) || strings.Contains(string(body), legacyRealJSONText(t, probe)) {
			t.Errorf("chat export contains %s segment text %q", segment.Kind, probe)
		}
	}
	if checked == 0 {
		t.Fatal("no non-chat segment text was checked")
	}
	t.Logf("chat export: %d bytes; %d non-chat segment probes absent", len(body), checked)
}

func legacyRealJSONText(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("encode probe: %v", err)
	}
	return strings.Trim(string(encoded), `"`)
}

func legacyRealDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}
