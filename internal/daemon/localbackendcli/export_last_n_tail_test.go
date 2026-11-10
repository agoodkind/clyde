package localbackendcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	lastNProbeChatSession = "4e2b7d1c-6a3f-4c8e-9b5d-1f0a2c3e4d77"
	lastNProbeTailSession = "5f3c8e2d-7b4a-4d9f-8c6e-2a1b3d4f5e88"
	lastNProbeAllSession  = "6a4d9f3e-8c5b-4e0a-9d7f-3b2c4e5f6a99"
	lastNProbeWorkspace   = "/lastn-probe/workspace"
	lastNProbeCount       = 3
	lastNProbeAllCount    = 4
	lastNProbeMaxTokens   = "150"
	lastNProbeOnlyChat    = "chat"
	lastNProbeOnlyTail    = "chat,tools"
	lastNProbeSegment     = "0"
	lastNProbeMarkdown    = "markdown"
	lastNProbePlainText   = "plain_text"
	lastNProbeJSON        = "json"
	lastNProbeDense       = "dense"
	lastNProbeAll         = "all"
	lastNProbeSignature   = "lastnprobe-signature"

	lastNProbeSummaryPreface = "This session is being continued from a previous conversation that ran out of context.\n\nSummary:\n"
	lastNProbeSummaryProse   = "lastnprobe summary prose"
	lastNProbeBeforeQuestion = "lastnprobe question before compaction"
	lastNProbeBeforeAnswer   = "lastnprobe answer before compaction"
	lastNProbeChatOne        = "lastnprobe chat one"
	lastNProbeChatTwo        = "lastnprobe chat two"
	lastNProbeChatThree      = "lastnprobe chat three"
	lastNProbeChatFour       = "lastnprobe chat four"
	lastNProbeChatFive       = "lastnprobe chat five"
	lastNProbeToolCommand    = "echo lastnprobe tool call"
	lastNProbeToolOutput     = "lastnprobe tool output"
	lastNProbeToolName       = "Bash"
	lastNProbeToolCallCount  = 4

	tailProbeChatEarly     = "tailprobe chat early"
	tailProbeChatLate      = "tailprobe chat late"
	tailProbeToolEarly     = "tailprobe tool early"
	tailProbeToolLate      = "tailprobe tool late"
	tailProbeThinkingOne   = "tailprobe thinking one"
	tailProbeThinkingTwo   = "tailprobe thinking two"
	tailProbeThinkingThree = "tailprobe thinking three"
)

type lastNProbeArguments struct {
	ConversationID     string   `json:"conversation_id"`
	Only               []string `json:"only"`
	LastN              int      `json:"last_n"`
	IncludeCompactions string   `json:"include_compactions,omitempty"`
	FullHistory        bool     `json:"full_history,omitempty"`
	MaxTokens          string   `json:"max_tokens,omitempty"`
	Format             string   `json:"format"`
	Whitespace         string   `json:"whitespace,omitempty"`
	Stdout             bool     `json:"stdout"`
}

type lastNProbeCall struct {
	Name      string              `json:"name"`
	Arguments lastNProbeArguments `json:"arguments"`
}

type lastNProbeRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  lastNProbeCall `json:"params"`
}

type lastNProbeTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type lastNProbeDocument struct {
	Messages []lastNProbeTurn `json:"messages"`
}

type lastNProbeExport struct {
	name      string
	format    string
	maxTokens string
}

