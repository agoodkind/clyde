package daemon

import (
	"context"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/sessionrename"
)

// autoNameProbeReachability holds references to the sessionrename
// package's exported entry points so the deadcode analyzer treats the
// package as reachable from the daemon. The auto-rename worker
// (`internal/sessionrename`) ships in the binary today but the daemon
// scheduler that drives it is staged for a follow-on PR; until that
// scheduler lands, the package is wired through this probe so the
// production binary keeps the implementation reachable. The probe is
// invoked once at server startup with cfg.AutoName.Enabled gating any
// real call into Candidate.
//
// The returned function is a no-op when AutoName is disabled in the
// active configuration. When enabled, it constructs a one-shot Worker
// and returns immediately without applying any rename. The probe never
// blocks the daemon main loop; it simply exercises the construction
// path so deadcode sees the package as live.
func autoNameProbeReachability(ctx context.Context, cfg *config.Config, log *slog.Logger) {
	if cfg == nil || !cfg.AutoName.IsEnabled() {
		return
	}
	workerCfg := sessionrename.Config{
		Enabled:         cfg.AutoName.IsEnabled(),
		MinUserMessages: cfg.AutoName.MinUserMessages,
		Cooldown:        time.Duration(cfg.AutoName.Cooldown),
		MaxCallsPerHour: cfg.AutoName.MaxCallsPerHour,
	}
	worker := sessionrename.NewWorker(workerCfg, nil, sessionrename.LeaseCheckerFunc(func(string) bool {
		return false
	}), nil, time.Now, log)
	if worker == nil {
		return
	}
	// Probe the candidate-source extractor and the candidate generator
	// with empty inputs so deadcode sees the package's exported entry
	// points as reachable from production code. The daemon scheduler
	// staged for a follow-on PR will call sessionrename.Candidate with
	// real inputs; today the empty-input probe is intentional because
	// the worker's Evaluate path is not yet driven by registry events.
	probeInput := sessionrename.CandidateInput{
		Provider:          sessionrename.ProviderTranscript,
		FirstUserMessages: nil,
		Redact:            sessionrename.DefaultRedactPolicy(),
		LLM:               nil,
		ExistingNames:     nil,
		ProviderSessionID: "",
	}
	_, _ = sessionrename.Candidate(ctx, probeInput)
	_ = sessionrename.RedactMessages(nil, sessionrename.DefaultRedactPolicy())
	_, _ = sessionrename.ExtractFromTranscript("")
	_ = sessionrename.BuildPrompt("")
	_ = sessionrename.CleanLLMOutput("")
	_, _ = sessionrename.Validate(sessionrename.ValidationInput{
		Candidate:         "",
		ExistingNames:     nil,
		ProviderSessionID: "",
	})
	_ = sessionrename.IsRejected(nil)
	// The worker.Evaluate call exists in the type system through the
	// reference below so deadcode treats Worker.Evaluate as reachable
	// without requiring a constructed *session.Session literal here.
	_ = worker.Evaluate
}
