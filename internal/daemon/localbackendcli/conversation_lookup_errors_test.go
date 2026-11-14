package localbackendcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/sandbox"
)

type lookupProbeArguments struct {
	ConversationID string   `json:"conversation_id"`
	Output         string   `json:"output,omitempty"`
	Only           []string `json:"only,omitempty"`
}

type lookupProbeCall struct {
	Name      string               `json:"name"`
	Arguments lookupProbeArguments `json:"arguments"`
}

type lookupProbeRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  lookupProbeCall `json:"params"`
}

func TestConversationLookupErrorsAreConcise(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	writeLocalProofFile(
		t,
		filepath.Join(home, ".config", "clyde", "config.toml"),
		"[logging]\nlevel = \"debug\"\n",
	)
	writeExportProbeRollout(t, home, exportProbeCompactedRollout, exportProbeCompactedBody())
	d := startLocalProofDaemon(t, binaryPath, home)
	waitForExportProbe(t, binaryPath, d.environment, exportProbeCompactedRollout)
	conversationID := "codex:" + exportProbeCompactedRollout

	for _, selector := range []string{
		"agent-missing-501",
		"50100000-0000-4000-8000-000000000001",
		filepath.Join(home, "missing transcript.jsonl"),
	} {
		t.Run(
			selector,
			func(t *testing.T) {
				assertLookupProbeMiss(t, d, selector)
			},
		)
	}

	t.Run(
		"diagnostics preserve provider retention",
		func(t *testing.T) {
			assertLookupProbeLog(t, home, conversation.RequestNotFoundReasonNotRetained.Describe())
		},
	)

	t.Run(
		"successful info and export preserve content",
		func(t *testing.T) {
			for _, operation := range []string{"info", "export"} {
				want := exportProbeCompactedRollout
				if operation == "export" {
					want = exportProbeLaterUser
				}
				text, err := lookupProbeCLI(t, d, operation, conversationID, "")
				if err != nil || !strings.Contains(text, want) {
					t.Fatalf("CLI %s failed or omitted %q: %v\n%s", operation, want, err, text)
				}

				result := lookupProbeMCP(t, d, operation, conversationID, "")
				if result.IsError || !strings.Contains(result.text(), want) {
					t.Fatalf("MCP %s failed or omitted %q: %+v", operation, want, result)
				}
			}
		},
	)

	t.Run(
		"unreadable artifacts remain load failures",
		func(t *testing.T) {
			path := codexProbeRolloutPath(home, exportProbeCompactedRollout)
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				if err := os.Chmod(path, 0o600); err != nil {
					t.Error(err)
				}
			})
			assertLookupProbeFailure(t, d, conversationID, "permission denied")
		},
	)

	t.Run(
		"stale misses preserve refresh diagnostics",
		func(t *testing.T) {
			writeExportProbeRollout(
				t,
				home,
				exportProbeSizingRollout,
				exportProbeTextBody(exportProbeSizingRollout, "refresh probe"),
			)
			cachePath := filepath.Join(d.roots.Cache, "clyde", "conversation-index.json")
			if err := os.Chmod(cachePath, 0o400); err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				if err := os.Chmod(cachePath, 0o600); err != nil {
					t.Error(err)
				}
			})
			assertLookupProbeMiss(t, d, "stale-missing-501")
			assertLookupProbeLog(t, home, "this miss may be stale")
			assertLookupProbeLog(t, home, "permission denied")
		},
	)

	t.Run(
		"unavailable daemon remains a connection failure",
		func(t *testing.T) {
			d.stop()

			text, err := lookupProbeCLI(t, d, "info", conversationID, "")
			if err == nil || !strings.Contains(text, "daemon is not running") {
				t.Fatalf("CLI info did not report the unavailable daemon: %v\n%s", err, text)
			}

			for _, operation := range []string{"info", "export"} {
				result := lookupProbeMCP(t, d, operation, conversationID, "")
				if !result.IsError || !strings.Contains(result.text(), "daemon is not running") {
					t.Fatalf("MCP %s did not report the unavailable daemon: %+v", operation, result)
				}
			}
		},
	)

	t.Run(
		"corrupt caches remain storage failures",
		func(t *testing.T) {
			roots, err := sandbox.NewRoots()
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				if err := os.RemoveAll(roots.Base); err != nil {
					t.Error(err)
				}
			})
			cache := filepath.Join(roots.Cache, "clyde", "conversation-index.json")
			installLocalProofTranscript(t, cache, "{")
			if err := os.Chmod(cache, 0o400); err != nil {
				t.Fatal(err)
			}

			broken := startLocalProofDaemon(t, binaryPath, home, "--root", roots.Base)
			deadline := time.Now().Add(sandboxStartTimeout)

			for {
				text, failure := lookupProbeCLI(t, broken, "info", conversationID, "")
				if failure != nil && strings.Contains(text, "decode conversation cache") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf(
						"corrupt-cache daemon did not report the decode failure: %v\n%s\n%s",
						failure,
						text,
						broken.stderr.String(),
					)
				}
				time.Sleep(localProofPollDelay)
			}
			assertLookupProbeFailure(t, broken, conversationID, "decode conversation cache")
		},
	)
}

