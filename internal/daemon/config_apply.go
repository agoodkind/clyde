package daemon

import (
	"context"
	"fmt"
	"log/slog"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
)

// applyConfigInProcess applies a hot-appliable config change without re-exec. It
// swaps the adapter model registry, every MITM proxy's provider routing, and the
// provider-stats pricing table, then advances the current-config pointer. If
// any component apply fails, it returns the error without advancing the pointer,
// so the caller falls back to the
// quiet-wait reload route and the running generation keeps serving the config it
// already had.
//
// The set of changes that reach this path is bounded by
// config.ClassifyConfigChange returning RouteHotApply: only the adapter
// model-registry inputs and MITM provider routing. No worker restart is needed
// for that set, so this performs only atomic config-derived state swaps.
func (r *runtimeServices) applyConfigInProcess(ctx context.Context, log *slog.Logger, newCfg *config.Config) error {
	if r == nil || newCfg == nil {
		return fmt.Errorf("apply config: nil runtime or config")
	}
	if r.adapter != nil {
		if err := r.adapter.ApplyConfig(newCfg.Adapter); err != nil {
			log.WarnContext(ctx, "daemon.config.apply_adapter_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
			return fmt.Errorf("apply adapter config: %w", err)
		}
	}
	for id, proxy := range r.mitmProxies {
		if err := proxy.ApplyConfig(newCfg.MITM); err != nil {
			log.WarnContext(ctx, "daemon.config.apply_mitm_failed", "concern", "daemon.workers.reload", "component", "daemon", "listener_id", id, "err", err)
			return fmt.Errorf("apply mitm config for %q: %w", id, err)
		}
	}
	if r.stats != nil {
		pricing := adapterruntime.NewPricingTable(newCfg.Adapter.ModelPricing())
		r.stats.replacePricing(pricing)
	}
	r.currentConfig.Store(newCfg)
	log.InfoContext(ctx, "daemon.config.applied_in_process", "concern", "daemon.workers.reload", "component", "daemon")
	return nil
}
