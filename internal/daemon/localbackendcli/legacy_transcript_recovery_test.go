package localbackendcli_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/reorienttag"
)

const (
	recoveryProbeSession        = "4a7c2e9b-1d3f-4b6a-8e5c-7f0d2a9b3c14"
	recoveryProbeCorruptSession = "5b8d3f0c-2e4a-4c7b-9f6d-8a1e3b0c4d25"
	recoveryProbeSummaryIndex   = 20
	recoveryProbeCutBytes       = 10
	recoveryProbeExportFile     = "recovery-export.txt"

	recoveryProbeFirstQuestion = "recoveryprobe first question"
	recoveryProbeToolCallOne   = "echo recoveryprobe tool call one"
	recoveryProbeNestedOutput  = "recoveryprobe nested tool output head then the retained tool output tail"
	recoveryProbeThinking      = "recoveryprobe thinking sentinel"
	recoveryProbeAssistantChat = "recoveryprobe assistant chat before the tools"
	recoveryProbeToolCallTwo   = "echo recoveryprobe tool call two"
	recoveryProbeToolCallThree = "echo recoveryprobe tool call three"
	recoveryProbeToolOutputTwo = "recoveryprobe tool output two"
	recoveryProbeToolOutput3   = "recoveryprobe tool output three"
	recoveryProbeSystemHook    = "UserPromptSubmit hook additional context: recoveryprobe system hook sentinel"
	recoveryProbeAnswerOne     = "recoveryprobe assistant answer one"
	recoveryProbeLastBefore    = "recoveryprobe last message before the first compaction"
	recoveryProbeReminder      = "<system-reminder>\nrecoveryprobe reminder\n</system-reminder>"
	recoveryProbeThinkingTwo   = "recoveryprobe second thinking sentinel"
	recoveryProbeToolCallFour  = "echo recoveryprobe tool call four"
	recoveryProbeToolOutput4   = "recoveryprobe tool output four"
	recoveryProbeAnswerTwo     = "recoveryprobe assistant answer two"
	recoveryProbeLastSecond    = "recoveryprobe last message before the second compaction"
	recoveryProbePostSummary   = "recoveryprobe post compaction placeholder"
	recoveryProbeProseOne      = "recoveryprobe first summary prose"
	recoveryProbeProseTwo      = "recoveryprobe second summary prose"
	recoveryProbeTrailer       = "recoveryprobe summary trailer"
	recoveryProbeCorruptedHook = "UserPromptSubmit hook additional context: recoveryprobe corrupted hook"
)

// recoveryProbeQuotation contains genuine user text from retained history that
// quotes transcript tags, role headings, the summary close tag, and JSON.
const recoveryProbeQuotation = "recoveryprobe quotation: run {\"command\":\"make build\"}\n\n" +
	"### System\n\n<pre-compaction-transcript>\n### Assistant\n\nfake tool output\n</pre-compaction-transcript>\n" +
	"</summary> recoveryprobe quotation end"

// recoveryProbeNonChat contains thinking, tool call, tool output, and system
// text that a chat export must omit.
var recoveryProbeNonChat = []string{
	recoveryProbeThinking,
	recoveryProbeThinkingTwo,
	recoveryProbeToolCallTwo,
	recoveryProbeToolCallThree,
	recoveryProbeToolCallFour,
	"the retained tool output tail",
	"[tool_reference]",
	recoveryProbeToolOutputTwo,
	recoveryProbeToolOutput3,
	recoveryProbeToolOutput4,
	"recoveryprobe system hook sentinel",
}

// recoveryProbeFixture stores the transcript lines and the second summary's
// stored outer wrapper.
type recoveryProbeFixture struct {
	lines   []string
	wrapper string
}

