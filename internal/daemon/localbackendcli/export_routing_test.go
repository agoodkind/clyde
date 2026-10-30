package localbackendcli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	exportProbeCompactedRollout = "019e0000-3a00-7010-bd9f-a6ee71559001"
	exportProbeSizingRollout    = "019e0000-3a00-7010-bd9f-a6ee71559002"
	exportProbeBelowRollout     = "019e0000-3a00-7010-bd9f-a6ee71559003"
	exportProbeAtRollout        = "019e0000-3a00-7010-bd9f-a6ee71559004"
	exportProbeAboveRollout     = "019e0000-3a00-7010-bd9f-a6ee71559005"
	exportProbeWorkspace        = "/export-probe/workspace"
	exportProbeRequestID        = 2
	exportProbeInlineLimit      = 32768
	exportProbeReceiptMaxBytes  = 1024
	exportProbeMultibyteRune    = "é"
	exportProbeMultibyteCount   = 64
	exportProbeTempDirMode      = 0o700
	exportProbeFileMode         = 0o600
	exportProbeFormat           = "plain_text"
	exportProbeToolName         = "clyde_export_transcript"

	exportProbeToolDefinition  = "exportprobe_tool_definition"
	exportProbeDeveloperPrompt = "exportprobe developer prompt"
	exportProbeEarlierUser     = "exportprobe earlier user question"
	exportProbeEarlierAnswer   = "exportprobe earlier assistant answer"
	exportProbeReasoning       = "exportprobe reasoning summary"
	exportProbeReasoningText   = "exportprobe reasoning content text"
	exportProbeImageData       = "data:image/png;base64,ZXhwb3J0cHJvYmUtaW1hZ2UtYnl0ZXM="
	exportProbeImageType       = "input_image"
	exportProbeCipher          = "exportprobe-cipher"
	exportProbeCallName        = "exportprobe_call_name"
	exportProbeCallArguments   = "exportprobe call arguments"
	exportProbeToolOutput      = "exportprobe tool output body"
	exportProbeSummary         = "exportprobe compaction summary"
	exportProbeLaterUser       = "exportprobe question after compaction"
	exportProbeLaterAnswer     = "exportprobe answer after compaction"
	exportProbeRawKey          = `"raw"`
	exportProbeConflictText    = "--stdout cannot be combined with --output"
)

type exportProbeDestination struct {
	Output string
	Stdout bool
}

type exportProbeArguments struct {
	ConversationID string   `json:"conversation_id"`
	Only           []string `json:"only"`
	FullHistory    bool     `json:"full_history"`
	Format         string   `json:"format"`
	Output         string   `json:"output,omitempty"`
	Stdout         bool     `json:"stdout,omitempty"`
}

type exportProbeToolCall struct {
	Name      string               `json:"name"`
	Arguments exportProbeArguments `json:"arguments"`
}

type exportProbeRPCRequest struct {
	JSONRPC string              `json:"jsonrpc"`
	ID      int                 `json:"id"`
	Method  string              `json:"method"`
	Params  exportProbeToolCall `json:"params"`
}

type exportProbePayload struct {
	ConversationID string `json:"conversation_id"`
	Path           string `json:"path"`
	Bytes          int    `json:"bytes"`
}

type exportProbeToolResult struct {
	IsError           bool                    `json:"isError"`
	Content           []localProofToolContent `json:"content"`
	StructuredContent *exportProbePayload     `json:"structuredContent"`
}

type exportProbeRPCResponse struct {
	ID     int                    `json:"id"`
	Result *exportProbeToolResult `json:"result"`
	Error  *localProofRPCError    `json:"error"`
}

func (result exportProbeToolResult) text() string {
	texts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		texts = append(texts, content.Text)
	}
	return strings.Join(texts, "\n")
}

