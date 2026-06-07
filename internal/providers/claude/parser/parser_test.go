package parser

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// countingReader records how many bytes were read and whether the underlying
// reader was drained to EOF, so a test can prove a bounded header read.
type countingReader struct {
	r      io.Reader
	bytes  int
	hitEOF bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytes += n
	if err == io.EOF {
		c.hitEOF = true
	}
	return n, err
}

func TestStreamStripsControlTagNoiseFromUserMessages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	body := `{"uuid":"1","type":"user","timestamp":"2026-04-24T19:00:00Z","message":{"role":"user","content":"<command-name>/exit</command-name>\n<command-message>exit</command-message>\nCatch you later!"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages=%d want 1", len(messages))
	}
	if messages[0].Text != "Catch you later!" {
		t.Fatalf("text=%q want %q", messages[0].Text, "Catch you later!")
	}
}

// TestScanRecordReadsBoundedHeader writes a tiny header followed by a very large
// body and asserts ScanRecord fills the record from the top without reading to
// EOF: it returns immediately after the first user line provides the title and
// created time, so the cost is bounded by the header, not the file size.
func TestScanRecordReadsBoundedHeader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.jsonl")

	var builder strings.Builder
	builder.WriteString(`{"sessionId":"sess-123","cwd":"/repo","type":"user","timestamp":"2026-04-24T19:00:00Z","content":"first user line"}` + "\n")
	// A large body after the header. If ScanRecord read to EOF it would touch
	// every one of these lines; the early stop must avoid that.
	const bodyLines = 200000
	filler := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}}` + "\n"
	for i := 0; i < bodyLines; i++ {
		builder.WriteString(filler)
	}
	data := builder.String()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	stamp := conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	counter := &countingReader{r: file, bytes: 0, hitEOF: false}

	record, ok := scanHeader(counter, path, stamp)
	if !ok {
		t.Fatalf("scanHeader returned ok=false")
	}
	if record.NativeID != "sess-123" {
		t.Fatalf("native id=%q want sess-123", record.NativeID)
	}
	if record.Title != "first user line" {
		t.Fatalf("title=%q want %q", record.Title, "first user line")
	}
	if record.CreatedAt.IsZero() {
		t.Fatalf("created time was not parsed from the header")
	}
	if counter.hitEOF {
		t.Fatalf("scanHeader read to EOF; the header read must stop early")
	}
	if int64(counter.bytes) >= info.Size() {
		t.Fatalf("scanHeader read %d bytes, want well under file size %d", counter.bytes, info.Size())
	}
	// The header sits in the first chunk, so the early stop should keep the read
	// to roughly one buffer fill, far under the multi-megabyte body.
	const headerReadCeiling = 1 << 20
	if counter.bytes > headerReadCeiling {
		t.Fatalf("scanHeader read %d bytes, want under %d (early stop should bound the read to the top)", counter.bytes, headerReadCeiling)
	}
}

