package localbackendcli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/conversation"
	codexcompaction "goodkind.io/clyde/internal/providers/codex/compaction"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorientinject"
	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/tokencount"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/gklog/correlation"
)

const (
	codexProbeProvider      = "codex"
	codexProbeFramedRollout = "019e0000-3a00-7010-bd9f-a6ee71559101"
	codexProbeLegacyRollout = "019e0000-3a00-7010-bd9f-a6ee71559102"
	codexProbeTurnMetadata  = `{"session_id":"codexprobe-session","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses"}}`
	codexProbeResponse      = `{"id":"resp-codexprobe","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + codexProbeSummary + `"}]}]}`
	codexProbeCallID        = "call-codexprobe"
	codexProbeLegacyFile    = "codex-legacy-export.txt"
	codexProbeSessionID     = "codexprobe-session"
	codexProbeCipher        = "codexprobe-cipher"
	codexProbeFinalMetadata = `{"session_id":"` + codexProbeSessionID + `","thread_source":"user","sandbox":"none","request_kind":"turn","compaction":{"phase":"final_answer"}}`
	codexProbeFinalRequest  = `{"model":"gpt-codexprobe","input":[{"type":"compaction","encrypted_content":"` + codexProbeCipher + `"},{"type":"message","role":"user","content":[{"type":"input_text","text":"codexprobe continue"}]}]}`
	codexProbeFinalResponse = `{"id":"resp-codexprobe-final","status":"completed","output":[{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"` + codexProbeSummary + `"}]}]}`

	codexProbeSummary       = "codexprobe summary prose before the transcript"
	codexProbeOldest        = "codexprobe oldest message the model summarizes"
	codexProbeUserChat      = "codexprobe retained user chat é 𝄞"
	codexProbeAssistantChat = "codexprobe retained assistant chat"
	codexProbeSystem        = "codexprobe developer system sentinel"
	codexProbeReasoning     = "codexprobe reasoning sentinel"
	codexProbeToolCall      = "codexprobe tool call sentinel"
	codexProbeToolOutput    = "codexprobe tool output sentinel ü 😀"
	codexProbePrompt        = "codexprobe summarize the conversation so far"
	codexProbeEarlierUser   = "codexprobe earlier user question"
	codexProbeLaterUser     = "codexprobe question after compaction"
	codexProbeLaterAnswer   = "codexprobe answer after compaction"
	codexProbeLegacySystem  = "codexprobe legacy system sentinel"

	codexProbeQuotationRollout  = "019e0000-3a00-7010-bd9f-a6ee71559103"
	codexProbeLiveRollout       = "019e0000-3a00-7010-bd9f-a6ee71559104"
	codexProbeLegacyLiveRollout = "019e0000-3a00-7010-bd9f-a6ee71559105"
	codexProbeToolOutputsOnly   = "tool_outputs"
	codexProbeFullLastN         = 100
	codexProbeQuotedFrame       = "ORDINARY_QUOTATION_TOOL_FRAME_MUST_REMAIN"
	codexProbeFramedQuotation   = "### User\n\nThis is a literal documentation example.\n\n" +
		"<pre-compaction-transcript>\n<clyde-transcript-format version=\"2\"/>\n" +
		"<segment role=\"user\" kind=\"tool_output\" bytes=\"41\">\n" + codexProbeQuotedFrame + "\n</segment>\n" +
		"</pre-compaction-transcript>\n"
	codexProbeLegacyQuotation = "This is a literal documentation example.\n\n" +
		"<pre-compaction-transcript>\n### User\n\n" + codexProbeQuotedFrame + "\n</pre-compaction-transcript>\n"
)

// codexProbeLegacyTranscript stores an unframed transcript with a user
// heading, a system section, and assistant chat followed by JSON tool input.
// It omits the format line. The current splitter does not write this format.
const codexProbeLegacyTranscript = "### User\n\ncodexprobe legacy retained user chat\n\n" +
	"### System\n\n" + codexProbeLegacySystem + "\n\n" +
	"### Assistant\n\ncodexprobe legacy retained assistant chat\n\n{\"command\":\"echo codexprobe legacy tool call\"}"