// TestExportFiltersCompactionAndRoutesMCPOutput runs the built clyde binary
// against a Codex rollout with saved compaction context. It checks chat, tool
// output, and explicit content selections in CLI exports. It starts clyde mcp
// serve against a sandbox daemon and checks inline bodies below and at 32768
// bytes, private temporary files above that limit, explicit output receipts,
// write failures, and the stdout and output conflict.
func TestExportFiltersCompactionAndRoutesMCPOutput(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	writeExportProbeRollout(t, home, exportProbeCompactedRollout, exportProbeCompactedBody())
	environment := environmentWith(localProofProviderEnvironment(t, home)...)

	t.Run("chat export omits saved tool and reasoning context", func(t *testing.T) {
		body := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, "chat")
		for _, kept := range []string{exportProbeSummary, exportProbeEarlierUser, exportProbeEarlierAnswer, exportProbeLaterUser, exportProbeLaterAnswer} {
			if !strings.Contains(body, kept) {
				t.Errorf("chat export does not contain expected text %q:\n%s", kept, body)
			}
		}
		if !strings.Contains(body, exportProbeImageType) {
			t.Errorf("chat export does not contain the image placeholder:\n%s", body)
		}
		for _, omitted := range []string{exportProbeToolDefinition, exportProbeDeveloperPrompt, exportProbeReasoning, exportProbeReasoningText, exportProbeCipher, exportProbeCallName, exportProbeCallArguments, exportProbeToolOutput, exportProbeRawKey, exportProbeImageData} {
			if strings.Contains(body, omitted) {
				t.Errorf("chat export contains excluded text %q:\n%s", omitted, body)
			}
		}
	})

	t.Run("saved tool calls and outputs without encrypted content", func(t *testing.T) {
		for _, only := range []string{"tool_outputs", "all"} {
			body := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, only)
			for _, kept := range []string{exportProbeCallName, exportProbeCallArguments, exportProbeToolOutput} {
				if !strings.Contains(body, kept) {
					t.Errorf("tool output export does not contain saved call or output %s %q:\n%s", only, kept, body)
				}
			}
			if strings.Contains(body, exportProbeCipher) {
				t.Errorf("tool output export contains encrypted content %s:\n%s", only, body)
			}
		}
		body := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, "tool_outputs")
		for _, omitted := range []string{exportProbeToolDefinition, exportProbeRawKey} {
			if strings.Contains(body, omitted) {
				t.Errorf("tool output export contains excluded text %q:\n%s", omitted, body)
			}
		}
	})

	t.Run("explicit selections keep their saved context", func(t *testing.T) {
		calls := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, "tool_calls")
		if !strings.Contains(calls, exportProbeCallName) || strings.Contains(calls, exportProbeToolOutput) {
			t.Errorf("tool call export does not contain the call or contains the tool output\n%s", calls)
		}
		summaries := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, "tools")
		if !strings.Contains(summaries, exportProbeCallName) || strings.Contains(summaries, exportProbeCallArguments) || strings.Contains(summaries, exportProbeToolOutput) {
			t.Errorf("tool summary export omits the call name or contains call arguments or tool output\n%s", summaries)
		}
		thinking := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, "thinking")
		if !strings.Contains(thinking, exportProbeReasoning) || !strings.Contains(thinking, exportProbeReasoningText) || strings.Contains(thinking, exportProbeCipher) {
			t.Errorf("thinking export omits the reasoning summary or reasoning content or contains encrypted content\n%s", thinking)
		}
		prompts := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, "system_prompts")
		if !strings.Contains(prompts, exportProbeDeveloperPrompt) {
			t.Errorf("system prompt export does not contain the developer prompt\n%s", prompts)
		}
	})

	sizes := writeExportProbeSizedRollouts(t, binaryPath, environment, home)
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	mcpTemp := t.TempDir()
	mcpEnvironment := append(append([]string{}, sandboxDaemon.environment...), "TMPDIR="+mcpTemp)
	waitForExportProbe(t, binaryPath, mcpEnvironment, exportProbeAboveRollout)

	t.Run("inline MCP export returns bodies up to the limit", func(t *testing.T) {
		for _, testCase := range []struct {
			rollout string
			extra   exportProbeDestination
		}{
			{rollout: exportProbeBelowRollout, extra: exportProbeDestination{Output: "", Stdout: false}},
			{rollout: exportProbeAtRollout, extra: exportProbeDestination{Output: "", Stdout: false}},
			{rollout: exportProbeAtRollout, extra: exportProbeDestination{Output: "-", Stdout: false}},
			{rollout: exportProbeAtRollout, extra: exportProbeDestination{Output: "", Stdout: true}},
		} {
			result := mustExportProbeMCP(t, binaryPath, mcpEnvironment, testCase.rollout, testCase.extra)
			want := sizes[testCase.rollout]
			if result.text() != want {
				t.Errorf("inline mcp export text differs from the cli body %s %v: %d, %d", testCase.rollout, testCase.extra, len(result.text()), len(want))
			}
			if result.StructuredContent == nil || result.StructuredContent.Path != "" || result.StructuredContent.Bytes != len(want) {
				t.Errorf("inline mcp export payload contains a path or has the wrong byte count %s: %+v", testCase.rollout, result.StructuredContent)
			}
		}
	})

	t.Run("inline MCP export above the limit returns a private file", func(t *testing.T) {
		for _, extra := range []exportProbeDestination{{Output: "", Stdout: false}, {Output: "-", Stdout: false}, {Output: "", Stdout: true}} {
			result := mustExportProbeMCP(t, binaryPath, mcpEnvironment, exportProbeAboveRollout, extra)
			assertExportProbeReceipt(t, result, sizes[exportProbeAboveRollout])
			path := result.StructuredContent.Path
			if !strings.HasPrefix(path, mcpTemp+string(filepath.Separator)) {
				t.Errorf("temporary export file is outside the test temporary directory %q %q", path, mcpTemp)
			}
			assertExportProbeMode(t, filepath.Dir(path), exportProbeTempDirMode)
			assertExportProbeMode(t, path, exportProbeFileMode)
		}
	})

	t.Run("explicit MCP output writes the complete file without the body", func(t *testing.T) {
		for _, rollout := range []string{exportProbeBelowRollout, exportProbeAboveRollout} {
			path := filepath.Join(t.TempDir(), "export.txt")
			result := mustExportProbeMCP(t, binaryPath, mcpEnvironment, rollout, exportProbeDestination{Output: path, Stdout: false})
			assertExportProbeReceipt(t, result, sizes[rollout])
			if result.StructuredContent.Path != path {
				t.Errorf("explicit mcp export returned a different path %q %q", result.StructuredContent.Path, path)
			}
		}
		path := filepath.Join(t.TempDir(), "compacted.txt")
		result := mustExportProbeMCP(t, binaryPath, mcpEnvironment, exportProbeCompactedRollout, exportProbeDestination{Output: path, Stdout: false})
		cliBody := exportProbeCLI(t, binaryPath, environment, exportProbeCompactedRollout, "chat")
		assertExportProbeReceipt(t, result, cliBody)
	})

	t.Run("MCP export reports write failures and destination conflicts", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing", "export.txt")
		response, err := callExportProbeMCP(binaryPath, mcpEnvironment, exportProbeBelowRollout, exportProbeDestination{Output: missing, Stdout: false})
		if err != nil || response.Error != nil || response.Result == nil || !response.Result.IsError || !strings.Contains(response.Result.text(), missing) {
			t.Errorf("mcp export to a missing directory did not return a tool error with the path %s: %v %+v", missing, err, response.Result)
		}
		if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("rejected mcp export left a file or the file check failed %s: %v", missing, statErr)
		}
		conflict := filepath.Join(t.TempDir(), "conflict.txt")
		response, err = callExportProbeMCP(binaryPath, mcpEnvironment, exportProbeBelowRollout, exportProbeDestination{Output: conflict, Stdout: true})
		if err != nil || response.Result == nil || response.Result.StructuredContent != nil || !strings.Contains(response.Result.text(), exportProbeConflictText) {
			t.Errorf("mcp export with stdout and an output file did not return the conflict message %s: %v %+v", conflict, err, response.Result)
		}
		if _, statErr := os.Stat(conflict); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("rejected mcp export left a file or the file check failed %s: %v", conflict, statErr)
		}
	})
}