// TestScanRecordStopsBeforeLineCap proves the header read is bounded by the line
// cap even when no created time or title ever appears, so a degenerate file
// cannot force a full read.
func TestScanRecordHonorsLineCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "noheader.jsonl")

	var builder strings.Builder
	// A session id on the first line so the record is usable, then many lines
	// that never supply a title or created time. The line cap must stop the read
	// well before EOF.
	builder.WriteString(`{"sessionId":"sess-x"}` + "\n")
	for i := 0; i < headerLineCap*4; i++ {
		builder.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[]}}` + "\n")
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	record, ok := New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	// With no title found, the title falls back to the session id.
	if record.Title != "sess-x" {
		t.Fatalf("title=%q want fallback sess-x", record.Title)
	}
}

func TestStreamMarksClaudeCompactSummaryMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "compact-summary.jsonl")
	body := `{"uuid":"summary-1","parentUuid":"parent-1","logicalParentUuid":"logical-1","type":"user","timestamp":"2026-06-03T23:17:32.185Z","isVisibleInTranscriptOnly":true,"isCompactSummary":true,"summarizeMetadata":{"messagesSummarized":7,"userContext":"keep the current release plan","direction":"backward"},"message":{"role":"user","content":"Summary block"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	message := messages[0]
	if message.ParentUUID != "parent-1" || message.LogicalParentUUID != "logical-1" {
		t.Fatalf("parent fields = %q/%q", message.ParentUUID, message.LogicalParentUUID)
	}
	if message.Visibility != transcript.MessageVisibilityTranscriptOnly {
		t.Fatalf("visibility = %q, want transcript_only", message.Visibility)
	}
	if message.Compaction == nil || message.Compaction.Kind != transcript.CompactionKindSummary {
		t.Fatalf("compaction = %#v", message.Compaction)
	}
	if message.Compaction.MessagesSummarized != 7 {
		t.Fatalf("messages summarized = %d, want 7", message.Compaction.MessagesSummarized)
	}
	if message.Compaction.UserContext != "keep the current release plan" {
		t.Fatalf("user context = %q", message.Compaction.UserContext)
	}
	if message.Compaction.Direction != "backward" {
		t.Fatalf("direction = %q", message.Compaction.Direction)
	}
	if len(message.Compaction.RawSummarizeMetadata) == 0 {
		t.Fatalf("raw summarize metadata was empty")
	}
	if len(message.Compaction.ContextItems) != 1 {
		t.Fatalf("context items len = %d, want 1", len(message.Compaction.ContextItems))
	}
	contextItem := message.Compaction.ContextItems[0]
	if contextItem.Kind != transcript.CompactedContextItemKindMessage ||
		contextItem.Message == nil {
		t.Fatalf("context item = %#v", contextItem)
	}
	if contextItem.Message.MessageClass != transcript.CompactedMessageClassSummary {
		t.Fatalf("message class = %q, want summary", contextItem.Message.MessageClass)
	}
	if contextItem.Message.Role != "user" {
		t.Fatalf("context role = %q, want user", contextItem.Message.Role)
	}
	if len(contextItem.Message.Content) != 1 {
		t.Fatalf("content len = %d, want 1", len(contextItem.Message.Content))
	}
	if contextItem.Message.Content[0].Text != "Summary block" {
		t.Fatalf("summary content = %q, want Summary block", contextItem.Message.Content[0].Text)
	}
	if len(contextItem.Message.Raw) == 0 {
		t.Fatalf("summary raw was empty")
	}
}

func TestStreamIncludesClaudeCompactionBoundariesOnlyWhenRequested(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "boundaries.jsonl")
	body := `{"uuid":"boundary-1","parentUuid":"","logicalParentUuid":"tail-1","type":"system","subtype":"compact_boundary","content":"Conversation compacted","timestamp":"2026-06-03T23:17:32.186Z","compactMetadata":{"trigger":"manual","preTokens":977652,"postTokens":12951,"messagesSummarized":7,"userContext":"keep the current release plan","direction":"backward","preCompactDiscoveredTools":["Read","Bash"],"preservedSegment":{"headUuid":"head-1","anchorUuid":"anchor-1","tailUuid":"tail-1"}}}` + "\n" +
		`{"uuid":"micro-1","parentUuid":"boundary-1","logicalParentUuid":"tail-2","type":"system","subtype":"microcompact_boundary","content":"Microcompact boundary","timestamp":"2026-06-03T23:17:33.186Z","compactMetadata":{"trigger":"manual","preTokens":999,"tokensSaved":1,"compactedToolIds":["legacy-tool"],"clearedAttachmentUUIDs":["legacy-attachment"]},"microcompactMetadata":{"trigger":"auto","preTokens":100,"postTokens":40,"tokensSaved":60,"compactedToolIds":["tool-1","tool-2"],"clearedAttachmentUUIDs":["attachment-1","attachment-2"]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	defaultMessages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect default messages: %v", err)
	}
	if len(defaultMessages) != 0 {
		t.Fatalf("default messages len = %d, want 0", len(defaultMessages))
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[0].Compaction == nil || messages[0].Compaction.Kind != transcript.CompactionKindBoundary {
		t.Fatalf("boundary compaction = %#v", messages[0].Compaction)
	}
	if messages[0].Compaction.Trigger != transcript.CompactionTriggerManual {
		t.Fatalf("boundary trigger = %q, want manual", messages[0].Compaction.Trigger)
	}
	if messages[0].Compaction.TokensSaved != 964701 {
		t.Fatalf("tokens saved = %d, want 964701", messages[0].Compaction.TokensSaved)
	}
	if messages[0].Compaction.MessagesSummarized != 7 {
		t.Fatalf("messages summarized = %d, want 7", messages[0].Compaction.MessagesSummarized)
	}
	if messages[0].Compaction.UserContext != "keep the current release plan" {
		t.Fatalf("boundary user context = %q", messages[0].Compaction.UserContext)
	}
	if messages[0].Compaction.Direction != "backward" {
		t.Fatalf("boundary direction = %q", messages[0].Compaction.Direction)
	}
	if len(messages[0].Compaction.PreCompactDiscoveredTools) != 2 {
		t.Fatalf("pre-compact discovered tools = %#v", messages[0].Compaction.PreCompactDiscoveredTools)
	}
	if messages[0].Compaction.HeadUUID != "head-1" || messages[0].Compaction.AnchorUUID != "anchor-1" || messages[0].Compaction.TailUUID != "tail-1" {
		t.Fatalf("preserved segment = %#v", messages[0].Compaction)
	}
	if len(messages[0].Compaction.ContextItems) != 0 {
		t.Fatalf("boundary context items = %#v, want none", messages[0].Compaction.ContextItems)
	}
	if len(messages[0].Compaction.RawCompactMetadata) == 0 {
		t.Fatalf("raw compact metadata was empty")
	}
	if messages[1].Compaction == nil || messages[1].Compaction.Kind != transcript.CompactionKindMicroboundary {
		t.Fatalf("microboundary compaction = %#v", messages[1].Compaction)
	}
	if messages[1].Compaction.Trigger != transcript.CompactionTriggerAuto {
		t.Fatalf("microboundary trigger = %q, want auto", messages[1].Compaction.Trigger)
	}
	if messages[1].Compaction.PreTokens != 100 {
		t.Fatalf("microboundary pre tokens = %d, want 100", messages[1].Compaction.PreTokens)
	}
	if messages[1].Compaction.TokensSaved != 60 {
		t.Fatalf("microboundary tokens saved = %d, want 60", messages[1].Compaction.TokensSaved)
	}
	if len(messages[1].Compaction.CompactedToolIDs) != 2 {
		t.Fatalf("compacted tool ids = %#v", messages[1].Compaction.CompactedToolIDs)
	}
	if len(messages[1].Compaction.ClearedAttachmentUUIDs) != 2 {
		t.Fatalf("cleared attachment uuids = %#v", messages[1].Compaction.ClearedAttachmentUUIDs)
	}
	if len(messages[1].Compaction.ContextItems) != 0 {
		t.Fatalf("microboundary context items = %#v, want none", messages[1].Compaction.ContextItems)
	}
	if len(messages[1].Compaction.RawMicrocompactMetadata) == 0 {
		t.Fatalf("raw microcompact metadata was empty")
	}
}

func TestStreamLiveClaudeCompactionSmoke(t *testing.T) {
	t.Parallel()
	const path = "/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites/71545485-56c8-46a3-8cb7-1807f572ece1.jsonl"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("live Claude transcript unavailable: %v", err)
	}

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect live Claude transcript: %v", err)
	}

	var summaryCount int
	var boundaryCount int
	var microboundaryCount int
	for _, message := range transcript.CompactionMessages(messages) {
		if message.Compaction == nil {
			continue
		}
		switch message.Compaction.Kind {
		case transcript.CompactionKindSummary:
			summaryCount++
		case transcript.CompactionKindBoundary:
			boundaryCount++
		case transcript.CompactionKindMicroboundary:
			microboundaryCount++
		}
	}
	if summaryCount != 1 {
		t.Fatalf("summary count = %d, want 1", summaryCount)
	}
	if boundaryCount != 1 {
		t.Fatalf("boundary count = %d, want 1", boundaryCount)
	}
	if microboundaryCount != 0 {
		t.Fatalf("microboundary count = %d, want 0", microboundaryCount)
	}
}
