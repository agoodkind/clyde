package config

import (
	"slices"

	"goodkind.io/clyde/internal/providerid"
)

// MITMConfig configures the local capture proxy and its persistence.
type MITMConfig struct {
	EnabledDefault bool                   `json:"enabledDefault,omitempty" toml:"enabled_default,omitempty"`
	Providers      MITMProviderSet        `json:"providers,omitempty" toml:"providers,omitempty"`
	CaptureDir     string                 `json:"captureDir,omitempty" toml:"capture_dir,omitempty"`
	Capture        MITMCapture            `json:"capture,omitzero" toml:"capture,omitempty"`
	CaptureRules   []MITMCaptureRouteRule `json:"captureRules,omitempty" toml:"capture_rules,omitempty"`
	Drift          MITMDriftConfig        `json:"drift,omitzero" toml:"drift,omitempty"`
	// ReorientSummaryInjection turns on the MITM reorient hook: when set, the
	// proxy detects a Claude Code compaction summarization request and rewrites
	// its streaming summary response, appending the recovered pre-compaction
	// transcript (read off disk by the request's session id) so the client
	// persists it in the isCompactSummary message. Default false; enabling it
	// makes the proxy read the local transcript and rewrite that one response.
	ReorientSummaryInjection bool `json:"reorientSummaryInjection,omitempty" toml:"reorient_summary_injection,omitempty"`
	// ReorientInjectMaxTokens caps the injected transcript before the hook also
	// applies its context-window fraction. Zero uses the hook default.
	ReorientInjectMaxTokens int `json:"reorientInjectMaxTokens,omitempty" toml:"reorient_inject_max_tokens,omitempty"`
	// ReorientRecentFraction is the fraction of conversation messages, by count,
	// the R2 request-trim split reattaches verbatim as the recent half. A larger
	// fraction reattaches more of the conversation and summarizes less; the injected
	// half is still bounded by the byte cap derived from ReorientInjectMaxTokens.
	// Zero uses the hook default (0.5).
	ReorientRecentFraction float64 `json:"reorientRecentFraction,omitempty" toml:"reorient_recent_fraction,omitempty"`
	// ReorientContextWindowFraction is the fraction of the request's context window
	// the injection may fill. Zero uses the hook default (0.5).
	ReorientContextWindowFraction float64 `json:"reorientContextWindowFraction,omitempty" toml:"reorient_context_window_fraction,omitempty"`
	// ReorientBytesPerToken is the token-to-byte approximation the hook uses to turn
	// a token budget into a byte cap. Zero uses the hook default (4).
	ReorientBytesPerToken int `json:"reorientBytesPerToken,omitempty" toml:"reorient_bytes_per_token,omitempty"`
	// ReorientStandardContextWindow is the assumed context window, in tokens, for a
	// compaction request without the context-1m beta. Zero uses the hook default
	// (200000).
	ReorientStandardContextWindow int `json:"reorientStandardContextWindow,omitempty" toml:"reorient_standard_context_window,omitempty"`
	// ReorientOneMillionContextWindow is the assumed context window, in tokens, for a
	// compaction request carrying the context-1m beta. Zero uses the hook default
	// (1000000).
	ReorientOneMillionContextWindow int `json:"reorientOneMillionContextWindow,omitempty" toml:"reorient_one_million_context_window,omitempty"`
	// ReorientInjectMaxLines caps the disk-recovered transcript, in lines, on the
	// fallback path taken when no valid request split is computed. Zero uses the
	// conversation renderer default (3500).
	ReorientInjectMaxLines int `json:"reorientInjectMaxLines,omitempty" toml:"reorient_inject_max_lines,omitempty"`
	// App maps each desktop (Electron) client name to its MITM listen endpoint,
	// declared as [mitm.app.<name>] (for example [mitm.app.cursor]). CLI maps
	// each CLI client name, declared as [mitm.cli.<name>] (for example
	// [mitm.cli.claude-code]). The two groups mirror the desktop-via-clyde
	// config's [apps.*] / [clis.*] split.
	App map[string]MITMListenerConfig `json:"app,omitempty" toml:"app,omitempty"`
	CLI map[string]MITMListenerConfig `json:"cli,omitempty" toml:"cli,omitempty"`
	// Listeners is the flattened, normalized id->endpoint map derived from App
	// and CLI at load time under a group-qualified id ("app.<name>" /
	// "cli.<name>"); it is not a TOML field. The daemon binds each address, tags
	// every captured exchange with the id, and keys per-listener proxies and
	// reload-inherited file descriptors from this map. An empty App and CLI
	// default to a single "default" listener.
	Listeners map[string]MITMListenerConfig `json:"listeners,omitempty"`
	// CaptureStore configures the shared SQLite store that persists completed
	// MITM exchanges.
	CaptureStore MITMCaptureStoreConfig `json:"captureStore,omitzero" toml:"capture_store,omitempty"`
	CA           MITMCAConfig           `json:"ca,omitzero" toml:"ca,omitempty"`
}

