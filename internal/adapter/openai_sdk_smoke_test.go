package adapter

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

// openAISDKPythonEnv is the environment variable that stores the path of
// a Python interpreter with the official openai package installed. The
// SDK smoke test skips when the variable is empty.
const openAISDKPythonEnv = "CLYDE_OPENAI_SDK_PYTHON"

// openAISDKSmokeTimeout bounds one SDK smoke run.
const openAISDKSmokeTimeout = 2 * time.Minute

// TestOpenAISDKSmoke runs testdata/openai_sdk_smoke.py with the official
// OpenAI Python SDK against the generic OpenAI listener and a local Codex
// upstream. The script validates every response and stream event with the
// SDK's Pydantic models.
func TestOpenAISDKSmoke(t *testing.T) {
	python := os.Getenv(openAISDKPythonEnv)
	if python == "" {
		t.Skip(openAISDKPythonEnv + " is not set")
	}
	upstream := &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 64),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexConformanceSSEBody(conformanceUsageWithReasoning)))
		},
	}
	listeners := startConformanceServer(t, upstream)
	script, err := filepath.Abs(filepath.Join("testdata", "openai_sdk_smoke.py"))
	if err != nil {
		t.Fatalf("resolve smoke script: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), openAISDKSmokeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, python, script, listeners.openAI+"/v1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("OpenAI SDK smoke failed: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
