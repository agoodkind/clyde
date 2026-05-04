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
})
