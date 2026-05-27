package contextusage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/contextusage"
	"goodkind.io/clyde/internal/providers/claude/claudepath"
)

// claudeProberID names this provider in the generic contextusage
// registry. The constant lives here rather than in the Claude
// provider's identity package because that package is currently
// unaware of generic context-usage routing.
const claudeProberID = "claude"

// claudeProber adapts the Claude-specific ProbeContextUsage spawn
// machinery to the provider-neutral Prober contract. Callers in
// generic code reach the spawn through Get(claudeProberID).
type claudeProber struct{}

// Probe satisfies the generic contextusage.Prober interface. The
// sessionRef argument is the Claude session UUID and WorkDir is the
// session's workspace root.
//
// Model resolution mirrors the launch and resume paths: claude reads
// the per-session settings file (opts.SessionSettingsFile) layered on
// top of ~/.claude/settings.json. When the caller supplies an explicit
// override via opts.Model this probe writes a one-shot temp settings
// file that copies the per-session file (if any) and swaps in the
// override before pointing claude at the temp path via `--settings`.
//
// The generic RefreshHint is accepted for protocol completeness.
// The claude prober spawns a fresh /context probe on every call, so
// the hint is effectively a no-op today; the field exists so the
// daemon can wire `clyde compact --refresh` through without a
// follow-up interface change once a caching prober path is added.
func (claudeProber) Probe(ctx context.Context, sessionRef string, opts contextusage.ProbeOptions) (contextusage.Snapshot, error) {
	settingsFile, cleanup, err := resolveProbeSettingsFile(opts)
	if err != nil {
		return contextusage.Snapshot{}, err
	}
	defer cleanup()
	return ProbeContextUsage(ctx, ProbeOptions{
		SessionID:    sessionRef,
		WorkDir:      opts.WorkDir,
		SettingsFile: settingsFile,
		Binary:       "",
		Timeout:      0,
	})
}

// resolveProbeSettingsFile returns the `--settings` path the probe
// should hand to claude. When opts.Model is empty the per-session
// settings file is used directly with no cleanup. When opts.Model is
// non-empty a temp settings file is written that copies the
// per-session file (or starts from an empty map when none exists) and
// swaps in the override model; the returned cleanup func removes the
// temp file once the probe returns.
func resolveProbeSettingsFile(opts contextusage.ProbeOptions) (string, func(), error) {
	if opts.Model == "" {
		return opts.SessionSettingsFile, func() {}, nil
	}
	rawSettings, err := loadRawSettings(opts.SessionSettingsFile)
	if err != nil {
		return "", func() {}, err
	}
	tmpPath, err := claudepath.WriteModelOverrideSettings(rawSettings, opts.Model)
	if err != nil {
		return "", func() {}, fmt.Errorf("probe: write model override settings: %w", err)
	}
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("compact.probe.override_cleanup_failed",
				"component", "compact",
				"subcomponent", "probe",
				"path", tmpPath,
				"err", rmErr,
			)
		}
	}
	return tmpPath, cleanup, nil
}

// loadRawSettings reads a session settings JSON file into a generic
// map so caller code can swap a single field (the model override)
// without modeling every known settings key. A missing path returns
// an empty map so callers can write a one-key override file.
func loadRawSettings(path string) (map[string]json.RawMessage, error) {
	if path == "" {
		return map[string]json.RawMessage{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		slog.Warn("compact.probe.settings_read_failed",
			"component", "compact",
			"subcomponent", "probe",
			"path", path,
			"err", err,
		)
		return nil, fmt.Errorf("probe: read settings file %q: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("compact.probe.settings_decode_failed",
			"component", "compact",
			"subcomponent", "probe",
			"path", path,
			"err", err,
		)
		return nil, fmt.Errorf("probe: decode settings file %q: %w", path, err)
	}
	return raw, nil
}

func init() {
	contextusage.Register(claudeProberID, claudeProber{})
}
