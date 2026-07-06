package daemon

import (
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/reorientinject"
)

// TestReorientInjectHooksDisabledByDefault asserts the default MITM config
// registers no request/response hooks, so the proxy path stays unchanged until
// the feature is explicitly enabled.
func TestReorientInjectHooksDisabledByDefault(t *testing.T) {
	t.Parallel()
	if hooks := reorientInjectHooks(config.MITMConfig{}); len(hooks) != 0 {
		t.Fatalf("reorientInjectHooks(default) = %d hooks, want 0", len(hooks))
	}
}

// TestReorientInjectHooksEnabledRegistersOneHook asserts enabling the feature
// registers exactly the reorient injection hook.
func TestReorientInjectHooksEnabledRegistersOneHook(t *testing.T) {
	t.Parallel()
	hooks := reorientInjectHooks(config.MITMConfig{ReorientSummaryInjection: true})
	if len(hooks) != 1 {
		t.Fatalf("reorientInjectHooks(enabled) = %d hooks, want 1", len(hooks))
	}
	if _, ok := hooks[0].(*reorientinject.Hook); !ok {
		t.Fatalf("hook type = %T, want *reorientinject.Hook", hooks[0])
	}
}
