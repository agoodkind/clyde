package responsehook_test

import (
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/agentgateaction"
	"goodkind.io/clyde/internal/mitm"
	_ "goodkind.io/clyde/internal/providers/claude/mitmcontrib"
	"goodkind.io/clyde/internal/responsehook"
)

type unreadableBody struct{}

func (unreadableBody) Bytes() ([]byte, error) {
	panic("compaction matching read the request body")
}

func TestHookRejectsCompactionBeforeProviderMatching(t *testing.T) {
	t.Parallel()
	hook := responsehook.New(agentgateaction.New("/missing/agent-gate"))
	match, err := hook.MatchRequestResponse(mitm.RequestResponseHookRequest{
		Provider: "claude",
		Method:   http.MethodPost,
		Path:     "/v1/messages",
		Body:     unreadableBody{},
		Purpose:  mitm.RequestPurposeCompaction,
	})
	if err != nil {
		t.Fatalf("MatchRequestResponse: %v", err)
	}
	if match.Matched {
		t.Fatal("response action matched compaction")
	}
}