// TestExportLastNCountsOnlyMessagesTheContentKindsRender checks that CLI and MCP
// exports select the last three chat messages and include the starting summary.
func TestExportLastNCountsOnlyMessagesTheContentKindsRender(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	installLastNProbeTranscript(t, home, lastNProbeChatSession, lastNProbeChatRecords(t))
	environment := environmentWith(localProofProviderEnvironment(t, home)...)
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForLastNProbeMCP(t, binaryPath, sandboxDaemon.environment, lastNProbeChatSession)

	exports := []lastNProbeExport{
		{name: "json", format: lastNProbeJSON, maxTokens: ""},
		{name: "markdown capped", format: lastNProbeMarkdown, maxTokens: lastNProbeMaxTokens},
		{name: "plain text", format: lastNProbePlainText, maxTokens: ""},
	}
	for _, export := range exports {
		cliArguments := []string{"conversation", "export", "claude:" + lastNProbeChatSession, "--only", lastNProbeOnlyChat, "--last-n", strconv.Itoa(lastNProbeCount), "--format", export.format, "--stdout"}
		if export.maxTokens != "" {
			cliArguments = append(cliArguments, "--max-tokens", export.maxTokens)
		}
		mcpArguments := lastNProbeArguments{
			ConversationID:     "claude:" + lastNProbeChatSession,
			Only:               []string{lastNProbeOnlyChat},
			LastN:              lastNProbeCount,
			IncludeCompactions: "",
			FullHistory:        false,
			MaxTokens:          export.maxTokens,
			Format:             export.format,
			Whitespace:         "",
			Stdout:             true,
		}
		bodies := map[string]string{
			"cli": runLastNProbeCLI(t, binaryPath, environment, cliArguments),
			"mcp": lastNProbeMCPBody(t, binaryPath, sandboxDaemon.environment, mcpArguments),
		}
		for surface, body := range bodies {
			label := surface + " " + export.name
			assertLastNProbeChatTail(t, label, body)
			if export.maxTokens == "" {
				assertLastNProbeContains(t, label, body, lastNProbeSummaryProse)
			}
			if export.format == lastNProbeJSON {
				assertLastNProbeJSONTurns(t, label, body)
			}
		}
	}
}

// TestExportTailLastNCountsChatAndToolMessages checks that CLI tail and the
// equivalent MCP call select the same chat and tool messages and starting summary.
func TestExportTailLastNCountsChatAndToolMessages(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	installLastNProbeTranscript(t, home, lastNProbeTailSession, lastNProbeTailRecords(t))
	environment := environmentWith(localProofProviderEnvironment(t, home)...)
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForLastNProbeMCP(t, binaryPath, sandboxDaemon.environment, lastNProbeTailSession)

	cliBody := runLastNProbeCLI(t, binaryPath, environment, []string{"conversation", "export", "tail", "claude:" + lastNProbeTailSession, "--last-n", strconv.Itoa(lastNProbeCount)})
	mcpBody := lastNProbeMCPBody(t, binaryPath, sandboxDaemon.environment, lastNProbeArguments{
		ConversationID:     "claude:" + lastNProbeTailSession,
		Only:               strings.Split(lastNProbeOnlyTail, ","),
		LastN:              lastNProbeCount,
		IncludeCompactions: lastNProbeSegment,
		FullHistory:        false,
		MaxTokens:          "",
		Format:             lastNProbeMarkdown,
		Whitespace:         lastNProbeDense,
		Stdout:             true,
	})
	for surface, body := range map[string]string{"cli tail": cliBody, "mcp": mcpBody} {
		assertLastNProbeContains(t, surface, body, lastNProbeSummaryProse)
		assertLastNProbeOrder(t, surface, body, []string{tailProbeToolEarly, tailProbeChatLate, tailProbeToolLate})
		for _, omitted := range []string{tailProbeChatEarly, tailProbeThinkingOne, tailProbeThinkingTwo, tailProbeThinkingThree, lastNProbeBeforeQuestion, lastNProbeBeforeAnswer} {
			assertLastNProbeOmits(t, surface, body, omitted)
		}
	}
	if cliBody != mcpBody {
		t.Errorf("cli tail body differs from the equivalent mcp export:\ncli:\n%s\nmcp:\n%s", cliBody, mcpBody)
	}
}