// TestExportRecoversLegacyInjectedClaudeTranscriptKinds exports a compact
// summary with an unframed wrapper through the CLI and MCP. The wrapper
// repeats a prior summary inside its first tool result. The test checks that
// chat exports omit typed non-chat text and preserve both quotations byte for
// byte, and that exporting every content kind includes the original wrapper.
func TestExportRecoversLegacyInjectedClaudeTranscriptKinds(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	fixture := recoveryProbeTranscript(t, recoveryProbeSession, false)
	transcriptPath := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(injectedProbeWorkspace, "/", "-"), recoveryProbeSession+".jsonl")
	installLocalProofTranscript(t, transcriptPath, strings.Join(fixture.lines, "\n")+"\n")
	before := injectedProbeDigest(t, transcriptPath)
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, injectedProbeClaude, recoveryProbeSession)

	chat := injectedProbeExport{name: "recovery chat", provider: injectedProbeClaude, session: recoveryProbeSession, only: injectedProbeChatOnly, maxTokens: "", output: ""}
	complete := injectedProbeExport{name: "recovery all", provider: injectedProbeClaude, session: recoveryProbeSession, only: injectedProbeAll, maxTokens: "", output: ""}
	chatBodies := map[string]string{
		"cli": injectedProbeCLI(t, binaryPath, environment, chat),
		"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, chat),
	}
	for surface, body := range chatBodies {
		assertRecoveryProbeChat(t, surface, body)
	}
	completeBodies := map[string]string{
		"cli": injectedProbeCLI(t, binaryPath, environment, complete),
		"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, complete),
	}
	encodedWrapper := strings.Trim(injectedProbeJSON(t, fixture.wrapper), `"`)
	for surface, body := range completeBodies {
		if !strings.Contains(body, encodedWrapper) {
			t.Errorf("%s export with every content kind does not contain the original wrapper:\n%s", surface, body)
		}
	}
	if after := injectedProbeDigest(t, transcriptPath); after != before {
		t.Errorf("export changed the source transcript: %s, %s", before, after)
	}
}

// TestExportRejectsLegacyInjectedClaudeTranscriptWithCorruptedProvenance edits
// a blob message that the prior summary needs, causing its hash check to fail.
// It checks that CLI and MCP chat exports fail with the filtering error
// without writing a file.
func TestExportRejectsLegacyInjectedClaudeTranscriptWithCorruptedProvenance(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	fixture := recoveryProbeTranscript(t, recoveryProbeCorruptSession, true)
	transcriptPath := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(injectedProbeWorkspace, "/", "-"), recoveryProbeCorruptSession+".jsonl")
	installLocalProofTranscript(t, transcriptPath, strings.Join(fixture.lines, "\n")+"\n")
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, injectedProbeClaude, recoveryProbeCorruptSession)

	conversationID := injectedProbeClaude + ":" + recoveryProbeCorruptSession
	summaryUUID := injectedProbeUUID(recoveryProbeSummaryIndex)
	cliOutput := filepath.Join(t.TempDir(), recoveryProbeExportFile)
	filtered := injectedProbeExport{name: "corrupted chat", provider: injectedProbeClaude, session: recoveryProbeCorruptSession, only: injectedProbeChatOnly, maxTokens: "", output: cliOutput}
	stdout, stderr, err := runInjectedProbeCLI(t, binaryPath, environment, filtered)
	if err == nil {
		t.Errorf("cli chat export with corrupted provenance succeeded:\n%s", stdout)
	}
	assertLegacyProbeError(t, "cli", stderr, conversationID, summaryUUID)
	assertLegacyProbeNoFile(t, "cli", cliOutput)

	mcpOutput := filepath.Join(t.TempDir(), recoveryProbeExportFile)
	filtered.output = mcpOutput
	response, err := callInjectedProbeMCP(binaryPath, sandboxDaemon.environment, filtered)
	if err != nil {
		t.Fatalf("clyde_export_transcript call failed: %v", err)
	}
	if response.Result == nil || !response.Result.IsError {
		t.Errorf("mcp chat export with corrupted provenance did not return a tool error: %+v %+v", response.Error, response.Result)
	} else {
		assertLegacyProbeError(t, "mcp", response.Result.text(), conversationID, summaryUUID)
	}
	assertLegacyProbeNoFile(t, "mcp", mcpOutput)
	if !strings.Contains(fixture.wrapper, recoveryProbeSystemHook) {
		t.Error("corrupted fixture does not contain the original system hook in the stored wrapper")
	}
}

func assertRecoveryProbeChat(t *testing.T, surface string, body string) {
	t.Helper()
	for _, kept := range []string{recoveryProbeProseTwo, recoveryProbeAssistantChat, recoveryProbeAnswerOne, recoveryProbeAnswerTwo, recoveryProbeTrailer, injectedProbeLaterAnswer, injectedProbeFinalAnswer} {
		if !strings.Contains(body, kept) {
			t.Errorf("%s chat export does not contain %q:\n%s", surface, kept, body)
		}
	}
	storedQuotation := strings.ReplaceAll(recoveryProbeQuotation, "</summary>", `<\/summary>`)
	if !strings.Contains(body, strings.Trim(injectedProbeJSON(t, storedQuotation), `"`)) {
		t.Errorf("%s chat export does not contain the retained quotation byte for byte:\n%s", surface, body)
	}
	if !strings.Contains(body, injectedProbeQuotation) {
		t.Errorf("%s chat export does not contain the later user quotation byte for byte:\n%s", surface, body)
	}
	for _, omitted := range recoveryProbeNonChat {
		if strings.Contains(body, omitted) {
			t.Errorf("%s chat export contains non-chat text %q:\n%s", surface, omitted, body)
		}
	}
}

