package daemon_test

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/sandbox"
)

const localProofRefusedAddress = "[::1]:1"

type localProofSearchRequest struct {
	Query           string `json:"query"`
	Provider        string `json:"provider,omitempty"`
	Workspace       string `json:"workspace,omitempty"`
	Roles           string `json:"roles,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	IncludeArchived bool   `json:"include_archived,omitempty"`
}

type localProofConversation struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	WorkspaceRoot string `json:"workspace_root"`
	Archived      bool   `json:"archived"`
}

type localProofMatch struct {
	Conversation localProofConversation `json:"conversation"`
	MessageIndex int                    `json:"message_index"`
	Role         string                 `json:"role"`
	Score        float64                `json:"score"`
	Snippet      string                 `json:"snippet"`
}

type localProofSearchOutput struct {
	ReturnedCount int               `json:"returned_count"`
	Limit         int               `json:"limit"`
	Offset        int               `json:"offset"`
	HasMore       bool              `json:"has_more"`
	Source        string            `json:"source"`
	Matches       []localProofMatch `json:"matches"`
}

type localProofToolCall struct {
	Name      string                  `json:"name"`
	Arguments localProofSearchRequest `json:"arguments"`
}

type localProofRPCRequest struct {
	JSONRPC string             `json:"jsonrpc"`
	ID      int                `json:"id"`
	Method  string             `json:"method"`
	Params  localProofToolCall `json:"params"`
}

type localProofToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type localProofToolResult struct {
	IsError           bool                    `json:"isError"`
	Content           []localProofToolContent `json:"content"`
	StructuredContent *localProofSearchOutput `json:"structuredContent"`
}

type localProofRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type localProofRPCResponse struct {
	ID     int                   `json:"id"`
	Result *localProofToolResult `json:"result"`
	Error  *localProofRPCError   `json:"error"`
}

type localProofUpsertRecord struct {
	Time                 time.Time `json:"time"`
	Message              string    `json:"msg"`
	Conversations        int       `json:"conversations"`
	RowsWritten          int       `json:"rows_written"`
	WrittenConversations []string  `json:"written_conversations"`
	VectorsEmbedded      int       `json:"vectors_embedded"`
}

type localProofMatchKey struct {
	conversationID string
	messageIndex   int
	role           string
}

type localProofSurface struct {
	name   string
	search func(request localProofSearchRequest) (localProofSearchOutput, error)
}

type localProofDaemon struct {
	binaryPath  string
	roots       sandbox.Roots
	environment []string
	stderr      *localProofBuffer
	stop        func()
}

type localProofBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *localProofBuffer) Write(content []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(content)
}

func (b *localProofBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func localProofClosedAddress(t *testing.T) string {
	t.Helper()
	return localProofRefusedAddress
}

func localProofProviderEnvironment(t *testing.T, home string) []string {
	t.Helper()
	environment := []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(home, ".codex"),
	}
	for _, name := range []string{"CLYDE_CURSOR_PROJECTS_DIRS", "CLYDE_CURSOR_DATA_DIRS", "CLYDE_ZED_DATA_DIRS", "COPILOT_HOME"} {
		directory := filepath.Join(home, "providers", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
		environment = append(environment, name+"="+directory)
	}
	return environment
}

func startLocalProofDaemon(t *testing.T, binaryPath string, home string, flags ...string) *localProofDaemon {
	t.Helper()
	providerEnvironment := localProofProviderEnvironment(t, home)
	arguments := append([]string{"daemon", "sandbox"}, flags...)
	command := exec.Command(binaryPath, arguments...)
	command.Env = environmentWith(providerEnvironment...)
	stderr := &localProofBuffer{mu: sync.Mutex{}, buffer: bytes.Buffer{}}
	command.Stderr = stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open sandbox daemon stdout: %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start sandbox daemon: %v", err)
	}
	exited := make(chan error, 1)
	rootLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if root, found := strings.CutPrefix(line, sandboxRootBannerPrefix); found {
				select {
				case rootLine <- strings.TrimSpace(root):
				default:
				}
			}
		}
		exited <- command.Wait()
	}()
	stop := sync.OnceFunc(func() {
		if err := command.Process.Signal(os.Interrupt); err != nil {
			t.Logf("interrupt sandbox daemon: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(sandboxStopTimeout):
			_ = command.Process.Kill()
			t.Errorf("sandbox daemon did not stop after interrupt; stderr:\n%s", stderr.String())
		}
	})
	t.Cleanup(stop)

	select {
	case base := <-rootLine:
		roots := sandbox.Roots{
			Base:    base,
			State:   filepath.Join(base, "state"),
			Config:  filepath.Join(base, "config"),
			Cache:   filepath.Join(base, "cache"),
			Runtime: filepath.Join(base, "run"),
		}
		environment := providerEnvironment
		for _, variable := range sandbox.Env(roots) {
			environment = append(environment, variable.Name+"="+variable.Value)
		}
		return &localProofDaemon{binaryPath: binaryPath, roots: roots, environment: environmentWith(environment...), stderr: stderr, stop: stop}
	case exitErr := <-exited:
		exited <- exitErr
		t.Fatalf("sandbox daemon exited before printing its root: %v\n%s", exitErr, stderr.String())
	case <-time.After(sandboxStartTimeout):
		t.Fatalf("sandbox daemon did not print its root; stderr:\n%s", stderr.String())
	}
	return nil
}

func (d *localProofDaemon) surfaces() []localProofSurface {
	return []localProofSurface{
		{name: "clyde conversation search", search: d.searchCLI},
		{name: "clyde_search", search: d.searchMCP},
	}
}

func (d *localProofDaemon) status(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, d.binaryPath, "daemon", "status")
	command.Env = d.environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("clyde daemon status: %v\n%s", err, output)
	}
	return string(output)
}

func (d *localProofDaemon) config(t *testing.T) string {
	t.Helper()
	path := filepath.Join(d.roots.Config, "clyde", "config.toml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sandbox config %s: %v", path, err)
	}
	return string(content)
}

func (d *localProofDaemon) searchCLI(request localProofSearchRequest) (localProofSearchOutput, error) {
	var empty localProofSearchOutput
	arguments := []string{"--output-format", "json", "conversation", "search", "--query", request.Query, "--limit", strconv.Itoa(request.Limit), "--offset", strconv.Itoa(request.Offset)}
	if request.Provider != "" {
		arguments = append(arguments, "--provider", request.Provider)
	}
	if request.Workspace != "" {
		arguments = append(arguments, "--workspace", request.Workspace)
	}
	if request.Roles != "" {
		arguments = append(arguments, "--roles", request.Roles)
	}
	if request.IncludeArchived {
		arguments = append(arguments, "--include-archived")
	}
	ctx, cancel := context.WithTimeout(context.Background(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, d.binaryPath, arguments...)
	command.Env = d.environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err != nil {
		return empty, fmt.Errorf("clyde conversation search: %w\n%s%s", err, stdout, stderr.String())
	}
	var result localProofSearchOutput
	if err := json.Unmarshal(stdout, &result); err != nil {
		return empty, fmt.Errorf("decode clyde conversation search output: %w\n%s", err, stdout)
	}
	return result, nil
}

func (d *localProofDaemon) searchMCP(request localProofSearchRequest) (localProofSearchOutput, error) {
	var empty localProofSearchOutput
	call, err := json.Marshal(localProofRPCRequest{
		JSONRPC: "2.0",
		ID:      localProofSearchRequestID,
		Method:  "tools/call",
		Params:  localProofToolCall{Name: "clyde_search", Arguments: request},
	})
	if err != nil {
		return empty, fmt.Errorf("encode clyde_search call: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, d.binaryPath, "mcp", "serve")
	command.Env = d.environment
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
	response, readErr := readLocalProofSearchResponse(stdout)
	closeErr := stdin.Close()
	waitErr := command.Wait()
	if failure := errors.Join(writeErr, readErr, closeErr); failure != nil {
		return empty, fmt.Errorf("clyde mcp serve: %w (exit %v)\n%s", failure, waitErr, stderr.String())
	}
	if response.Error != nil {
		return empty, fmt.Errorf("clyde_search returned JSON-RPC error %d: %s", response.Error.Code, response.Error.Message)
	}
	if response.Result == nil {
		return empty, errors.New("clyde_search returned no result")
	}
	if response.Result.IsError || response.Result.StructuredContent == nil {
		texts := make([]string, 0, len(response.Result.Content))
		for _, content := range response.Result.Content {
			texts = append(texts, content.Text)
		}
		return empty, fmt.Errorf("clyde_search returned a tool error: %s", strings.Join(texts, "\n"))
	}
	return *response.Result.StructuredContent, nil
}

func readLocalProofSearchResponse(stdout io.Reader) (localProofRPCResponse, error) {
	var empty localProofRPCResponse
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), localProofScannerBytes)
	for scanner.Scan() {
		var response localProofRPCResponse
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			return empty, fmt.Errorf("decode clyde mcp serve line %q: %w", scanner.Text(), err)
		}
		if response.ID == localProofSearchRequestID {
			return response, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return empty, fmt.Errorf("read clyde mcp serve stdout: %w", err)
	}
	return empty, errors.New("clyde mcp serve closed stdout before answering clyde_search")
}

func localProofSearchAll(t *testing.T, surface localProofSurface, requests []localProofSearchRequest) []localProofSearchOutput {
	t.Helper()
	results := make([]localProofSearchOutput, len(requests))
	failures := make([]error, len(requests))
	var group sync.WaitGroup
	for position, request := range requests {
		group.Go(func() {
			results[position], failures[position] = surface.search(request)
		})
	}
	group.Wait()
	for position, failure := range failures {
		if failure != nil {
			t.Fatalf("%s search %+v: %v", surface.name, requests[position], failure)
		}
	}
	return results
}