// TestExportLastNWithAllSkipsCompactionMessages checks CLI and MCP exports with only=all
// and last_n=4. Each export includes the starting summary, the assistant answer before
// compaction, and three chat messages in order, and omits the earlier user question.
func TestExportLastNWithAllSkipsCompactionMessages(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	installLastNProbeTranscript(t, home, lastNProbeAllSession, lastNProbeAllRecords(t))
	environment := environmentWith(localProofProviderEnvironment(t, home)...)
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForLastNProbeMCP(t, binaryPath, sandboxDaemon.environment, lastNProbeAllSession)

	cliBody := runLastNProbeCLI(t, binaryPath, environment, []string{"conversation", "export", "claude:" + lastNProbeAllSession, "--only", lastNProbeAll, "--full-history", "--last-n", strconv.Itoa(lastNProbeAllCount), "--format", lastNProbeMarkdown, "--stdout"})
	mcpBody := lastNProbeMCPBody(t, binaryPath, sandboxDaemon.environment, lastNProbeArguments{
		ConversationID:     "claude:" + lastNProbeAllSession,
		Only:               []string{lastNProbeAll},
		LastN:              lastNProbeAllCount,
		IncludeCompactions: "",
		FullHistory:        true,
		MaxTokens:          "",
		Format:             lastNProbeMarkdown,
		Whitespace:         "",
		Stdout:             true,
	})
	for surface, body := range map[string]string{"cli": cliBody, "mcp": mcpBody} {
		label := surface + " all"
		assertLastNProbeContains(t, label, body, lastNProbeSummaryProse)
		assertLastNProbeOrder(t, label, body, []string{lastNProbeBeforeAnswer, lastNProbeChatOne, lastNProbeChatTwo, lastNProbeChatThree})
		assertLastNProbeOmits(t, label, body, lastNProbeBeforeQuestion)
	}
}

func assertLastNProbeChatTail(t *testing.T, label string, body string) {
	t.Helper()
	assertLastNProbeOrder(t, label, body, []string{lastNProbeChatThree, lastNProbeChatFour, lastNProbeChatFive})
	for _, omitted := range []string{lastNProbeChatOne, lastNProbeChatTwo, lastNProbeBeforeQuestion, lastNProbeBeforeAnswer, lastNProbeToolCommand, lastNProbeToolOutput, lastNProbeToolName} {
		assertLastNProbeOmits(t, label, body, omitted)
	}
}

func assertLastNProbeJSONTurns(t *testing.T, label string, body string) {
	t.Helper()
	var document lastNProbeDocument
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("%s export is not a JSON document: %v\n%s", label, err, body)
	}
	want := []string{lastNProbeChatThree, lastNProbeChatFour, lastNProbeChatFive}
	texts := make([]string, 0, len(document.Messages))
	for _, turn := range document.Messages {
		texts = append(texts, turn.Text)
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s export messages = %q, want %q", label, texts, want)
	}
}

func assertLastNProbeOrder(t *testing.T, label string, body string, ordered []string) {
	t.Helper()
	previous := -1
	for _, text := range ordered {
		position := strings.Index(body, text)
		if position < 0 {
			t.Errorf("%s export does not contain %q:\n%s", label, text, body)
			continue
		}
		if position < previous {
			t.Errorf("%s export contains %q out of order:\n%s", label, text, body)
		}
		if count := strings.Count(body, text); count != 1 {
			t.Errorf("%s export contains %q %d times, want once:\n%s", label, text, count, body)
		}
		previous = position
	}
}

func assertLastNProbeContains(t *testing.T, label string, body string, text string) {
	t.Helper()
	if !strings.Contains(body, text) {
		t.Errorf("%s export does not contain %q:\n%s", label, text, body)
	}
}

func assertLastNProbeOmits(t *testing.T, label string, body string, text string) {
	t.Helper()
	if strings.Contains(body, text) {
		t.Errorf("%s export contains excluded text %q:\n%s", label, text, body)
	}
}

// lastNProbeChatRecords returns transcript records with a compacted segment,
// chat messages, tool calls, and tool results.
func lastNProbeChatRecords(t *testing.T) []string {
	t.Helper()
	records := lastNProbeCompactedHead(t, lastNProbeChatSession)
	for index, text := range []string{lastNProbeChatOne, lastNProbeChatTwo, lastNProbeChatThree, lastNProbeChatFour, lastNProbeChatFive} {
		records = append(records, lastNProbeTextRecord(t, lastNProbeChatSession, len(records), lastNProbeRole(index), text))
	}
	for call := range lastNProbeToolCallCount {
		callID := fmt.Sprintf("toolu_lastnprobe_%d", call)
		records = append(records, lastNProbeToolUseRecord(t, lastNProbeChatSession, len(records), callID, lastNProbeToolCommand))
		records = append(records, lastNProbeToolResultRecord(t, lastNProbeChatSession, len(records), callID, lastNProbeToolOutput))
	}
	return records
}