// exportProbeCompactedBody returns a Codex rollout with a compacted record. The
// replacement history contains a tool definition, a developer prompt, a user message
// with an image data URL, an assistant message, reasoning with a summary and
// encrypted content, reasoning with only readable content, a tool call, a tool output,
// a compaction item, and the summary message. Two messages follow the compacted record.
func exportProbeCompactedBody() string {
	replacement := []string{
		`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"` + exportProbeToolDefinition + `"}]}`,
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + exportProbeDeveloperPrompt + `"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + exportProbeEarlierUser + `"},{"type":"` + exportProbeImageType + `","image_url":"` + exportProbeImageData + `"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + exportProbeEarlierAnswer + `"}]}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"` + exportProbeReasoning + `"}],"encrypted_content":"` + exportProbeCipher + `"}`,
		`{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"` + exportProbeReasoningText + `"}]}`,
		`{"type":"function_call","name":"` + exportProbeCallName + `","arguments":"{\"cmd\":\"` + exportProbeCallArguments + `\"}","call_id":"call-export-probe"}`,
		`{"type":"function_call_output","call_id":"call-export-probe","output":"` + exportProbeToolOutput + `"}`,
		`{"type":"compaction","encrypted_content":"` + exportProbeCipher + `"}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + exportProbeSummary + `"}]}`,
	}
	var body strings.Builder
	body.WriteString(exportProbeSessionMeta(exportProbeCompactedRollout))
	fmt.Fprintf(&body, `{"timestamp":"2026-07-01T11:00:02.000Z","type":"compacted","payload":{"message":%q,"replacement_history":[%s]}}`+"\n", exportProbeSummary, strings.Join(replacement, ","))
	fmt.Fprintf(&body, `{"timestamp":"2026-07-01T11:00:03.000Z","type":"event_msg","payload":{"type":"user_message","message":%q}}`+"\n", exportProbeLaterUser)
	fmt.Fprintf(&body, `{"timestamp":"2026-07-01T11:00:04.000Z","type":"event_msg","payload":{"type":"agent_message","message":%q}}`+"\n", exportProbeLaterAnswer)
	return body.String()
}