// TestExportAppliesContentKindsToInjectedCodexTranscript runs the production
// Codex compaction splitter and response transformer on a native request. It
// stores the transformed assistant item in rollout replacement_history beside
// an ordinary assistant item that quotes the transcript format and exports
// through the built CLI and clyde_export_transcript. It checks chat only, a
// broad selection, and all content kinds, including the exact quotation, the
// original injection, and the unchanged rollout.
func TestExportAppliesContentKindsToInjectedCodexTranscript(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	item := codexProbeInjectedItem(t, codexProbeResponse)
	rolloutPath := codexProbeRolloutPath(home, codexProbeFramedRollout)
	installLocalProofTranscript(t, rolloutPath, codexProbeRollout(t, codexProbeFramedRollout, item))
	before := injectedProbeDigest(t, rolloutPath)
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, codexProbeFramedRollout)

	injectedText := codexProbeItemText(t, item)
	exports := []injectedProbeExport{
		{name: "chat", provider: codexProbeProvider, session: codexProbeFramedRollout, only: injectedProbeChatOnly, maxTokens: "", output: "", lastN: injectedProbeLastN},
		{name: "broad", provider: codexProbeProvider, session: codexProbeFramedRollout, only: injectedProbeBroad, maxTokens: "", output: "", lastN: injectedProbeLastN},
		{name: "all", provider: codexProbeProvider, session: codexProbeFramedRollout, only: injectedProbeAll, maxTokens: "", output: "", lastN: injectedProbeLastN},
	}
	for _, export := range exports {
		bodies := map[string]string{
			"cli": injectedProbeCLI(t, binaryPath, environment, export),
			"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, export),
		}
		for surface, body := range bodies {
			label := surface + " " + export.name
			switch export.name {
			case "chat":
				assertCodexProbeContains(t, label, body, codexProbeSummary, codexProbeUserChat, codexProbeAssistantChat, codexProbeEarlierUser, codexProbeLaterAnswer, injectedProbeQuotation)
				assertCodexProbeOmits(t, label, body, codexProbeSystem, codexProbeReasoning, codexProbeToolCall, codexProbeToolOutput)
			case "broad":
				assertCodexProbeContains(t, label, body, codexProbeSummary, codexProbeUserChat, codexProbeAssistantChat, codexProbeSystem, codexProbeReasoning, codexProbeToolCall, codexProbeToolOutput, injectedProbeQuotation)
			default:
				assertCodexProbeContains(t, label, body, injectedText, injectedProbeQuotation)
			}
		}
	}
	if after := injectedProbeDigest(t, rolloutPath); after != before {
		t.Errorf("The export changed the rollout file (before digest: %s, after digest: %s).", before, after)
	}
}

// TestExportRejectsFilteredLegacyInjectedCodexTranscript uses the v2 final
// answer producer to mark an unframed injection. It verifies that CLI and MCP
// chat exports fail without writing a file. It also verifies that an export
// with all content kinds returns the original text and leaves the rollout
// unchanged.
func TestExportRejectsFilteredLegacyInjectedCodexTranscript(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	item := codexProbeRecoveredItem(t, reorienttag.WrapInjection(codexProbeLegacyTranscript, ""))
	rolloutPath := codexProbeRolloutPath(home, codexProbeLegacyRollout)
	installLocalProofTranscript(t, rolloutPath, codexProbeRollout(t, codexProbeLegacyRollout, item))
	before := injectedProbeDigest(t, rolloutPath)
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, codexProbeLegacyRollout)

	assertCodexProbeFilteredExportFails(t, binaryPath, environment, sandboxDaemon.environment, codexProbeLegacyRollout)

	complete := injectedProbeExport{name: "legacy all", provider: codexProbeProvider, session: codexProbeLegacyRollout, only: injectedProbeAll, maxTokens: "", output: "", lastN: injectedProbeLastN}
	bodies := map[string]string{
		"cli": injectedProbeCLI(t, binaryPath, environment, complete),
		"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, complete),
	}
	for surface, body := range bodies {
		assertCodexProbeContains(t, surface+" legacy all", body, codexProbeItemText(t, item), injectedProbeQuotation)
	}
	if after := injectedProbeDigest(t, rolloutPath); after != before {
		t.Errorf("The export changed the rollout file (before digest: %s, after digest: %s).", before, after)
	}
}