// recoveryProbeTranscript builds two compactions; the first request retains
// tool, thinking, system, and chat segments and uses a blob with string
// content. Two references do not have blobs: an assistant message.id group of
// four records and two consecutive tool result records. The second summary
// repeats the first inside a tool result with a nested tool_reference block.
func recoveryProbeTranscript(t *testing.T, session string, corrupt bool) recoveryProbeFixture {
	t.Helper()
	nestedResult := `[` + recoveryTextPart(t, recoveryProbeNestedOutput) + `,{"tool_name":"Bash","type":"tool_reference"}]`
	firstMessages := []recoveryMessage{
		recoveryStringMessage(t, "user", recoveryProbeFirstQuestion),
		recoveryPartsMessage("assistant", recoveryToolUsePart(t, "toolu_recovery_1", recoveryProbeToolCallOne)),
		recoveryPartsMessage("user", recoveryToolResultPart("toolu_recovery_1", nestedResult)),
		recoveryPartsMessage("assistant", recoveryThinkingPart(t, recoveryProbeThinking), recoveryTextPart(t, recoveryProbeAssistantChat), recoveryToolUsePart(t, "toolu_recovery_2", recoveryProbeToolCallTwo), recoveryToolUsePart(t, "toolu_recovery_3", recoveryProbeToolCallThree)),
		recoveryPartsMessage("user", recoveryToolResultPart("toolu_recovery_2", recoveryJSONString(t, recoveryProbeToolOutputTwo)), recoveryToolResultPart("toolu_recovery_3", recoveryJSONString(t, recoveryProbeToolOutput3))),
		recoveryPartsMessage("system", recoveryTextPart(t, recoveryProbeSystemHook)),
		recoveryStringMessage(t, "user", recoveryProbeQuotation),
		recoveryPartsMessage("assistant", recoveryTextPart(t, recoveryProbeAnswerOne)),
		recoveryPartsMessage("user", recoveryTextPart(t, recoveryProbeLastBefore)),
	}
	if corrupt {
		firstMessages[5].stored = strings.Replace(firstMessages[5].stored, recoveryProbeSystemHook, recoveryProbeCorruptedHook, 1)
	}
	toolResultOne := recoveryProbeNestedOutput + "\n[tool_reference]"
	firstBody := "### Assistant\n\n" + `{"command":"` + recoveryProbeToolCallOne + `"}` +
		"\n\n### User\n\n" + toolResultOne +
		"\n\n### Assistant\n\n" + recoveryProbeThinking + "\n\n" + recoveryProbeAssistantChat + "\n\n" + `{"command":"` + recoveryProbeToolCallTwo + `"}` + "\n\n" + `{"command":"` + recoveryProbeToolCallThree + `"}` +
		"\n\n### User\n\n" + recoveryProbeToolOutputTwo + "\n\n" + recoveryProbeToolOutput3 +
		"\n\n### System\n\n" + recoveryProbeSystemHook +
		"\n\n### User\n\n" + recoveryProbeQuotation +
		"\n\n### Assistant\n\n" + recoveryProbeAnswerOne
	firstSummary := recoveryProbeSummary(t, recoveryProbeProseOne, firstBody)
	cut := strings.Index(firstSummary, toolResultOne) + recoveryProbeCutBytes
	secondMessages := []recoveryMessage{
		recoveryPartsMessage("user", recoveryTextPart(t, recoveryProbeReminder), recoveryTextPart(t, firstSummary)),
		recoveryPartsMessage("assistant", recoveryThinkingPart(t, recoveryProbeThinkingTwo), recoveryToolUsePart(t, "toolu_recovery_4", recoveryProbeToolCallFour)),
		recoveryPartsMessage("user", recoveryToolResultPart("toolu_recovery_4", recoveryJSONString(t, recoveryProbeToolOutput4))),
		recoveryPartsMessage("assistant", recoveryTextPart(t, recoveryProbeAnswerTwo)),
		recoveryPartsMessage("user", recoveryTextPart(t, recoveryProbeLastSecond)),
	}
	secondBody := "### User\n\n" + firstSummary[cut:] +
		"\n\n### Assistant\n\n" + recoveryProbeThinkingTwo + "\n\n" + `{"command":"` + recoveryProbeToolCallFour + `"}` +
		"\n\n### User\n\n" + recoveryProbeToolOutput4 +
		"\n\n### Assistant\n\n" + recoveryProbeAnswerTwo
	secondSummary := recoveryProbeSummary(t, recoveryProbeProseTwo, secondBody)
	builder := recoveryProbeLines{session: session, lines: nil}
	builder.firstConversation(t, firstMessages)
	builder.requests(firstMessages, []int{0, 1, 2, 5, 6, 7, 8}, "request-first", 0)
	builder.postCompaction(t, "request-first-post", recoveryProbePostSummary+" one")
	builder.summary(t, 12, 11, firstSummary)
	builder.secondConversation(secondMessages)
	builder.requests(secondMessages[:2], []int{0, 1}, "request-second-a", 0)
	builder.requests(secondMessages, []int{2, 3, 4}, "request-second-b", 2)
	builder.postCompaction(t, "request-second-post", recoveryProbePostSummary+" two")
	builder.summary(t, 19, 18, secondSummary)
	builder.after(t)
	wrapperStart := strings.Index(secondSummary, reorienttag.PreCompactionTranscriptOpen+"\n")
	wrapperEnd := strings.LastIndex(secondSummary, reorienttag.PreCompactionTranscriptClose) + len(reorienttag.PreCompactionTranscriptClose)
	return recoveryProbeFixture{lines: builder.lines, wrapper: secondSummary[wrapperStart:wrapperEnd]}
}

