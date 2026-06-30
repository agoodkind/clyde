package config

import "testing"

// minimalConfig returns a valid baseline Config with the adapter enabled and a
// single MITM listener, used as the unchanged side of classifier cases.
func minimalConfig() Config {
	cfg := *NewConfig()
	cfg.Adapter.Enabled = true
	cfg.Adapter.Host = "[::1]"
	cfg.Adapter.Port = 11434
	cfg.MITM.EnabledDefault = true
	cfg.MITM.Listeners = map[string]MITMListenerConfig{
		"cli.test": {Host: "localhost", Port: 48723},
	}
	cfg.MITM.CaptureStore.DBPath = "/state/mitm/capture.db"
	return cfg
}

func TestClassifyConfigChange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Config)
		want   Route
	}{
		{"no change", func(*Config) {}, RouteReload},
		{"adapter disabled", func(c *Config) { c.Adapter.Enabled = false }, RouteRestartRequired},
		{"mitm disabled", func(c *Config) { c.MITM.EnabledDefault = false }, RouteRestartRequired},
		{"adapter port", func(c *Config) { c.Adapter.Port = 21434 }, RouteRebind},
		{"cursor ingress port", func(c *Config) { c.Adapter.CursorIngressPort = 21435 }, RouteRebind},
		{"pprof addr", func(c *Config) { c.Debug.PProfAddr = "[::1]:0" }, RouteRebind},
		{"mitm listener port", func(c *Config) {
			c.MITM.Listeners["cli.test"] = MITMListenerConfig{Host: "localhost", Port: 58723}
		}, RouteRebind},
		{"capture db path", func(c *Config) { c.MITM.CaptureStore.DBPath = "/tmp/x.db" }, RouteReload},
		{"ca cert path", func(c *Config) { c.MITM.CA.CertPath = "/tmp/ca.crt" }, RouteReload},
		{"model alias added", func(c *Config) {
			c.Adapter.Models = map[string]AdapterModel{"x": {Backend: "claude"}}
		}, RouteHotApply},
		{"default model", func(c *Config) { c.Adapter.DefaultModel = "clyde-haiku" }, RouteHotApply},
		{"pricing added", func(c *Config) {
			c.Adapter.Pricing = map[string]AdapterModelPricing{"m": {InputPerMTok: 1}}
		}, RouteHotApply},
		{"mitm providers", func(c *Config) { c.MITM.Providers = []string{"anthropic"} }, RouteHotApply},
		{"non-hot adapter field (require token)", func(c *Config) { c.Adapter.RequireToken = "secret" }, RouteReload},
		{"non-hot adapter field (max concurrent)", func(c *Config) { c.Adapter.MaxConcurrent = 8 }, RouteReload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := minimalConfig()
			next := minimalConfig()
			tc.mutate(&next)
			if got := ClassifyConfigChange(base, next); got != tc.want {
				t.Errorf("ClassifyConfigChange(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
