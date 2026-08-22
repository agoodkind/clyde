//go:build livebody

package mitmcontrib

import (
	"os"
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

// TestConversationIDFromRealCapturedBody runs the decoder against a real
// captured request body exported from capture.db. It is build-tagged because it
// needs an operator-supplied file, and exists to prove the decoder handles the
// exact bytes the proxy hands it, which synthetic fixtures cannot establish.
//
//	CLYDE_LIVE_BODY=/tmp/convprobe/run.bin \
//	  go test -tags livebody ./internal/providers/cursor/mitmcontrib -run RealCaptured -v
func TestConversationIDFromRealCapturedBody(t *testing.T) {
	path := os.Getenv("CLYDE_LIVE_BODY")
	if path == "" {
		t.Skip("set CLYDE_LIVE_BODY to a captured request body")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured body: %v", err)
	}
	route := os.Getenv("CLYDE_LIVE_PATH")
	if route == "" {
		route = agentRunPath
	}
	got, ok := routeProvider{}.ConversationIDFromBody(mitm.ExchangeDiagnostic{
		RequestHeader:      nil,
		DecodedRequestBody: body,
		Method:             "POST",
		Path:               route,
		Host:               "api2.cursor.sh",
		Concern:            "providers.mitm.wire",
		HookName:           "",
	})
	if !ok {
		t.Fatalf("no conversation id recovered from %s (%d bytes)", path, len(body))
	}
	t.Logf("recovered conversation id %s from %d bytes", got, len(body))
}