// lastNProbeTailRecords returns transcript records with a compacted segment,
// chat messages, tool calls, tool results, and thinking messages.
func lastNProbeTailRecords(t *testing.T) []string {
	t.Helper()
	session := lastNProbeTailSession
	records := lastNProbeCompactedHead(t, session)
	records = append(records, lastNProbeTextRecord(t, session, len(records), "user", tailProbeChatEarly))
	records = append(records, lastNProbeToolUseRecord(t, session, len(records), "toolu_tailprobe_early", tailProbeToolEarly))
	records = append(records, lastNProbeToolResultRecord(t, session, len(records), "toolu_tailprobe_early", lastNProbeToolOutput))
	records = append(records, lastNProbeThinkingRecord(t, session, len(records), tailProbeThinkingOne))
	records = append(records, lastNProbeThinkingRecord(t, session, len(records), tailProbeThinkingTwo))
	records = append(records, lastNProbeTextRecord(t, session, len(records), "assistant", tailProbeChatLate))
	records = append(records, lastNProbeToolUseRecord(t, session, len(records), "toolu_tailprobe_late", tailProbeToolLate))
	records = append(records, lastNProbeToolResultRecord(t, session, len(records), "toolu_tailprobe_late", lastNProbeToolOutput))
	records = append(records, lastNProbeThinkingRecord(t, session, len(records), tailProbeThinkingThree))
	return records
}

// lastNProbeAllRecords returns the compacted head records followed by three
// chat messages that alternate between user and assistant.
func lastNProbeAllRecords(t *testing.T) []string {
	t.Helper()
	records := lastNProbeCompactedHead(t, lastNProbeAllSession)
	for index, text := range []string{lastNProbeChatOne, lastNProbeChatTwo, lastNProbeChatThree} {
		records = append(records, lastNProbeTextRecord(t, lastNProbeAllSession, len(records), lastNProbeRole(index), text))
	}
	return records
}

// lastNProbeCompactedHead returns a question, an answer, a compact boundary,
// and a compact summary for the given session.
func lastNProbeCompactedHead(t *testing.T, session string) []string {
	t.Helper()
	summary := lastNProbeSummaryPreface + lastNProbeSummaryProse
	return []string{
		lastNProbeTextRecord(t, session, 0, "user", lastNProbeBeforeQuestion),
		lastNProbeTextRecord(t, session, 1, "assistant", lastNProbeBeforeAnswer),
		lastNProbeRecord(session, "system", 2, `"subtype":"compact_boundary","content":"Conversation compacted","logicalParentUuid":"`+lastNProbeUUID(1)+`","compactMetadata":{"trigger":"manual","preTokens":1200}`),
		lastNProbeRecord(session, "user", 3, `"isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":`+lastNProbeJSONText(t, summary)+`}`),
	}
}

func lastNProbeRole(index int) string {
	if index%2 == 0 {
		return "user"
	}
	return "assistant"
}

func lastNProbeTextRecord(t *testing.T, session string, index int, role string, text string) string {
	t.Helper()
	if role == "user" {
		return lastNProbeRecord(session, role, index, `"message":{"role":"user","content":`+lastNProbeJSONText(t, text)+`}`)
	}
	return lastNProbeRecord(session, role, index, `"message":{"role":"assistant","content":[{"type":"text","text":`+lastNProbeJSONText(t, text)+`}]}`)
}

func lastNProbeToolUseRecord(t *testing.T, session string, index int, callID string, command string) string {
	t.Helper()
	input := `{"command":` + lastNProbeJSONText(t, command) + `,"description":` + lastNProbeJSONText(t, command) + `}`
	return lastNProbeRecord(session, "assistant", index, `"message":{"role":"assistant","content":[{"type":"tool_use","id":"`+callID+`","name":"`+lastNProbeToolName+`","input":`+input+`}]}`)
}