func exportProbeSessionMeta(id string) string {
	return fmt.Sprintf(`{"timestamp":"2026-07-01T11:00:00.000Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-07-01T11:00:00.000Z","cwd":%q,"originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`+"\n", id, exportProbeWorkspace)
}

func exportProbeTextBody(id string, text string) string {
	return exportProbeSessionMeta(id) +
		fmt.Sprintf(`{"timestamp":"2026-07-01T11:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":%q}}`+"\n", text)
}

func writeExportProbeRollout(t *testing.T, home string, id string, body string) {
	t.Helper()
	path := filepath.Join(home, ".codex", "sessions", "2026", "07", "01", "rollout-2026-07-01T11-00-00-"+id+".jsonl")
	installLocalProofTranscript(t, path, body)
}

// writeExportProbeSizedRollouts measures chat export overhead with a one-byte
// message. It writes three rollouts with multibyte UTF-8 text and chat exports
// one byte below, exactly at, and one byte above 32768 bytes. It returns each CLI
// body by rollout ID.
func writeExportProbeSizedRollouts(t *testing.T, binaryPath string, environment []string, home string) map[string]string {
	t.Helper()
	const sizingText = "a"
	writeExportProbeRollout(t, home, exportProbeSizingRollout, exportProbeTextBody(exportProbeSizingRollout, sizingText))
	overhead := len(exportProbeCLI(t, binaryPath, environment, exportProbeSizingRollout, "chat")) - len(sizingText)
	multibyte := strings.Repeat(exportProbeMultibyteRune, exportProbeMultibyteCount)
	targets := map[string]int{
		exportProbeBelowRollout: exportProbeInlineLimit - 1,
		exportProbeAtRollout:    exportProbeInlineLimit,
		exportProbeAboveRollout: exportProbeInlineLimit + 1,
	}
	bodies := make(map[string]string, len(targets))
	for id, target := range targets {
		padding := target - overhead - len(multibyte)
		writeExportProbeRollout(t, home, id, exportProbeTextBody(id, multibyte+strings.Repeat("a", padding)))
		body := exportProbeCLI(t, binaryPath, environment, id, "chat")
		if len(body) != target || !utf8.ValidString(body) || !strings.Contains(body, multibyte) {
			t.Fatalf("sized rollout export has the wrong length or is not valid utf-8 %s: %d, %d", id, len(body), target)
		}
		bodies[id] = body
	}
	return bodies
}

