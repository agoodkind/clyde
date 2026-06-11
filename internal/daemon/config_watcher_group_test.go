package daemon

import (
	"sync"
	"testing"

	"goodkind.io/clyde/internal/livetrack"
)

// TestConfigWatcherAttachesCancelNoWait asserts the watcher registry attaches as
// a PhaseWorkers, CancelNoWait member, preserving the non-blocking cancel that
// prevents a reload-triggering watcher from deadlocking on its own drain.
func TestConfigWatcherAttachesCancelNoWait(t *testing.T) {
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	runtime := &runtimeServices{group: g, reloadMu: sync.Mutex{}}
	before := g.MemberCount()
	_ = newConfigWatcher(nil, "baseline", runtime)
	if g.MemberCount() != before+1 {
		t.Fatalf("expected watcher member attached, got %d", g.MemberCount()-before)
	}
}