// recoveryProbeSummary inserts the unframed body into a model summary with the
// Anthropic summary injector, removes summary tags, and reduces line feed runs
// to two, as Claude Code does when storing compact summaries.
func recoveryProbeSummary(t *testing.T, prose string, body string) string {
	t.Helper()
	delta := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + recoveryJSONString(t, "<summary>\n"+prose+"\n</summary>") + `}}`
	response := "event: content_block_delta\ndata: " + delta + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	injected, err := anthropic.InjectIntoSummary([]byte(response), reorienttag.WrapInjection(body, ""))
	if err != nil {
		t.Fatalf("inject the legacy transcript into the summary: %v", err)
	}
	text := anthropic.ResponseText(injected)
	inner := text[strings.Index(text, "<summary>")+len("<summary>") : strings.LastIndex(text, "</summary>")]
	stored := injectedProbeSummaryPreface + strings.TrimSpace(inner) + "\n\n" + recoveryProbeTrailer
	for strings.Contains(stored, "\n\n\n") {
		stored = strings.ReplaceAll(stored, "\n\n\n", "\n\n")
	}
	return stored
}

// recoveryMessage stores one request message's content blocks, the JSON stored
// in its blob, and its reference hash.
type recoveryMessage struct {
	parts  []string
	stored string
	hash   string
}

func recoveryPartsMessage(role string, parts ...string) recoveryMessage {
	canonical := `{"content":[` + strings.Join(parts, ",") + `],"role":"` + role + `"}`
	return recoveryMessage{parts: parts, stored: canonical, hash: recoveryDigest(canonical)}
}

func recoveryStringMessage(t *testing.T, role string, text string) recoveryMessage {
	t.Helper()
	message := recoveryPartsMessage(role, recoveryTextPart(t, text))
	message.stored = `{"content":` + recoveryJSONString(t, text) + `,"role":"` + role + `"}`
	return message
}

func recoveryTextPart(t *testing.T, text string) string {
	t.Helper()
	return `{"text":` + recoveryJSONString(t, text) + `,"type":"text"}`
}

func recoveryThinkingPart(t *testing.T, text string) string {
	t.Helper()
	return `{"signature":"sig","thinking":` + recoveryJSONString(t, text) + `,"type":"thinking"}`
}

func recoveryToolUsePart(t *testing.T, id string, command string) string {
	t.Helper()
	return `{"id":"` + id + `","input":{"command":` + recoveryJSONString(t, command) + `},"name":"Bash","type":"tool_use"}`
}

func recoveryToolResultPart(id string, content string) string {
	return `{"content":` + content + `,"tool_use_id":"` + id + `","type":"tool_result"}`
}