// TestExportPreservesOrdinaryCodexQuotations checks that CLI and MCP exports
// preserve ordinary framed and unframed transcript quotations when the content
// filter selects chat or all content kinds. It also checks that chat exports
// exclude marked system, reasoning, and tool output text and that exports
// leave the rollout unchanged.
func TestExportPreservesOrdinaryCodexQuotations(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	marked := codexProbeInjectedItem(t, codexProbeResponseWithSummary(t, codexProbeFramedQuotation+codexProbeLegacyQuotation))
	replacement := []string{
		`{"type":"message","id":"ordinary-framed","role":"assistant","content":[{"type":"output_text","text":` + injectedProbeJSON(t, codexProbeFramedQuotation) + `}]}`,
		`{"type":"message","id":"ordinary-legacy","role":"assistant","content":[{"type":"output_text","text":` + injectedProbeJSON(t, codexProbeLegacyQuotation) + `}]}`,
		string(marked),
	}
	rolloutPath := codexProbeRolloutPath(home, codexProbeQuotationRollout)
	installLocalProofTranscript(t, rolloutPath, codexProbeRolloutWith(codexProbeQuotationRollout, replacement))
	before := injectedProbeDigest(t, rolloutPath)
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, codexProbeQuotationRollout)

	for _, only := range []string{injectedProbeChatOnly, injectedProbeAll} {
		export := injectedProbeExport{name: only, provider: codexProbeProvider, session: codexProbeQuotationRollout, only: only, maxTokens: "", output: "", lastN: injectedProbeLastN}
		bodies := map[string]string{
			"cli": injectedProbeCLI(t, binaryPath, environment, export),
			"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, export),
		}
		for surface, body := range bodies {
			label := surface + " " + only
			assertCodexProbeContains(t, label, body, codexProbeFramedQuotation, codexProbeLegacyQuotation)
			if only == injectedProbeChatOnly {
				assertCodexProbeOmits(t, label, body, codexProbeSystem, codexProbeReasoning, codexProbeToolOutput)
				continue
			}
			assertCodexProbeContains(t, label, body, codexProbeItemText(t, marked))
		}
	}
	if after := injectedProbeDigest(t, rolloutPath); after != before {
		t.Errorf("The export changed the rollout file (before digest: %s, after digest: %s).", before, after)
	}
}

func assertCodexProbeFilteredExportFails(t *testing.T, binaryPath string, cliEnvironment []string, mcpEnvironment []string, session string) {
	t.Helper()
	conversationID := codexProbeProvider + ":" + session
	cliOutput := filepath.Join(t.TempDir(), codexProbeLegacyFile)
	filtered := injectedProbeExport{name: "unresolved chat", provider: codexProbeProvider, session: session, only: injectedProbeChatOnly, maxTokens: "", output: cliOutput, lastN: codexProbeFullLastN}
	stdout, stderr, err := runInjectedProbeCLI(t, binaryPath, cliEnvironment, filtered)
	if err == nil {
		t.Errorf("The CLI chat export accepted an unframed Codex injection:\n%s", stdout)
	}
	assertCodexProbeError(t, "cli", stderr, conversationID)
	assertLegacyProbeNoFile(t, "cli", cliOutput)

	mcpOutput := filepath.Join(t.TempDir(), codexProbeLegacyFile)
	filtered.output = mcpOutput
	response, err := callInjectedProbeMCP(binaryPath, mcpEnvironment, filtered)
	if err != nil {
		t.Fatalf("The test could not export the transcript with clyde_export_transcript: %v", err)
	}
	if response.Result == nil || !response.Result.IsError {
		t.Errorf("The MCP chat export did not return a tool error for an unframed Codex injection: %+v %+v", response.Error, response.Result)
	} else {
		assertCodexProbeError(t, "mcp", response.Result.text(), conversationID)
	}
	assertLegacyProbeNoFile(t, "mcp", mcpOutput)
}