func lastNProbeToolResultRecord(t *testing.T, session string, index int, callID string, output string) string {
	t.Helper()
	return lastNProbeRecord(session, "user", index, `"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+callID+`","content":`+lastNProbeJSONText(t, output)+`}]}`)
}

func lastNProbeThinkingRecord(t *testing.T, session string, index int, thinking string) string {
	t.Helper()
	return lastNProbeRecord(session, "assistant", index, `"message":{"role":"assistant","content":[{"type":"thinking","thinking":`+lastNProbeJSONText(t, thinking)+`,"signature":"`+lastNProbeSignature+`"}]}`)
}

func lastNProbeRecord(session string, recordType string, index int, fields string) string {
	parentField := `"parentUuid":null`
	if index > 0 {
		parentField = `"parentUuid":"` + lastNProbeUUID(index-1) + `"`
	}
	timestamp := time.Date(2026, 7, 2, 12, 0, index, 0, time.UTC).Format(time.RFC3339)
	return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":%q,"uuid":%q,%s,"timestamp":%q,%s}`, recordType, session, lastNProbeWorkspace, lastNProbeUUID(index), parentField, timestamp, fields)
}

func lastNProbeUUID(index int) string {
	return fmt.Sprintf("00000000-0000-4000-9000-%012d", index)
}

func lastNProbeJSONText(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("encode probe text: %v", err)
	}
	return string(encoded)
}

func installLastNProbeTranscript(t *testing.T, home string, session string, records []string) {
	t.Helper()
	path := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(lastNProbeWorkspace, "/", "-"), session+".jsonl")
	installLocalProofTranscript(t, path, strings.Join(records, "\n")+"\n")
}

func runLastNProbeCLI(t *testing.T, binaryPath string, environment []string, arguments []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, arguments...)
	command.Env = environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("clyde %s failed: %v\n%s", strings.Join(arguments, " "), err, stderr.String())
	}
	return string(output)
}

func lastNProbeMCPBody(t *testing.T, binaryPath string, environment []string, arguments lastNProbeArguments) string {
	t.Helper()
	response, err := callLastNProbeMCP(binaryPath, environment, arguments)
	if err != nil {
		t.Fatalf("clyde_export_transcript call failed %+v: %v", arguments, err)
	}
	if response.Error != nil || response.Result == nil || response.Result.IsError {
		t.Fatalf("clyde_export_transcript returned an error %+v: %+v %+v", arguments, response.Error, response.Result)
	}
	return response.Result.text()
}

func waitForLastNProbeMCP(t *testing.T, binaryPath string, environment []string, session string) {
	t.Helper()
	arguments := lastNProbeArguments{
		ConversationID:     "claude:" + session,
		Only:               []string{lastNProbeAll},
		LastN:              0,
		IncludeCompactions: "",
		FullHistory:        false,
		MaxTokens:          "",
		Format:             lastNProbeMarkdown,
		Whitespace:         "",
		Stdout:             true,
	}
	deadline := time.Now().Add(localProofReadyTimeout)
	for {
		response, err := callLastNProbeMCP(binaryPath, environment, arguments)
		if err == nil && response.Error == nil && response.Result != nil && !response.Result.IsError {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox daemon did not export the probe conversation before the timeout: %v %+v", err, response)
		}
		time.Sleep(localProofPollDelay)
	}
}

// callLastNProbeMCP starts the MCP server, initializes a session, calls
// clyde_export_transcript with the given arguments, and returns its JSON RPC response.
func callLastNProbeMCP(binaryPath string, environment []string, arguments lastNProbeArguments) (exportProbeRPCResponse, error) {
	var empty exportProbeRPCResponse
	call, err := json.Marshal(lastNProbeRPCRequest{JSONRPC: "2.0", ID: exportProbeRequestID, Method: "tools/call", Params: lastNProbeCall{Name: exportProbeToolName, Arguments: arguments}})
	if err != nil {
		return empty, fmt.Errorf("encode clyde_export_transcript call: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, "mcp", "serve")
	command.Env = environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		return empty, fmt.Errorf("open clyde mcp serve stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return empty, fmt.Errorf("open clyde mcp serve stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		return empty, fmt.Errorf("start clyde mcp serve: %w", err)
	}
	messages := strings.Join([]string{localProofInitializeRequest, localProofInitializedMessage, string(call)}, "\n") + "\n"
	_, writeErr := stdin.Write([]byte(messages))
	response, readErr := readExportProbeResponse(stdout)
	closeErr := stdin.Close()
	waitErr := command.Wait()
	if failure := errors.Join(writeErr, readErr, closeErr); failure != nil {
		return empty, fmt.Errorf("clyde mcp serve: %w (%v)\n%s", failure, waitErr, stderr.String())
	}
	return response, nil
}