// MITMListenerConfig configures one MITM listen endpoint. ID is the coarse
// client label that tags every captured exchange the listener serves (for
// example "cursor", "claude-code", "codex-cli", "claude-app", "codex-app"). It
// is populated from the [mitm.listeners.<id>] table key at load time and is not
// itself a TOML field.
type MITMListenerConfig struct {
	ID   string `json:"id,omitempty"`
	Host string `json:"host,omitempty" toml:"host,omitempty"`
	Port int    `json:"port,omitempty" toml:"port,omitempty"`
}

// MITMCaptureStoreConfig configures the shared SQLite capture store that
// persists completed MITM exchanges. DBPath defaults to <capture_dir>/capture.db;
// the remaining fields default inside the capture package when zero.
type MITMCaptureStoreConfig struct {
	DBPath            string   `json:"dbPath,omitempty" toml:"db_path,omitempty"`
	MaxBodyBytes      int      `json:"maxBodyBytes,omitempty" toml:"max_body_bytes,omitempty"`
	RetentionMaxAge   Duration `json:"retentionMaxAge,omitempty" toml:"retention_max_age,omitempty"`
	RetentionMaxBytes int64    `json:"retentionMaxBytes,omitempty" toml:"retention_max_bytes,omitempty"`
	RetentionInterval Duration `json:"retentionInterval,omitempty" toml:"retention_interval,omitempty"`
}

// MITMCAConfig configures on-disk persistence of the MITM CA.
type MITMCAConfig struct {
	CertPath string `json:"certPath,omitempty" toml:"cert_path,omitempty"`
	KeyPath  string `json:"keyPath,omitempty"  toml:"key_path,omitempty"`
}

// MITMCapture configures rotation of the drift-capture transcript that feeds
// baseline learning. The full request/response exchange store is SQLite and is
// configured under MITMCaptureStoreConfig.
type MITMCapture struct {
	Rotation LoggingRotation `json:"rotation,omitzero" toml:"rotation,omitempty"`
}

// MITMProviderSet is the configured set of provider families routed through
// the capture proxy. The special value "all" enables every provider family.
type MITMProviderSet []string

// MITMProvider is the bounded set of routed upstream provider families a
// capture rule may match. The match input is the routed provider's
// [providerid.Provider] label (see captureRuleMatches), so the valid values
// are exactly the labels [providerid.Parse] accepts. Empty means the rule
// does not constrain by provider.
type MITMProvider string

// IsValid reports whether the provider is empty (unconstrained) or a label
// that [providerid.Parse] resolves to a real provider family.
func (p MITMProvider) IsValid() bool {
	if p == "" {
		return true
	}
	_, ok := providerid.Parse(string(p))
	return ok
}

// MITMHTTPMethod is the bounded set of HTTP request methods a capture rule
// may match. Empty means the rule does not constrain by method. Values are
// normalized to upper case at load.
type MITMHTTPMethod string