// TestExportFiltersMarkedCodexLiveMessage checks that CLI and MCP exports
// select chat or tool outputs from a marked live transcript and preserve the
// ordinary quotation when the content filter selects chat or all content
// kinds. It also checks that exports leave the rollout unchanged.
func TestExportFiltersMarkedCodexLiveMessage(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	injection := codexProbeItemText(t, codexProbeInjectedItem(t, codexProbeResponse))
	item := codexProbeInjectedItem(t, codexProbeResponseWithSummary(t, injection))
	rolloutPath := codexProbeRolloutPath(home, codexProbeLiveRollout)
	installLocalProofTranscript(t, rolloutPath, codexProbeLiveRolloutBody(t, codexProbeLiveRollout, item))
	before := injectedProbeDigest(t, rolloutPath)
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, codexProbeLiveRollout)

	quotation := transcript.NormalizeConversationOnlyText(injection)
	for _, only := range []string{injectedProbeChatOnly, codexProbeToolOutputsOnly, injectedProbeAll} {
		export := injectedProbeExport{name: only, provider: codexProbeProvider, session: codexProbeLiveRollout, only: only, maxTokens: "", output: "", lastN: codexProbeFullLastN}
		bodies := map[string]string{
			"cli": injectedProbeCLI(t, binaryPath, environment, export),
			"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, export),
		}
		for surface, body := range bodies {
			label := "live " + surface + " " + only
			switch only {
			case injectedProbeChatOnly:
				assertCodexProbeContains(t, label, body, quotation, codexProbeLaterAnswer)
				assertCodexProbeRecoveredChat(t, label, body, quotation)
			case codexProbeToolOutputsOnly:
				assertCodexProbeContains(t, label, body, codexProbeToolOutput)
				assertCodexProbeOmits(t, label, body, codexProbeUserChat, codexProbeAssistantChat, codexProbeLaterAnswer, codexProbeLaterUser)
			default:
				assertCodexProbeContains(t, label, body, quotation, codexProbeToolOutput)
			}
		}
	}
	if after := injectedProbeDigest(t, rolloutPath); after != before {
		t.Errorf("The export changed the rollout file (before digest: %s, after digest: %s).", before, after)
	}
}

// assertCodexProbeRecoveredChat removes every literal copy of the quotation
// from the export body and checks that the remaining text includes recovered
// user chat and excludes marked tool output.
func assertCodexProbeRecoveredChat(t *testing.T, label string, body string, quotation string) {
	t.Helper()
	remainder := strings.ReplaceAll(body, quotation, "")
	if strings.Contains(remainder, codexProbeToolOutput) {
		t.Errorf("The %s export contains marked tool text outside the quotation:\n%s", label, body)
	}
	if !strings.Contains(remainder, codexProbeUserChat) {
		t.Errorf("The %s export omitted recovered chat from the marked part:\n%s", label, body)
	}
}

// TestExportRejectsUnframedMarkedCodexLiveMessage checks that CLI and MCP chat
// exports reject an unframed marked live transcript without creating output
// files. It also checks that exports with all content kinds include the legacy
// system text and leave the rollout unchanged.
func TestExportRejectsUnframedMarkedCodexLiveMessage(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	item := codexProbeRecoveredItem(t, reorienttag.WrapInjection(codexProbeLegacyTranscript, ""))
	rolloutPath := codexProbeRolloutPath(home, codexProbeLegacyLiveRollout)
	installLocalProofTranscript(t, rolloutPath, codexProbeLiveRolloutBody(t, codexProbeLegacyLiveRollout, item))
	before := injectedProbeDigest(t, rolloutPath)
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, codexProbeLegacyLiveRollout)

	assertCodexProbeFilteredExportFails(t, binaryPath, environment, sandboxDaemon.environment, codexProbeLegacyLiveRollout)

	complete := injectedProbeExport{name: "live legacy all", provider: codexProbeProvider, session: codexProbeLegacyLiveRollout, only: injectedProbeAll, maxTokens: "", output: "", lastN: codexProbeFullLastN}
	bodies := map[string]string{
		"cli": injectedProbeCLI(t, binaryPath, environment, complete),
		"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, complete),
	}
	for surface, body := range bodies {
		assertCodexProbeContains(t, surface+" live legacy all", body, codexProbeLegacySystem)
	}
	if after := injectedProbeDigest(t, rolloutPath); after != before {
		t.Errorf("The export changed the rollout file (before digest: %s, after digest: %s).", before, after)
	}
}

// assertCodexProbeContains reports each text that body does not contain
// literally or as JSON string contents.
func assertCodexProbeContains(t *testing.T, label string, body string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if !codexProbeHas(t, body, text) {
			t.Errorf("The export for %s did not include the expected text %q:\n%s", label, text, body)
		}
	}
}

func assertCodexProbeOmits(t *testing.T, label string, body string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if codexProbeHas(t, body, text) {
			t.Errorf("The export for %s included the excluded text %q:\n%s", label, text, body)
		}
	}
}

