package daemon

import (
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/reorientinject"
	"goodkind.io/clyde/internal/sentinelinject"
)

// TestMitmHooksDisabledByDefault asserts the default MITM config registers no
// request/response hooks, so the proxy path stays unchanged until a feature is
// explicitly enabled.
func TestMitmHooksDisabledByDefault(t *testing.T) {
	t.Parallel()
	if hooks := mitmRequestResponseHooks(config.MITMConfig{}); len(hooks) != 0 {
		t.Fatalf("mitmRequestResponseHooks(default) = %d hooks, want 0", len(hooks))
	}
}

// TestMitmHooksReorientOnlyRegistersOneHook asserts enabling reorient registers
// exactly the reorient injection hook.
func TestMitmHooksReorientOnlyRegistersOneHook(t *testing.T) {
	t.Parallel()
	hooks := mitmRequestResponseHooks(config.MITMConfig{ReorientSummaryInjection: true})
	if len(hooks) != 1 {
		t.Fatalf("mitmRequestResponseHooks(reorient) = %d hooks, want 1", len(hooks))
	}
	if _, ok := hooks[0].(*reorientinject.Hook); !ok {
		t.Fatalf("hook type = %T, want *reorientinject.Hook", hooks[0])
	}
}

// TestSentinelInjectHooksRegistersWhenConfigured asserts a non-empty sentinel
// registers the sentinel rewrite hook.
func TestSentinelInjectHooksRegistersWhenConfigured(t *testing.T) {
	t.Parallel()
	hooks := mitmRequestResponseHooks(config.MITMConfig{Sentinel: "MYKEYWORD"})
	if len(hooks) != 1 {
		t.Fatalf("mitmRequestResponseHooks(sentinel) = %d hooks, want 1", len(hooks))
	}
	if _, ok := hooks[0].(*sentinelinject.Hook); !ok {
		t.Fatalf("hook type = %T, want *sentinelinject.Hook", hooks[0])
	}
}

// TestMitmHooksRegistersSentinelBeforeReorient locks the first-match order: when
// both features are on, sentinel is first so it wins if both hooks would match.
func TestMitmHooksRegistersSentinelBeforeReorient(t *testing.T) {
	t.Parallel()
	hooks := mitmRequestResponseHooks(config.MITMConfig{
		Sentinel:                 "MYKEYWORD",
		ReorientSummaryInjection: true,
	})
	if len(hooks) != 2 {
		t.Fatalf("hooks = %d, want 2", len(hooks))
	}
	if _, ok := hooks[0].(*sentinelinject.Hook); !ok {
		t.Fatalf("hooks[0] = %T, want *sentinelinject.Hook first", hooks[0])
	}
	if _, ok := hooks[1].(*reorientinject.Hook); !ok {
		t.Fatalf("hooks[1] = %T, want *reorientinject.Hook second", hooks[1])
	}
}