const (
	// MITMMethodGet matches HTTP GET requests.
	MITMMethodGet MITMHTTPMethod = "GET"
	// MITMMethodHead matches HTTP HEAD requests.
	MITMMethodHead MITMHTTPMethod = "HEAD"
	// MITMMethodPost matches HTTP POST requests.
	MITMMethodPost MITMHTTPMethod = "POST"
	// MITMMethodPut matches HTTP PUT requests.
	MITMMethodPut MITMHTTPMethod = "PUT"
	// MITMMethodPatch matches HTTP PATCH requests.
	MITMMethodPatch MITMHTTPMethod = "PATCH"
	// MITMMethodDelete matches HTTP DELETE requests.
	MITMMethodDelete MITMHTTPMethod = "DELETE"
	// MITMMethodConnect matches HTTP CONNECT requests.
	MITMMethodConnect MITMHTTPMethod = "CONNECT"
	// MITMMethodOptions matches HTTP OPTIONS requests.
	MITMMethodOptions MITMHTTPMethod = "OPTIONS"
	// MITMMethodTrace matches HTTP TRACE requests.
	MITMMethodTrace MITMHTTPMethod = "TRACE"
)

// IsValid reports whether the method is empty (unconstrained) or one of the
// known HTTP methods.
func (m MITMHTTPMethod) IsValid() bool {
	switch m {
	case "",
		MITMMethodGet,
		MITMMethodHead,
		MITMMethodPost,
		MITMMethodPut,
		MITMMethodPatch,
		MITMMethodDelete,
		MITMMethodConnect,
		MITMMethodOptions,
		MITMMethodTrace:
		return true
	default:
		return false
	}
}

// MITMCaptureRouteRule classifies one captured MITM request into a concern.
type MITMCaptureRouteRule struct {
	Concern             string         `json:"concern" toml:"concern"`
	Provider            MITMProvider   `json:"provider,omitempty" toml:"provider,omitempty"`
	Host                string         `json:"host,omitempty" toml:"host,omitempty"`
	Method              MITMHTTPMethod `json:"method,omitempty" toml:"method,omitempty"`
	PathExact           string         `json:"pathExact,omitempty" toml:"path_exact,omitempty"`
	PathPrefix          string         `json:"pathPrefix,omitempty" toml:"path_prefix,omitempty"`
	PathContains        string         `json:"pathContains,omitempty" toml:"path_contains,omitempty"`
	ContentTypeContains string         `json:"contentTypeContains,omitempty" toml:"content_type_contains,omitempty"`
}

// MITMDriftConfig configures daemon-owned baseline refresh and drift reporting.
type MITMDriftConfig struct {
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	// Interval is the periodic drift-check tick. It accepts a Go duration
	// string such as "5m" via Duration's TextUnmarshaler; a raw
	// time.Duration field would reject the string form go-toml v2 sees.
	Interval  Duration                        `json:"interval,omitempty" toml:"interval,omitempty"`
	Upstreams map[string]MITMDriftUpstreamCfg `json:"upstreams,omitempty" toml:"upstreams,omitempty"`
}

// MITMDriftUpstreamCfg configures one upstream's daemon-owned baseline refresh.
// The baseline corpus, the current baseline, and the difference matrix all live
// in the shared SQLite capture store, so the only per-upstream knobs are the
// keep filters that scope which captured caller flavor seeds the baseline.
type MITMDriftUpstreamCfg struct {
	IncludeUA       []string `json:"includeUa,omitempty" toml:"include_ua,omitempty"`
	ExcludeUA       []string `json:"excludeUa,omitempty" toml:"exclude_ua,omitempty"`
	RequireBodyKeys []string `json:"requireBodyKeys,omitempty" toml:"require_body_keys,omitempty"`
	ForbidBodyKeys  []string `json:"forbidBodyKeys,omitempty" toml:"forbid_body_keys,omitempty"`
}

// EnabledFor reports whether the configured MITM provider set includes provider.
func (m MITMConfig) EnabledFor(provider string) bool {
	normalizedProvider := normalizeMITMProviderName(provider)
	if !isValidMITMProviderName(normalizedProvider) {
		return false
	}
	providers, err := parseMITMProviders(m.Providers)
	if err != nil {
		return false
	}
	if len(providers) == 1 && providers[0] == mitmProviderAll {
		return true
	}
	return slices.Contains(providers, normalizedProvider)
}
