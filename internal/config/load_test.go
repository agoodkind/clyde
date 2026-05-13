package config_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
)

var _ = Describe("NewConfig", func() {
	It("should create config with defaults", func() {
		cfg := config.NewConfig()
		Expect(cfg).NotTo(BeNil())
		Expect(cfg.Profiles).To(BeEmpty())
	})
})

var _ = Describe("LoadGlobalOrDefault", func() {
	var origXDG string

	BeforeEach(func() {
		origXDG = os.Getenv("XDG_CONFIG_HOME")
	})

	AfterEach(func() {
		if origXDG == "" {
			_ = os.Unsetenv("XDG_CONFIG_HOME")
		} else {
			_ = os.Setenv("XDG_CONFIG_HOME", origXDG)
		}
	})

	It("returns empty config when file is absent", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Expect(cfg.Profiles).To(BeEmpty())
		Expect(cfg.Logging.Level).To(Equal("info"))
		Expect(cfg.Logging.Rotation.Enabled).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Enabled).To(BeTrue())
		Expect(cfg.Logging.Rotation.MaxSizeMB).To(Equal(64))
		Expect(cfg.Logging.Rotation.MaxBackups).To(Equal(192))
		Expect(cfg.Logging.Rotation.MaxAgeDays).To(Equal(14))
		Expect(cfg.Logging.Rotation.Compress).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Compress).To(BeTrue())
		Expect(cfg.Logging.Body.Mode).To(Equal("summary"))
		Expect(cfg.Logging.Body.MaxKB).To(Equal(32))
		Expect(cfg.Defaults.CompactCounter).To(Equal("api"))
	})

	It("loads compact counter source", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[defaults]\ncompact_counter = \"probe\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Defaults.CompactCounter).To(Equal("probe"))
	})

	It("loads profiles correctly when config.toml is present", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[profiles.quick]\nmodel = \"haiku\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Profiles["quick"].Model).To(Equal("haiku"))
	})

	It("loads MITM provider sets with cursor", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[mitm]\nproviders = [\"cursor\", \"codex\"]\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Providers).To(Equal(config.MITMProviderSet{"codex", "cursor"}))
		Expect(cfg.MITM.EnabledFor("cursor")).To(BeTrue())
		Expect(cfg.MITM.EnabledFor("codex")).To(BeTrue())
		Expect(cfg.MITM.EnabledFor("claude")).To(BeFalse())
	})

	It("loads the MITM all provider shorthand", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[mitm]\nproviders = [\"all\"]\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.Providers).To(Equal(config.MITMProviderSet{"all"}))
		Expect(cfg.MITM.EnabledFor("cursor")).To(BeTrue())
		Expect(cfg.MITM.EnabledFor("claude")).To(BeTrue())
		Expect(cfg.MITM.EnabledFor("codex")).To(BeTrue())
	})

	It("loads default MITM capture route rules", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[mitm]\nproviders = [\"cursor\"]\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.CaptureRules).NotTo(BeEmpty())
		Expect(cfg.MITM.CaptureRules[0].Concern).To(Equal("cursor.bidi"))
		Expect(cfg.MITM.CaptureRules[len(cfg.MITM.CaptureRules)-1].Concern).To(Equal("unknown"))
	})

	It("loads custom MITM capture route rules", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		toml := `[mitm]
providers = ["cursor"]

[[mitm.capture_rules]]
concern = "cursor.custom"
provider = "cursor"
host = "API2.Cursor.SH."
method = "post"
path_prefix = "/custom"
content_type_contains = "PROTOBUF"

[[mitm.capture_rules]]
concern = "unknown"
`
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(toml), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.CaptureRules).To(HaveLen(2))
		Expect(cfg.MITM.CaptureRules[0].Concern).To(Equal("cursor.custom"))
		Expect(cfg.MITM.CaptureRules[0].Host).To(Equal("api2.cursor.sh"))
		Expect(cfg.MITM.CaptureRules[0].Method).To(Equal("POST"))
		Expect(cfg.MITM.CaptureRules[0].ContentTypeContains).To(Equal("protobuf"))
	})

	It("loads openai_compat_passthrough upstream", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.openai_compat_passthrough]\nbase_url = \"http://[::1]:1234/v1\"\napi_key_env = \"OPENAI_API_KEY\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Adapter.OpenAICompatPassthrough.BaseURL).To(Equal("http://[::1]:1234/v1"))
		Expect(cfg.Adapter.OpenAICompatPassthrough.APIKeyEnv).To(Equal("OPENAI_API_KEY"))
	})

	It("loads passthrough override upstreams", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.passthrough_overrides.local]\nbase_url = \"http://localhost:1234/v1\"\napi_key_env = \"LOCAL_API_KEY\"\nmodel = \"local-model\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Adapter.PassthroughOverrides["local"].BaseURL).To(Equal("http://localhost:1234/v1"))
		Expect(cfg.Adapter.PassthroughOverrides["local"].APIKeyEnv).To(Equal("LOCAL_API_KEY"))
		Expect(cfg.Adapter.PassthroughOverrides["local"].Model).To(Equal("local-model"))
	})

	It("loads and normalizes codex reasoning summary", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.codex]\nreasoning_summary = \"Detailed\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Adapter.Codex.ReasoningSummary).To(Equal("detailed"))
	})

	It("rejects invalid codex reasoning summary", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.codex]\nreasoning_summary = \"verbose\"\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("adapter.codex.reasoning_summary"))
	})

	It("loads adapter instructions_file contents relative to config.toml", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(filepath.Join(globalDir, "prompts"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "prompts", "family.md"), []byte("family prompt\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "prompts", "model.md"), []byte("model prompt"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "prompts", "codex.md"), []byte("codex prompt"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.models.custom]\nmodel = \"claude-sonnet\"\ninstructions_file = \"prompts/model.md\"\n\n[adapter.families.family]\nmodel = \"claude-family\"\nefforts = [\"medium\"]\nthinking_modes = [\"default\"]\nmax_output_tokens = 1024\nsupports_tools = true\nsupports_vision = false\ninstructions_file = \"prompts/family.md\"\ncontexts = [{ tokens = 200000 }]\n\n[adapter.codex]\nmodels = [\n  { alias_prefix = \"gpt-test\", model = \"gpt-test\", efforts = [\"medium\"], max_output_tokens = 1024, instructions_file = \"prompts/codex.md\", contexts = [{ tokens = 200000 }] }\n]\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Adapter.Models["custom"].Instructions).To(Equal("model prompt"))
		Expect(cfg.Adapter.Families["family"].Instructions).To(Equal("family prompt\n"))
		Expect(cfg.Adapter.Codex.Models).To(HaveLen(1))
		Expect(cfg.Adapter.Codex.Models[0].Instructions).To(Equal("codex prompt"))
	})

	It("rejects missing adapter instructions_file", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.models.custom]\nmodel = \"claude-sonnet\"\ninstructions_file = \"missing.md\"\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("adapter.models.custom.instructions_file"))
		Expect(err.Error()).To(ContainSubstring("missing.md"))
	})

	It("rejects empty adapter instructions_file", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "empty.md"), nil, 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.models.custom]\nmodel = \"claude-sonnet\"\ninstructions_file = \"empty.md\"\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("adapter.models.custom.instructions_file"))
		Expect(err.Error()).To(ContainSubstring("file is empty"))
	})

	It("ignores empty adapter instructions_file fields", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.models.custom]\nmodel = \"claude-sonnet\"\ninstructions_file = \"   \"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Adapter.Models["custom"].Instructions).To(Equal(""))
	})

	It("ignores legacy global config.json", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.json"), []byte(`{"profiles":{"quick":{"model":"haiku"}}}`), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Profiles).To(BeEmpty())
	})

	It("loads adapter notice usage thresholds", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.notices.usage]\nthresholds_used_percent = [95, 75, 75]\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Adapter.Notices.UsageThresholdsUsedPercentOrDefault()).To(Equal([]float64{75, 95}))
	})

	It("loads adapter notice usage repeat policy", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		contents := "[adapter.notices.usage.repeat]\nmode = \"time_cooldown\"\ncooldown = \"12h\"\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(contents), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		policy := cfg.Adapter.Notices.UsageRepeatPolicyOrDefault()
		Expect(policy.Mode).To(Equal(config.AdapterNoticeRepeatTimeCooldown))
		Expect(policy.Cooldown).To(Equal("12h"))
		Expect(policy.CooldownDuration).To(Equal(12 * time.Hour))
	})

	It("rejects invalid adapter notice usage repeat policy", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		contents := "[adapter.notices.usage.repeat]\nmode = \"turn_cooldown\"\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(contents), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("adapter.notices.usage.repeat.cooldown_turns"))
	})

	It("rejects invalid adapter notice usage thresholds", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[adapter.notices.usage]\nthresholds_used_percent = [0, 75]\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("adapter.notices.usage.thresholds_used_percent"))
	})

	It("applies logging defaults when logging stanza is omitted", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[profiles.quick]\nmodel = \"haiku\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Rotation.Enabled).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Enabled).To(BeTrue())
		Expect(cfg.Logging.Rotation.MaxSizeMB).To(Equal(64))
		Expect(cfg.Logging.Rotation.MaxBackups).To(Equal(192))
		Expect(cfg.Logging.Rotation.MaxAgeDays).To(Equal(14))
		Expect(cfg.Logging.Rotation.Compress).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Compress).To(BeTrue())
		Expect(cfg.Logging.Body.Mode).To(Equal("summary"))
		Expect(cfg.Logging.Body.MaxKB).To(Equal(32))
	})

	It("rejects invalid logging.body.mode", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.body]\nmode = \"bogus\"\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.body.mode must be one of summary|whitelist|raw|off"))
	})

	It("accepts logging.rotation.enabled = false", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.rotation]\nenabled = false\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Rotation.Enabled).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Enabled).To(BeFalse())
	})

	It("loads logging.paths for daemon and tui", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.paths]\ndaemon = \"/tmp/clyde-daemon.jsonl\"\ntui = \"/tmp/clyde-tui.jsonl\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Paths.Daemon).To(Equal("/tmp/clyde-daemon.jsonl"))
		Expect(cfg.Logging.Paths.TUI).To(Equal("/tmp/clyde-tui.jsonl"))
	})

	It("rejects negative logging.rotation.max_backups", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.rotation]\nmax_backups = -1\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.rotation.max_backups must be >= 0"))
	})

	It("rejects invalid logging.body.max_kb", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.body]\nmax_kb = 300\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.body.max_kb must be between 1 and 256"))
	})

	It("loads logging sink and concern overrides", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		configData := `[logging.sinks.daemon]
level = "debug"
detail = "verbose"

[logging.sinks.daemon.retention]
max_age_days = 7
max_backups = 3
max_total_mb = 256
compress = false
cleanup_mode = "audit_only"

[logging.concerns."adapter.http.raw"]
level = "warn"
detail = "raw"
sink = "concerns"
`
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configData), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Sinks[config.LoggingSinkDaemon].Level).To(Equal("debug"))
		Expect(cfg.Logging.Sinks[config.LoggingSinkDaemon].Detail).To(Equal("verbose"))
		Expect(cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.MaxAgeDays).NotTo(BeNil())
		Expect(*cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.MaxAgeDays).To(Equal(7))
		Expect(cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.MaxBackups).NotTo(BeNil())
		Expect(*cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.MaxBackups).To(Equal(3))
		Expect(cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.MaxTotalMB).NotTo(BeNil())
		Expect(*cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.MaxTotalMB).To(Equal(256))
		Expect(cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.Compress).NotTo(BeNil())
		Expect(*cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.Compress).To(BeFalse())
		Expect(cfg.Logging.Sinks[config.LoggingSinkDaemon].Retention.CleanupMode).To(Equal("audit_only"))
		Expect(cfg.Logging.Concerns["adapter.http.raw"].Level).To(Equal("warn"))
		Expect(cfg.Logging.Concerns["adapter.http.raw"].Detail).To(Equal("raw"))
		Expect(cfg.Logging.Concerns["adapter.http.raw"].Sink).To(Equal("concerns"))
	})

	It("rejects unknown logging sink names", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.sinks.nope]\nlevel = \"debug\"\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.sinks.nope is not a known sink"))
	})

	It("rejects invalid logging retention controls", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.sinks.daemon.retention]\nmax_total_mb = -1\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.sinks.daemon.retention.max_total_mb must be >= 0"))
	})

	It("rejects invalid MITM capture rotation controls", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		configData := "[mitm.capture.rotation]\nmax_size_mb = -1\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configData), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mitm.capture.rotation.max_size_mb must be >= 0"))
	})
})