// codexProbeHas reports whether body contains text literally or as JSON string
// contents without surrounding quotes. Export writes compacted context items
// as JSON.
func codexProbeHas(t *testing.T, body string, text string) bool {
	t.Helper()
	encoded := strings.Trim(injectedProbeJSON(t, text), `"`)
	return strings.Contains(body, text) || strings.Contains(body, encoded)
}

func assertCodexProbeError(t *testing.T, surface string, text string, conversationID string) {
	t.Helper()
	for _, want := range []string{conversationID, injectedProbeAll} {
		if !strings.Contains(text, want) {
			t.Errorf("The export error for %s did not include the expected value %q:\n%s", surface, want, text)
		}
	}
	if strings.Contains(text, codexProbeLegacySystem) {
		t.Errorf("The export error for %s included transcript text:\n%s", surface, text)
	}
}

// codexProbeInjectedItem runs the production splitter and the v1 response
// transformer on a native request and returns the transformed assistant item.
func codexProbeInjectedItem(t *testing.T, responseBody string) json.RawMessage {
	t.Helper()
	splitter := codexProbeSplitter()
	items := []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codexProbeOldest + `"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codexProbeUserChat + `"}]}`,
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + codexProbeSystem + `"}]}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"` + codexProbeReasoning + `"}]}`,
		`{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo ` + codexProbeToolCall + `\"}","call_id":"` + codexProbeCallID + `"}`,
		`{"type":"function_call_output","call_id":"` + codexProbeCallID + `","output":"` + codexProbeToolOutput + `"}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + codexProbeAssistantChat + `"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codexProbePrompt + `"}]}`,
	}
	request := adaptercodex.RawResponsesRequest{
		Body:        []byte(`{"model":"gpt-codexprobe","input":[` + strings.Join(items, ",") + `]}`),
		Header:      http.Header{adaptercodex.CodexTurnMetadataHeader: {codexProbeTurnMetadata}},
		RequestID:   "",
		Correlation: correlation.Context{},
		Stream:      false,
	}
	_, transformer := adaptercodex.PrepareRawResponsesCompaction(request, splitter)
	if transformer == nil {
		t.Fatal("PrepareRawResponsesCompaction did not return a response transformer for the probe request.")
	}
	return codexProbeTransformedItem(t, transformer, responseBody)
}

func codexProbeSplitter() adaptercodex.CompactionSplitter {
	return codexcompaction.NewSplitter(reorientinject.Settings{
		DefaultBudget:    injectedProbeBudget,
		DefaultContent:   conversation.NewContentKindSet(),
		MaxRetainedBytes: adaptercodex.MaxCompactionInjectionBytes,
		InstructionsFile: "",
		Counter:          tokencount.LocalCounter(tokencount.FamilyClaude, "", tokencount.Settings{}),
	})
}

func codexProbeResponseWithSummary(t *testing.T, summary string) string {
	t.Helper()
	return `{"id":"resp-codexprobe","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + injectedProbeJSON(t, summary) + `}]}]}`
}

// codexProbeRecoveredItem arms the v2 registry with injection, runs the v2
// recovery request path, and returns the transformed final answer item.
func codexProbeRecoveredItem(t *testing.T, injection string) json.RawMessage {
	t.Helper()
	registry := adaptercodex.NewRawResponsesCompactionV2Registry(nil)
	if !registry.Arm(codexProbeSessionID, codexProbeCipher, injection) {
		t.Fatal("Registry Arm rejected the test injection.")
	}
	request := adaptercodex.RawResponsesRequest{
		Body:        []byte(codexProbeFinalRequest),
		Header:      http.Header{adaptercodex.CodexTurnMetadataHeader: {codexProbeFinalMetadata}},
		RequestID:   "",
		Correlation: correlation.Context{},
		Stream:      false,
	}
	injected, recovery, ok := adaptercodex.InjectRawResponsesCompactionV2Recovery(request, registry)
	if !ok {
		t.Fatal("InjectRawResponsesCompactionV2Recovery rejected the test request.")
	}
	transformer := adaptercodex.NewRawResponsesCompactionV2FinalAnswerTransformer(injected, recovery)
	return codexProbeTransformedItem(t, transformer, codexProbeFinalResponse)
}