func assertLookupProbeMiss(t *testing.T, d *localProofDaemon, selector string) {
	t.Helper()
	want := "Conversation " + selector + " not found."

	for _, operation := range []string{"info", "export"} {
		cliPath := filepath.Join(t.TempDir(), "cli-export.txt")
		text, err := lookupProbeCLI(t, d, operation, selector, cliPath)
		if err == nil || text != "Error: "+want+"\n" {
			t.Errorf("CLI %s error = %v, text = %q; want %q", operation, err, text, want)
		}
		mcpPath := filepath.Join(t.TempDir(), "mcp-export.txt")
		result := lookupProbeMCP(t, d, operation, selector, mcpPath)
		if !result.IsError || result.text() != want {
			t.Errorf(
				"MCP %s error = %t, text = %q; want %q",
				operation,
				result.IsError,
				result.text(),
				want,
			)
		}

		for _, path := range []string{cliPath, mcpPath} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("failed export created %s or stat failed: %v", path, err)
			}
		}
	}
}

func assertLookupProbeFailure(
	t *testing.T,
	d *localProofDaemon,
	selector string,
	diagnostic string,
) {
	t.Helper()

	for _, operation := range []string{"info", "export"} {
		text, err := lookupProbeCLI(t, d, operation, selector, "")
		if err == nil || !strings.Contains(text, diagnostic) ||
			strings.Contains(text, " not found.") {
			t.Errorf("CLI %s did not preserve %q: %v\n%s", operation, diagnostic, err, text)
		}
		result := lookupProbeMCP(t, d, operation, selector, "")
		if !result.IsError || !strings.Contains(result.text(), diagnostic) ||
			strings.Contains(result.text(), " not found.") {
			t.Errorf("MCP %s did not preserve %q: %+v", operation, diagnostic, result)
		}
	}
}

func assertLookupProbeLog(t *testing.T, home string, diagnostic string) {
	t.Helper()
	path := filepath.Join(home, ".local", "state", "clyde", localProofDaemonLogName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), diagnostic) {
		t.Errorf("daemon log does not contain %q:\n%s", diagnostic, data)
	}
}

func lookupProbeCLI(
	t *testing.T,
	d *localProofDaemon,
	operation string,
	selector string,
	path string,
) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), localProofCommandTimeout)
	defer cancel()
	arguments := []string{"conversation", operation, selector}
	if operation == "export" {
		arguments = append(arguments, "--only", "chat")
		if path == "" {
			arguments = append(arguments, "--stdout")
		} else {
			arguments = append(arguments, "--output", path)
		}
	}
	command := exec.CommandContext(ctx, d.binaryPath, arguments...)
	command.Env = d.environment
	output, err := command.CombinedOutput()

	return string(output), err
}

func lookupProbeMCP(
	t *testing.T,
	d *localProofDaemon,
	operation string,
	selector string,
	path string,
) exportProbeToolResult {
	t.Helper()
	tool := "clyde_conversation_info"
	var only []string
	if operation == "export" {
		tool = exportProbeToolName
		only = []string{"chat"}
	} else {
		path = ""
	}
	call, err := json.Marshal(lookupProbeRequest{
		JSONRPC: "2.0",
		ID:      exportProbeRequestID,
		Method:  "tools/call",
		Params: lookupProbeCall{
			Name:      tool,
			Arguments: lookupProbeArguments{ConversationID: selector, Output: path, Only: only},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, d.binaryPath, "mcp", "serve")
	command.Env = d.environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	messages := strings.Join(
		[]string{localProofInitializeRequest, localProofInitializedMessage, string(call)},
		"\n",
	) + "\n"
	_, writeErr := stdin.Write([]byte(messages))
	response, readErr := readExportProbeResponse(stdout)
	closeErr := stdin.Close()
	waitErr := command.Wait()
	if failure := errors.Join(writeErr, readErr, closeErr, waitErr); failure != nil {
		t.Fatalf("MCP %s failed: %v\n%s", operation, failure, stderr.String())
	}
	if response.Error != nil || response.Result == nil {
		t.Fatalf(
			"MCP %s did not return a tool result: %+v\n%s",
			operation,
			response,
			stderr.String(),
		)
	}

	return *response.Result
}
