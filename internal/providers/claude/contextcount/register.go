package contextcount

import (
	"fmt"
	"log/slog"

	generic "goodkind.io/clyde/internal/contextcount"
)

func init() {
	if err := registerCounters(); err != nil {
		slog.Error("providers.claude.contextcount.register_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "register",
			"err", err,
		)
	}
}

func registerCounters() error {
	if err := generic.Register(generic.CounterSourceAPI, func(deps generic.Deps) (generic.Counter, error) {
		access := deps.Access
		if access == nil {
			access = Credentials{}
		}
		return NewAPICounter(access, APICounterOptions{
			Endpoint: "",
			Client:   nil,
			Sleep:    nil,
			Clock:    deps.Clock,
		}), nil
	}); err != nil {
		slog.Error("providers.claude.contextcount.api_register_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "register",
			"err", err,
		)
		return fmt.Errorf("register api counter: %w", err)
	}
	if err := generic.Register(generic.CounterSourceProbe, func(deps generic.Deps) (generic.Counter, error) {
		return NewProbeCounter(ProbeCounterOptions{
			Binary:  "",
			WorkDir: deps.WorkDir,
			Timeout: deps.Timeout,
			Exec:    nil,
			Clock:   deps.Clock,
		}), nil
	}); err != nil {
		slog.Error("providers.claude.contextcount.probe_register_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "register",
			"err", err,
		)
		return fmt.Errorf("register probe counter: %w", err)
	}
	return nil
}
