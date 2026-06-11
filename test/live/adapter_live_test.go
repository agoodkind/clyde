//go:build live

package live

import (
	"strings"
	"testing"
	"time"
)

// TestLiveAdapterHotApplyModelAlias boots the adapter on the fake port, captures
// the worker pid, edits a config-only field (adds a model alias), and asserts
// the daemon hot-applies it: the worker pid is unchanged (no re-exec) and the
// new alias serves on the next /v1/models request.
func TestLiveAdapterHotApplyModelAlias(t *testing.T) {
	up := startSlowUpstream(t)
	up.releaseAll() // this test does not need an in-flight request
	h := newHarness(t)
	h.writeAdapterConfig(t, h.cfg.AdapterPort, up.baseURL(), nil)
	h.boot(t)

	pidBefore := h.latestWorkerPid()
	if pidBefore == 0 {
		t.Fatal("could not read worker pid before apply")
	}
	const alias = "clyde-hot-alias"
	if strings.Contains(h.adapterGet(t, h.cfg.AdapterPort, "/v1/models"), alias) {
		t.Fatalf("alias %q present before apply", alias)
	}

	// Add the model alias: a hot-set change (adapter Models).
	h.writeAdapterConfig(t, h.cfg.AdapterPort, up.baseURL(), []string{alias})

	if !h.waitForDaemonLog(hotApplyMarker, 10*time.Second) {
		t.Fatalf("hot apply did not occur (no %q)", hotApplyMarker)
	}
	if h.logContains(reloadTriggeredKey) {
		t.Fatalf("hot apply unexpectedly re-execed (saw %q)", reloadTriggeredKey)
	}
	if pidAfter := h.latestWorkerPid(); pidAfter != pidBefore {
		t.Fatalf("worker pid changed on hot apply: %d -> %d (re-exec)", pidBefore, pidAfter)
	}
	// The new alias must serve on the next request.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.adapterGet(t, h.cfg.AdapterPort, "/v1/models"), alias) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("alias %q did not serve after hot apply", alias)
}

// TestLiveAdapterQuietWaitDefersUntilRequestFinishes boots the adapter, starts a
// slow in-flight chat request that holds an adapter egress session, edits a
// reload-routed field, and asserts the reload defers until the request finishes:
// no reload_triggered while the request is in flight, then reload_triggered after
// the upstream is released.
func TestLiveAdapterQuietWaitDefersUntilRequestFinishes(t *testing.T) {
	up := startSlowUpstream(t)
	h := newHarness(t)
	h.writeAdapterConfig(t, h.cfg.AdapterPort, up.baseURL(), nil)
	h.boot(t)

	resp := h.postChat(t, h.cfg.AdapterPort)
	up.waitForRequest(t, 10*time.Second) // egress session now in flight

	// Edit a reload-routed field (require_token is not in the hot set).
	h.writeReloadEdit(t, up.baseURL())

	// While the request is in flight, the watcher must be waiting for quiet and
	// must NOT have triggered the reload yet.
	if !h.waitForDaemonLog(`"route":"reload"`, 10*time.Second) {
		t.Fatal("reload-routed edit was not classified as reload")
	}
	time.Sleep(1 * time.Second)
	if h.logContains(reloadTriggeredKey) {
		t.Fatal("reload fired while a request was still in flight (quiet-wait did not defer)")
	}

	// Release the upstream so the request finishes, then the reload proceeds.
	up.releaseAll()
	<-resp
	if !h.waitForDaemonLog(reloadTriggeredKey, 15*time.Second) {
		t.Fatal("reload did not fire after the in-flight request finished")
	}
}

// TestLiveAdapterQuietWaitBoundFiresCap boots with a short quiet-wait bound,
// starts a slow request that outlives the bound, edits a reload-routed field,
// and asserts the cap fires: the watcher logs quiet_wait_timeout and proceeds to
// reload before the request is released.
func TestLiveAdapterQuietWaitBoundFiresCap(t *testing.T) {
	up := startSlowUpstream(t)
	h := newHarness(t)
	h.extraEnv = []string{"CLYDE_RELOAD_QUIET_WAIT=2s"}
	h.writeAdapterConfig(t, h.cfg.AdapterPort, up.baseURL(), nil)
	h.boot(t)

	resp := h.postChat(t, h.cfg.AdapterPort)
	up.waitForRequest(t, 10*time.Second)

	h.writeReloadEdit(t, up.baseURL())

	// The request stays in flight past the 2s bound, so the cap fires: the
	// watcher logs the quiet-wait timeout and the reload proceeds without waiting
	// for the request.
	if !h.waitForDaemonLog("daemon.config_watch.quiet_wait_timeout", 15*time.Second) {
		t.Fatal("quiet-wait cap did not fire on a request longer than the bound")
	}
	up.releaseAll()
	<-resp
}

// TestLiveAdapterTopologyReexecs boots the adapter, captures the worker pid,
// moves the adapter listener to a different fake port, and asserts the change
// routes to a rebind re-exec: route=rebind, the rebind drain starts, and the
// worker pid advances.
func TestLiveAdapterTopologyReexecs(t *testing.T) {
	up := startSlowUpstream(t)
	up.releaseAll()
	h := newHarness(t)
	h.writeAdapterConfig(t, h.cfg.AdapterPort, up.baseURL(), nil)
	h.boot(t)

	pidBefore := pidOnPort(h.cfg.AdapterPort)
	if pidBefore == 0 {
		t.Fatalf("no process listening on adapter port %d before topology change", h.cfg.AdapterPort)
	}

	// Move the adapter listener to the fake topology port.
	h.writeAdapterConfig(t, h.cfg.TopologyPort, up.baseURL(), nil)

	if !h.waitForDaemonLog(`"route":"rebind"`, 15*time.Second) {
		t.Fatal("adapter port change was not classified as rebind")
	}
	if h.logContains(hotApplyMarker) {
		t.Fatalf("topology change unexpectedly hot-applied (saw %q)", hotApplyMarker)
	}
	// The rebind re-execs and binds the moved port fresh: a new process must own
	// the new port, the old port must be released, and the pid must change.
	pidAfter := waitForPidOnPort(h.cfg.TopologyPort, 20*time.Second)
	if pidAfter == 0 {
		t.Fatalf("nothing bound the moved adapter port %d after rebind", h.cfg.TopologyPort)
	}
	if pidAfter == pidBefore {
		t.Fatalf("worker pid did not change after rebind (still %d)", pidBefore)
	}
	if pidOnPort(h.cfg.AdapterPort) != 0 {
		t.Fatalf("old adapter port %d still held after rebind", h.cfg.AdapterPort)
	}
}
