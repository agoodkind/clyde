package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const embeddingRefusalCause = "context_length_exceeded at unix:///private/run/search.sock from lm-semantic-search using NV-EmbedCode-7b-v1 with credential sk-private"

func TestConversationSearchCommandSurfacesEmbeddingRefusal(t *testing.T) {
	socketPath := conversationSearchSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on daemon socket: %v", err)
	}
	grpcServer := grpc.NewServer()
	searchIndex := &fakeSearchIndex{records: nil, matchingErr: nil}
	semantic := &fakeSemanticSearch{
		hits: nil,
		err:  status.Error(codes.InvalidArgument, embeddingRefusalCause),
	}
	clydev1.RegisterClydeServiceServer(grpcServer, &controlServer{
		index: conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{}),
		searchSource: &semanticConversationSearchSource{
			index: searchIndex,
			searchClient: func() conversationSemanticSearchClient {
				return semantic
			},
			collectionID: "conversations",
		},
	})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		select {
		case serverErr := <-serveErr:
			if serverErr != nil && !errors.Is(serverErr, grpc.ErrServerStopped) {
				t.Errorf("serve daemon gRPC: %v", serverErr)
			}
		case <-time.After(time.Second):
			t.Error("daemon gRPC server did not stop")
		}
	})

	binaryPath := buildConversationSearchCLI(t)
	configRoot := t.TempDir()
	configDir := filepath.Join(configRoot, "clyde")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("create CLI config directory: %v", err)
	}
	configText := fmt.Sprintf("[daemon]\ngrpc_address = %q\n", "unix://"+socketPath)
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatalf("write CLI config: %v", err)
	}

	command := exec.CommandContext(
		t.Context(),
		binaryPath,
		"conversation",
		"search",
		"--query",
		"alpha beta gamma",
	)
	command.Env = environmentWithOverrides(
		"HOME="+t.TempDir(),
		"XDG_CACHE_HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+configRoot,
		"XDG_RUNTIME_DIR="+t.TempDir(),
		"XDG_STATE_HOME="+t.TempDir(),
	)
	outputBytes, err := command.CombinedOutput()
	output := string(outputBytes)
	if err == nil {
		t.Fatalf("conversation search exited zero; output:\n%s", output)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
		t.Fatalf("conversation search error = %v, want a non-zero exit; output:\n%s", err, output)
	}
	for _, expected := range []string{
		string(conversationSearchSourceRefused),
		"conversation search source refused the query",
		"InvalidArgument",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("conversation search output missing %q:\n%s", expected, output)
		}
	}
	for _, unsafeText := range []string{
		"/private/run/search.sock",
		"lm-semantic-search",
		"NV-EmbedCode-7b-v1",
		"sk-private",
	} {
		if strings.Contains(output, unsafeText) {
			t.Fatalf("conversation search output exposed %q:\n%s", unsafeText, output)
		}
	}
	if strings.Contains(output, "No results found") {
		t.Fatalf("conversation search rendered a false empty result:\n%s", output)
	}
}

func conversationSearchSocketPath(t *testing.T) string {
	t.Helper()
	socketFile, err := os.CreateTemp("/tmp", "clyde-search-refusal-*.sock")
	if err != nil {
		t.Fatalf("create daemon socket path: %v", err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatalf("close daemon socket placeholder: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove daemon socket placeholder: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove daemon socket: %v", err)
		}
	})
	return socketPath
}

func buildConversationSearchCLI(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := daemonTestRepositoryRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	binaryPath := filepath.Join(t.TempDir(), "clyde")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binaryPath, "./cmd/clyde")
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build clyde CLI: %v\n%s", err, output)
	}
	return binaryPath
}

func daemonTestRepositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", os.ErrNotExist
		}
		directory = parent
	}
}

func environmentWithOverrides(overrides ...string) []string {
	overrideKeys := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		key, _, found := strings.Cut(override, "=")
		if found {
			overrideKeys[key] = true
		}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && overrideKeys[key] {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, overrides...)
}