func exportProbeCLI(t *testing.T, binaryPath string, environment []string, id string, only string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, "conversation", "export", "codex:"+id, "--only", only, "--full-history", "--format", exportProbeFormat, "--stdout")
	command.Env = environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("clyde conversation export failed %s %s: %v\n%s", id, only, err, stderr.String())
	}
	return string(output)
}

func waitForExportProbe(t *testing.T, binaryPath string, environment []string, id string) {
	t.Helper()
	deadline := time.Now().Add(localProofReadyTimeout)
	for {
		response, err := callExportProbeMCP(binaryPath, environment, id, exportProbeDestination{Output: filepath.Join(t.TempDir(), "ready.txt"), Stdout: false})
		if err == nil && response.Error == nil && response.Result != nil && !response.Result.IsError {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox daemon did not export the conversation before the timeout %s: %v %+v", id, err, response)
		}
		time.Sleep(localProofPollDelay)
	}
}

func mustExportProbeMCP(t *testing.T, binaryPath string, environment []string, id string, extra exportProbeDestination) exportProbeToolResult {
	t.Helper()
	response, err := callExportProbeMCP(binaryPath, environment, id, extra)
	if err != nil {
		t.Fatalf("clyde_export_transcript call failed %s %v: %v", id, extra, err)
	}
	if response.Error != nil || response.Result == nil || response.Result.IsError {
		t.Fatalf("clyde_export_transcript returned an error %s %v: %+v %+v", id, extra, response.Error, response.Result)
	}
	return *response.Result
}

// assertExportProbeReceipt checks that a file result reports an absolute path
// and byte count. It checks that the short result text contains the path and
// omits the body, and that the file contains exactly the expected body.
func assertExportProbeReceipt(t *testing.T, result exportProbeToolResult, body string) {
	t.Helper()
	if result.StructuredContent == nil || !filepath.IsAbs(result.StructuredContent.Path) || result.StructuredContent.Bytes != len(body) {
		t.Fatalf("file export payload does not contain an absolute path or the byte count %+v", result.StructuredContent)
	}
	text := result.text()
	if len(text) > exportProbeReceiptMaxBytes || !strings.Contains(text, result.StructuredContent.Path) {
		t.Errorf("file export text is too long or does not contain the path %d:\n%s", len(text), text)
	}
	written, err := os.ReadFile(result.StructuredContent.Path)
	if err != nil {
		t.Fatalf("read exported file %s: %v", result.StructuredContent.Path, err)
	}
	if string(written) != body {
		t.Errorf("exported file differs from the expected body %s: %d, %d", result.StructuredContent.Path, len(written), len(body))
	}
}

func assertExportProbeMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat exported path %s: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("exported path has the wrong permissions %s: %v, %v", path, info.Mode().Perm(), want)
	}
}

// callExportProbeMCP starts clyde mcp serve, sends an initialize request, and
// calls clyde_export_transcript once for a Codex rollout. The call selects chat,
// full history, plain text, and the given destination. The function returns the
// JSON-RPC response.
func callExportProbeMCP(binaryPath string, environment []string, id string, extra exportProbeDestination) (exportProbeRPCResponse, error) {
	var empty exportProbeRPCResponse
	arguments := exportProbeArguments{
		ConversationID: "codex:" + id,
		Only:           []string{"chat"},
		FullHistory:    true,
		Format:         exportProbeFormat,
		Output:         extra.Output,
		Stdout:         extra.Stdout,
	}
	call, err := json.Marshal(exportProbeRPCRequest{JSONRPC: "2.0", ID: exportProbeRequestID, Method: "tools/call", Params: exportProbeToolCall{Name: exportProbeToolName, Arguments: arguments}})
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

func readExportProbeResponse(stdout io.Reader) (exportProbeRPCResponse, error) {
	var empty exportProbeRPCResponse
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), localProofScannerBytes)
	for scanner.Scan() {
		var response exportProbeRPCResponse
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			return empty, fmt.Errorf("decode clyde mcp serve line %q: %w", scanner.Text(), err)
		}
		if response.ID == exportProbeRequestID {
			return response, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return empty, fmt.Errorf("read clyde mcp serve stdout: %w", err)
	}
	return empty, errors.New("clyde mcp serve closed stdout before answering clyde_export_transcript")
}