// recoveryJSONString encodes text without HTML escaping, as JSON.stringify
// does for the fixture text.
func recoveryJSONString(t *testing.T, text string) string {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(text); err != nil {
		t.Fatalf("encode fixture text: %v", err)
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

func recoveryDigest(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

// recoveryProbeLines collects transcript lines for one session.
type recoveryProbeLines struct {
	session string
	lines   []string
}

func (builder *recoveryProbeLines) record(recordType string, index int, parent int, fields string) {
	parentUUID := ""
	if parent >= 0 {
		parentUUID = injectedProbeUUID(parent)
	}
	builder.lines = append(builder.lines, injectedProbeRecord(builder.session, recordType, index, parentUUID, fields))
}

func (builder *recoveryProbeLines) message(index int, role string, messageID string, parts ...string) {
	identity := ""
	if messageID != "" {
		identity = `,"id":"` + messageID + `"`
	}
	builder.record(role, index, index-1, `"message":{"content":[`+strings.Join(parts, ",")+`]`+identity+`,"role":"`+role+`"}`)
}

// firstConversation writes typed records before the first compaction. Request
// position 3 spans four assistant records with one message.id, and request
// position 4 spans two tool result records.
func (builder *recoveryProbeLines) firstConversation(t *testing.T, messages []recoveryMessage) {
	t.Helper()
	builder.record("user", 0, -1, `"message":{"content":`+recoveryJSONString(t, recoveryProbeFirstQuestion)+`,"role":"user"}`)
	builder.message(1, "assistant", "msg_recovery_1", messages[1].parts...)
	builder.message(2, "user", "", messages[2].parts...)
	for offset, part := range messages[3].parts {
		builder.message(3+offset, "assistant", "msg_recovery_3", part)
	}
	builder.message(7, "user", "", messages[4].parts[0])
	builder.message(8, "user", "", messages[4].parts[1])
	builder.record("user", 9, 8, `"message":{"content":`+recoveryJSONString(t, recoveryProbeQuotation)+`,"role":"user"}`)
	builder.message(10, "assistant", "msg_recovery_7", messages[7].parts...)
	builder.message(11, "user", "", messages[8].parts...)
}

// secondConversation writes typed records between the two compactions.
func (builder *recoveryProbeLines) secondConversation(messages []recoveryMessage) {
	builder.message(14, "assistant", "msg_recovery_14", messages[1].parts[0])
	builder.message(15, "assistant", "msg_recovery_14", messages[1].parts[1])
	builder.message(16, "user", "", messages[2].parts...)
	builder.message(17, "assistant", "msg_recovery_17", messages[3].parts...)
	builder.message(18, "user", "", messages[4].parts...)
}

// requests writes blobs for the listed positions and one api-request record
// that retains keep references and appends the remaining messages.
func (builder *recoveryProbeLines) requests(messages []recoveryMessage, blobPositions []int, id string, keep int) {
	for _, position := range blobPositions {
		builder.lines = append(builder.lines, `{"type":"api-request-blob","sessionId":"`+builder.session+`","hash":"`+messages[position].hash+`","message":`+messages[position].stored+`}`)
	}
	tail := make([]string, 0, len(messages)-keep)
	for _, message := range messages[keep:] {
		tail = append(tail, `"`+message.hash+`"`)
	}
	builder.lines = append(builder.lines, `{"type":"api-request","sessionId":"`+builder.session+`","id":"`+id+`","keep":`+fmt.Sprint(keep)+`,"tail":[`+strings.Join(tail, ",")+`]}`)
}

// postCompaction writes the request with keep 0 that Claude Code records after
// a compaction.
func (builder *recoveryProbeLines) postCompaction(t *testing.T, id string, text string) {
	t.Helper()
	message := recoveryPartsMessage("user", recoveryTextPart(t, text))
	builder.requests([]recoveryMessage{message}, []int{0}, id, 0)
}

func (builder *recoveryProbeLines) summary(t *testing.T, boundary int, parent int, summary string) {
	t.Helper()
	builder.record("system", boundary, -1, `"subtype":"compact_boundary","content":"Conversation compacted","logicalParentUuid":"`+injectedProbeUUID(parent)+`","compactMetadata":{"trigger":"manual","preTokens":1200}`)
	builder.record("user", boundary+1, boundary, `"isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":`+injectedProbeJSON(t, summary)+`}`)
}

func (builder *recoveryProbeLines) after(t *testing.T) {
	t.Helper()
	builder.record("assistant", 21, recoveryProbeSummaryIndex, `"message":{"role":"assistant","content":[{"type":"text","text":"`+injectedProbeLaterAnswer+`"}]}`)
	builder.record("user", 22, 21, `"message":{"role":"user","content":`+injectedProbeJSON(t, injectedProbeQuotation)+`}`)
	builder.record("assistant", 23, 22, `"message":{"role":"assistant","content":[{"type":"text","text":"`+injectedProbeFinalAnswer+`"}]}`)
}
