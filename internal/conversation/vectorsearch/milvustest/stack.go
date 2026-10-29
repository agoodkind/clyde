//go:build milvusintegration

// Package milvustest runs a temporary Milvus compose stack for tests with the
// milvusintegration build tag.
package milvustest

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"text/template"
	"time"
)

//go:embed milvus-compose.yml.tmpl
var composeTemplate string

const (
	// IntegrationEnv enables Milvus integration tests when its value is not empty.
	IntegrationEnv       = "CLYDE_MILVUS_INTEGRATION"
	stackReadyTimeout    = 5 * time.Minute
	stackCommandTimeout  = 6 * time.Minute
	stackTearDownTimeout = "30"
)

type composeValues struct {
	Project  string
	GRPCPort int
}

// RequireEnabled skips the test when IntegrationEnv is empty.
func RequireEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv(IntegrationEnv) == "" {
		t.Skipf("set %s=1 (make milvus-integration) to run against a throwaway Milvus stack", IntegrationEnv)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release probe port: %v", err)
	}
	return port
}

// Start runs Milvus v2.6.18 in a temporary compose project and returns its gRPC
// address. Test cleanup removes the project's containers and volumes.
func Start(t *testing.T) string {
	t.Helper()
	idBytes := make([]byte, 4)
	if _, err := rand.Read(idBytes); err != nil {
		t.Fatalf("random project id: %v", err)
	}
	values := composeValues{Project: "clydetest-" + hex.EncodeToString(idBytes), GRPCPort: freePort(t)}
	parsed, err := template.New("compose").Parse(composeTemplate)
	if err != nil {
		t.Fatalf("parse compose template: %v", err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, values); err != nil {
		t.Fatalf("render compose template: %v", err)
	}
	composePath := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(composePath, rendered.Bytes(), 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	compose := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), stackCommandTimeout)
		defer cancel()
		command := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", values.Project, "-f", composePath}, args...)...)
		return command.CombinedOutput()
	}
	t.Cleanup(func() {
		if output, err := compose("down", "--volumes", "--remove-orphans", "--timeout", stackTearDownTimeout); err != nil {
			t.Errorf("tear down %s: %v\n%s (remove it with: docker compose -p %s down -v)", values.Project, err, output, values.Project)
		}
	})
	if output, err := compose("up", "--detach", "--wait", "--wait-timeout", strconv.Itoa(int(stackReadyTimeout.Seconds()))); err != nil {
		t.Fatalf("start %s: %v\n%s", values.Project, err, output)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(values.GRPCPort))
}
