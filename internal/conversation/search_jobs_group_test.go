package conversation

import (
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// TestSearchManagerAttachesAsWorker asserts the search job registry attaches as
// a PhaseWorkers member that is not quiet-relevant, so background jobs never
// hold a reload to its max-wait.
func TestSearchManagerAttachesAsWorker(t *testing.T) {
	t.Parallel()
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	before := g.MemberCount()
	_ = NewSearchJobManager(NewIndex(nil), nil, config.SearchConfig{}, nil, nil, "", g)
	if g.MemberCount() != before+1 {
		t.Fatalf("expected 1 worker member attached, got %d", g.MemberCount()-before)
	}
}
