package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/config"
)

var _ = Describe("NewConfig", func() {
	It("should create config with defaults", func() {
		cfg := config.NewConfig()
		Expect(cfg).NotTo(BeNil())
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
		Expect(cfg.Logging.Level).To(Equal("info"))
		Expect(cfg.Logging.Rotation.Enabled).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Enabled).To(BeTrue())
		Expect(cfg.Logging.Rotation.MaxSizeMB).To(Equal(64))
		Expect(cfg.Logging.Rotation.MaxBackups).To(Equal(192))
		Expect(cfg.Logging.Rotation.MaxAgeDays).To(Equal(14))
		Expect(cfg.Logging.Rotation.Compress).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Compress).To(BeTrue())
		Expect(cfg.Conversation.Semantic.Enabled).To(BeFalse())
		Expect(cfg.Conversation.Semantic.CollectionID).To(Equal("clyde-conversations"))
	})

	It("loads conversation semantic sync config", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		configText := "[conversation.semantic]\nenabled = true\nsocket_path = \"/tmp/lm-semantic.sock\"\ncollection_id = \"custom-conversations\"\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configText), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Conversation.Semantic.Enabled).To(BeTrue())
		Expect(cfg.Conversation.Semantic.SocketPath).To(Equal("/tmp/lm-semantic.sock"))
		Expect(cfg.Conversation.Semantic.CollectionID).To(Equal("custom-conversations"))
	})

	It("defaults blank conversation semantic collection id", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		configText := "[conversation.semantic]\nenabled = true\ncollection_id = \"   \"\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configText), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Conversation.Semantic.Enabled).To(BeTrue())
		Expect(cfg.Conversation.Semantic.CollectionID).To(Equal("clyde-conversations"))
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

	It("normalizes legacy MITM always-on capture dir", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		captureDir := filepath.Join(tmpDir, "state", "mitm", "always-on")
		configText := "[mitm]\ncapture_dir = " + strconv.Quote(captureDir) + "\n"
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configText), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.CaptureDir).To(Equal(filepath.Dir(captureDir)))
	})

	It("defaults absent MITM capture dir to the state MITM directory", func() {
		tmpDir := GinkgoT().TempDir()
		stateDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
		GinkgoT().Setenv("XDG_STATE_HOME", stateDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[mitm]\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.CaptureDir).To(Equal(filepath.Join(stateDir, "clyde", "mitm")))
	})

	It("home-expands and cleans MITM capture dir before legacy always-on normalization", func() {
		tmpDir := GinkgoT().TempDir()
		home := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
		GinkgoT().Setenv("HOME", home)

		globalDir := filepath.Join(tmpDir, "clyde")
		configText := "[mitm]\ncapture_dir = \"~/x/always-on\"\n"
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configText), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MITM.CaptureDir).To(Equal(filepath.Join(home, "x")))
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
		Expect(cfg.MITM.CaptureRules[0].Provider).To(Equal(config.MITMProvider("cursor")))
		Expect(cfg.MITM.CaptureRules[0].Host).To(Equal("api2.cursor.sh"))
		Expect(cfg.MITM.CaptureRules[0].Method).To(Equal(config.MITMMethodPost))
		Expect(cfg.MITM.CaptureRules[0].ContentTypeContains).To(Equal("protobuf"))
	})

	It("rejects an unknown MITM capture rule provider", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		toml := "[[mitm.capture_rules]]\nconcern = \"weird\"\nprovider = \"definitely-not-a-provider\"\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(toml), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("provider"))
		Expect(err.Error()).To(ContainSubstring("definitely-not-a-provider"))
	})

	It("rejects an unknown MITM capture rule method", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		toml := "[[mitm.capture_rules]]\nconcern = \"weird\"\nmethod = \"FETCH\"\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(toml), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("method"))
		Expect(err.Error()).To(ContainSubstring("FETCH"))
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
		Expect(cfg).NotTo(BeNil())
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
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(""), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Rotation.Enabled).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Enabled).To(BeTrue())
		Expect(cfg.Logging.Rotation.MaxSizeMB).To(Equal(64))
		Expect(cfg.Logging.Rotation.MaxBackups).To(Equal(192))
		Expect(cfg.Logging.Rotation.MaxAgeDays).To(Equal(14))
		Expect(cfg.Logging.Rotation.Compress).NotTo(BeNil())
		Expect(*cfg.Logging.Rotation.Compress).To(BeTrue())
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

	It("loads logging.paths for daemon and cli", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.paths]\ndaemon = \"/tmp/clyde-daemon.jsonl\"\ncli = \"/tmp/clyde-cli.jsonl\"\n"), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Paths.Daemon).To(Equal("/tmp/clyde-daemon.jsonl"))
		Expect(cfg.Logging.Paths.CLI).To(Equal("/tmp/clyde-cli.jsonl"))
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
	It("loads logging sink and concern controls", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		configData := `[logging.sinks]
enabled = ["daemon", "concerns"]

[logging.concerns."adapter.chat.dispatch"]
level = "warn"
detail = "verbose"
sink = "concerns"
`
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configData), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Sinks.Enabled).To(Equal([]string{config.LoggingSinkDaemon, config.LoggingSinkConcerns}))
		Expect(cfg.Logging.Concerns["adapter.chat.dispatch"].Level).To(Equal("warn"))
		Expect(cfg.Logging.Concerns["adapter.chat.dispatch"].Detail).To(Equal("verbose"))
		Expect(cfg.Logging.Concerns["adapter.chat.dispatch"].Sink).To(Equal("concerns"))
	})

	It("rejects unknown logging sink names", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.sinks]\nenabled = [\"nope\"]\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.sinks.enabled contains unknown sink"))
	})

	It("loads per-sink config tables alongside the flat enabled list", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		configData := `[logging.sinks]
enabled = ["daemon", "concerns"]

[logging.sinks.audit]
enabled = false
level = "warn"

[logging.sinks.anthropic_sidecar]
enabled = true
[logging.sinks.anthropic_sidecar.rotation]
max_size_mb = 32
`
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configData), 0o644)).To(Succeed())

		cfg, err := config.LoadGlobalOrDefault()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Sinks.Enabled).To(Equal([]string{config.LoggingSinkDaemon, config.LoggingSinkConcerns}))

		auditOverride, ok := cfg.Logging.Sinks.Override(config.LoggingSinkAudit)
		Expect(ok).To(BeTrue())
		Expect(auditOverride.Enabled).NotTo(BeNil())
		Expect(*auditOverride.Enabled).To(BeFalse())
		Expect(auditOverride.Level).To(Equal("warn"))

		anthOverride, ok := cfg.Logging.Sinks.Override(config.LoggingSinkAnthropicSidecar)
		Expect(ok).To(BeTrue())
		Expect(anthOverride.Enabled).NotTo(BeNil())
		Expect(*anthOverride.Enabled).To(BeTrue())
		Expect(anthOverride.Rotation.MaxSizeMB).To(Equal(32))
	})

	It("rejects an invalid per-sink table level", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		configData := "[logging.sinks.audit]\nlevel = \"nope\"\n"
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configData), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.sinks.audit.level must be one of debug|info|warn|error"))
	})

	It("rejects invalid logging cleanup controls", func() {
		tmpDir := GinkgoT().TempDir()
		_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

		globalDir := filepath.Join(tmpDir, "clyde")
		Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("[logging.cleanup]\nmax_total_mb = -1\n"), 0o644)).To(Succeed())

		_, err := config.LoadGlobalOrDefault()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("logging.cleanup.max_total_mb must be >= 0"))
	})

	DescribeTable("accepts removed logging config surfaces as no-op warnings",
		func(configData string) {
			tmpDir := GinkgoT().TempDir()
			_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)

			globalDir := filepath.Join(tmpDir, "clyde")
			Expect(os.MkdirAll(globalDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(configData), 0o644)).To(Succeed())

			cfg, err := config.LoadGlobalOrDefault()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg).NotTo(BeNil())
		},
		Entry("logging.body", "[logging.body]\nmode = \"summary\"\n"),
		Entry("mitm.body_mode", "[mitm]\nbody_mode = \"summary\"\n"),
		Entry("logging.cleanup.audit_only", "[logging.cleanup]\naudit_only = true\n"),
		Entry("logging.cleanup.cleanup_mode", "[logging.cleanup]\ncleanup_mode = \"audit\"\n"),
	)
})
