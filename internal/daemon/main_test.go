package daemon

import (
	"os"
	"testing"

	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/util"
)

// TestMain registers Claude's pre-launch session-id minter for the daemon test
// binary. In production the Claude lifecycle's init registers it when the
// binary imports the provider registry, but the daemon test package cannot
// import the lifecycle: the lifecycle imports the daemon, so that would be an
// import cycle. Registering the real minter here keeps daemon tests that create
// session records exercising the same path as production.
func TestMain(m *testing.M) {
	session.RegisterSessionIDMinter(session.ProviderClaude, util.GenerateUUIDE)
	os.Exit(m.Run())
}
