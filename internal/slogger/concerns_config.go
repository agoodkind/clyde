package slogger

// Config subsystem concern constants. Covers TOML/JSON config
// loading, validation, runtime-dir creation, and the legacy-key
// migration path under `internal/config/`.
const (
	ConcernConfig = "config"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernConfig: "config/config.jsonl",
	})
}