// codexProbeTransformedItem runs transformer on responseBody and returns the
// single output item. It fails the test when that item does not contain a
// marked transcript part.
func codexProbeTransformedItem(t *testing.T, transformer *adaptercodex.RawResponsesCompactionTransformer, responseBody string) json.RawMessage {
	t.Helper()
	response := transformer.TransformResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(responseBody))),
	})
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("The test could not read the transformed response body: %v", err)
	}
	var decoded struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.Output) != 1 {
		t.Fatalf("The response failed decoding or did not contain exactly one output item: %v\nThe response contains: %s", err, body)
	}
	if !strings.Contains(codexProbeItemText(t, decoded.Output[0]), reorienttag.PreCompactionTranscriptOpen) {
		t.Fatalf("The transformed assistant item does not contain the transcript open tag:\n%s", body)
	}
	return decoded.Output[0]
}

func codexProbeItemText(t *testing.T, item json.RawMessage) string {
	t.Helper()
	var message struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	indexes := codexstore.InjectedTranscriptIndexes(item)
	if err := json.Unmarshal(item, &message); err != nil || len(indexes) != 1 || indexes[0] >= len(message.Content) {
		t.Fatalf("The probe item failed decoding or did not contain exactly one marked part: %v\nThe probe item contains: %s", err, item)
	}
	return message.Content[indexes[0]].Text
}

func codexProbeRolloutPath(home string, id string) string {
	return filepath.Join(home, ".codex", "sessions", "2026", "07", "01", "rollout-2026-07-01T11-00-00-"+id+".jsonl")
}

// codexProbeRollout returns a Codex rollout with a session metadata record, a
// user message, a compacted record, and two later messages. The
// replacement_history stores an earlier user message, the injected assistant
// item, and an ordinary assistant item with the quotation.
func codexProbeRollout(t *testing.T, id string, injected json.RawMessage) string {
	t.Helper()
	replacement := []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codexProbeEarlierUser + `"}]}`,
		string(injected),
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + injectedProbeJSON(t, injectedProbeQuotation) + `}]}`,
	}
	return codexProbeRolloutWith(id, replacement)
}

// codexProbeLiveRolloutBody builds a rollout with an agent_message event that
// has the concatenated content text, the corresponding marked response_item,
// and later user and assistant messages.
func codexProbeLiveRolloutBody(t *testing.T, id string, item json.RawMessage) string {
	t.Helper()
	var message struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(item, &message); err != nil {
		t.Fatalf("The test could not decode the live probe item: %v", err)
	}
	var eventText strings.Builder
	for _, part := range message.Content {
		eventText.WriteString(part.Text)
	}
	lines := []string{
		strings.TrimSuffix(exportProbeSessionMeta(id), "\n"),
		`{"timestamp":"2026-07-01T11:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"codexprobe first question"}}`,
		`{"timestamp":"2026-07-01T11:00:02.000Z","type":"event_msg","payload":{"type":"agent_message","message":` + injectedProbeJSON(t, eventText.String()) + `,"phase":"final_answer"}}`,
		`{"timestamp":"2026-07-01T11:00:02.100Z","type":"response_item","payload":` + string(item) + `}`,
		`{"timestamp":"2026-07-01T11:00:03.000Z","type":"event_msg","payload":{"type":"user_message","message":"` + codexProbeLaterUser + `"}}`,
		`{"timestamp":"2026-07-01T11:00:04.000Z","type":"event_msg","payload":{"type":"agent_message","message":"` + codexProbeLaterAnswer + `"}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func codexProbeRolloutWith(id string, replacement []string) string {
	lines := []string{
		strings.TrimSuffix(exportProbeSessionMeta(id), "\n"),
		`{"timestamp":"2026-07-01T11:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"codexprobe first question"}}`,
		`{"timestamp":"2026-07-01T11:00:02.000Z","type":"compacted","payload":{"message":"","replacement_history":[` + strings.Join(replacement, ",") + `]}}`,
		`{"timestamp":"2026-07-01T11:00:03.000Z","type":"event_msg","payload":{"type":"user_message","message":"` + codexProbeLaterUser + `"}}`,
		`{"timestamp":"2026-07-01T11:00:04.000Z","type":"event_msg","payload":{"type":"agent_message","message":"` + codexProbeLaterAnswer + `"}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}
