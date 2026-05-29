package config

import (
	"slices"

	"goodkind.io/clyde/internal/providerid"
)

// MITMConfig configures the local capture proxy and its persistence.
type MITMConfig struct {
	EnabledDefault    bool                   `json:"enabledDefault,omitempty" toml:"enabled_default,omitempty"`
	Providers         MITMProviderSet        `json:"providers,omitempty" toml:"providers,omitempty"`
	RawCaptureEnabled bool                   `json:"-" toml:"-"`
	CaptureDir        string                 `json:"captureDir,omitempty" toml:"capture_dir,omitempty"`
	Capture           MITMCapture            `json:"capture,omitzero" toml:"capture,omitempty"`
	CaptureRules      []MITMCaptureRouteRule `json:"captureRules,omitempty" toml:"capture_rules,omitempty"`
	// Hooks fire in TOML declaration order: matchHookRule returns the first
	// [[mitm.hook]] whose host/method/path match, so the order [[mitm.hook]]
	// blocks appear in the config file is the order they are evaluated and
	// the first match wins. Config load preserves this slice order.
	Hooks  []MITMHookRule   `json:"hooks,omitempty" toml:"hook,omitempty"`
	Drift  MITMDriftConfig  `json:"drift,omitzero" toml:"drift,omitempty"`
	Listen MITMListenConfig `json:"listen,omitzero" toml:"listen,omitempty"`
	CA     MITMCAConfig     `json:"ca,omitzero" toml:"ca,omitempty"`
}

// MITMListenConfig configures the stable listen address of the in-process
// MITM proxy.
type MITMListenConfig struct {
	Host string `json:"host,omitempty" toml:"host,omitempty"`
	Port int    `json:"port,omitempty" toml:"port,omitempty"`
}

// MITMCAConfig configures on-disk persistence of the MITM CA.
type MITMCAConfig struct {
	CertPath string `json:"certPath,omitempty" toml:"cert_path,omitempty"`
	KeyPath  string `json:"keyPath,omitempty"  toml:"key_path,omitempty"`
}

// MITMCapture configures MITM capture index file policy.
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

// MITMHookMode names one of the three supported hook execution modes
// for [[mitm.hook]] rules.
type MITMHookMode string

const (
	// MITMHookModeSynthesize skips the upstream call entirely.
	MITMHookModeSynthesize MITMHookMode = "synthesize"
	// MITMHookModeTransformRequest invokes the hook before the upstream call.
	MITMHookModeTransformRequest MITMHookMode = "transform_request"
	// MITMHookModeTransformResponse invokes the hook after the upstream call.
	MITMHookModeTransformResponse MITMHookMode = "transform_response"
)

// IsValid reports whether the mode is one of the three known values.
func (m MITMHookMode) IsValid() bool {
	switch m {
	case MITMHookModeSynthesize, MITMHookModeTransformRequest, MITMHookModeTransformResponse:
		return true
	}
	return false
}

// MITMHookRule declares an external subprocess that Clyde forks for matching
// MITM-decrypted requests.
type MITMHookRule struct {
	Name           string       `json:"name" toml:"name"`
	MatchHost      string       `json:"matchHost,omitempty" toml:"match_host,omitempty"`
	MatchPathRegex string       `json:"matchPathRegex,omitempty" toml:"match_path_regex,omitempty"`
	MatchMethod    string       `json:"matchMethod,omitempty" toml:"match_method,omitempty"`
	Mode           MITMHookMode `json:"mode,omitempty" toml:"mode,omitempty"`
	Command        string       `json:"command" toml:"command"`
	Args           []string     `json:"args,omitempty" toml:"args,omitempty"`
	Timeout        Duration     `json:"timeout,omitempty" toml:"timeout,omitempty"`
}

// MITMDriftConfig configures daemon-owned baseline refresh and drift reporting.
type MITMDriftConfig struct {
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	// Interval is the periodic drift-check tick. It accepts a Go duration
	// string such as "5m" via Duration's TextUnmarshaler; a raw
	// time.Duration field would reject the string form go-toml v2 sees.
	Interval    Duration                        `json:"interval,omitempty" toml:"interval,omitempty"`
	DriftLogDir string                          `json:"driftLogDir,omitempty" toml:"drift_log_dir,omitempty"`
	CaptureRoot string                          `json:"captureRoot,omitempty" toml:"capture_root,omitempty"`
	CACertPath  string                          `json:"caCertPath,omitempty" toml:"ca_cert_path,omitempty"`
	Upstreams   map[string]MITMDriftUpstreamCfg `json:"upstreams,omitempty" toml:"upstreams,omitempty"`
}

// MITMDriftUpstreamCfg configures one upstream's daemon-owned baseline refresh.
type MITMDriftUpstreamCfg struct {
	Reference       string   `json:"reference" toml:"reference"`
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
