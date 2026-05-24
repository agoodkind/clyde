package logpolicy_test

import (
	"log/slog"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/slogger"
)

func TestResolveUsesDefaults(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	policies, err := logpolicy.Resolve(*cfg)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	daemonPolicy := policies.Sinks[logpolicy.SinkDaemon]
	if !daemonPolicy.Enabled {
		t.Fatalf("daemon sink should default to enabled")
	}
	if daemonPolicy.Level != logpolicy.LevelInfo {
		t.Fatalf("daemon level = %q, want %q", daemonPolicy.Level, logpolicy.LevelInfo)
	}
	if daemonPolicy.Rotation.MaxSizeMB != 64 {
		t.Fatalf("daemon rotation max size = %d, want 64", daemonPolicy.Rotation.MaxSizeMB)
	}
	if !daemonPolicy.Cleanup.Enabled {
		t.Fatalf("daemon cleanup should default to enabled")
	}
	if _, ok := policies.Concerns[slogger.ConcernAdapterChatDispatch]; !ok {
		t.Fatalf("policy set should include registered concern %q", slogger.ConcernAdapterChatDispatch)
	}
}

func TestResolveAppliesEnabledSinkSetAndConcernOverride(t *testing.T) {
	maxTotalMB := 512
	maxSizeMB := 7
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Level = "info"
	cfg.Logging.Sinks = config.LoggingSinks{
		Enabled: []string{config.LoggingSinkDaemon, config.LoggingSinkConcerns},
	}
	cfg.Logging.Cleanup.MaxTotalMB = &maxTotalMB
	cfg.Logging.Concerns = config.LoggingConcerns{
		slogger.ConcernAdapterChatDispatch: {
			Level:  "debug",
			Detail: "verbose",
			Rotation: config.LoggingRotation{
				MaxSizeMB: maxSizeMB,
			},
		},
	}

	policies, err := logpolicy.Resolve(*cfg)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if !policies.Sinks[logpolicy.SinkDaemon].Enabled {
		t.Fatalf("daemon sink should be enabled by configured sink set")
	}
	if policies.Sinks[logpolicy.SinkAudit].Enabled {
		t.Fatalf("audit sink should be disabled when omitted from configured sink set")
	}
	sinkPolicy := policies.Sinks[logpolicy.SinkConcerns]
	if !sinkPolicy.Enabled {
		t.Fatalf("concerns sink should be enabled by configured sink set")
	}
	if sinkPolicy.Cleanup.MaxTotalMB == nil || *sinkPolicy.Cleanup.MaxTotalMB != 512 {
		t.Fatalf("concerns sink max total MB = %v, want 512", sinkPolicy.Cleanup.MaxTotalMB)
	}

	concernPolicy := policies.Concerns[slogger.ConcernAdapterChatDispatch]
	if !concernPolicy.Enabled {
		t.Fatalf("concern should inherit enabled sink state")
	}
	if concernPolicy.Level != logpolicy.LevelDebug {
		t.Fatalf("concern level = %q, want %q", concernPolicy.Level, logpolicy.LevelDebug)
	}
	if concernPolicy.Detail != logpolicy.DetailVerbose {
		t.Fatalf("concern detail = %q, want %q", concernPolicy.Detail, logpolicy.DetailVerbose)
	}
	if concernPolicy.Rotation.MaxSizeMB != maxSizeMB {
		t.Fatalf("concern rotation max size = %d, want %d", concernPolicy.Rotation.MaxSizeMB, maxSizeMB)
	}
	if concernPolicy.Cleanup.MaxTotalMB == nil || *concernPolicy.Cleanup.MaxTotalMB != 512 {
		t.Fatalf("concern cleanup max total MB = %v, want 512", concernPolicy.Cleanup.MaxTotalMB)
	}
}

func TestResolveSinksInheritGlobalRotation(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Rotation = config.LoggingRotation{
		MaxSizeMB:  11,
		MaxBackups: 22,
		MaxAgeDays: 33,
	}

	policies, err := logpolicy.Resolve(*cfg)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	for _, sinkName := range []logpolicy.SinkName{
		logpolicy.SinkAudit,
		logpolicy.SinkAnthropicSidecar,
		logpolicy.SinkInventory,
	} {
		sinkPolicy := policies.Sinks[sinkName]
		if !sinkPolicy.Enabled {
			t.Fatalf("%s sink should default to enabled", sinkName)
		}
		if sinkPolicy.Rotation.MaxSizeMB != 11 {
			t.Fatalf("%s rotation max size = %d, want 11", sinkName, sinkPolicy.Rotation.MaxSizeMB)
		}
		if sinkPolicy.Rotation.MaxBackups != 22 {
			t.Fatalf("%s rotation max backups = %d, want 22", sinkName, sinkPolicy.Rotation.MaxBackups)
		}
		if sinkPolicy.Rotation.MaxAgeDays != 33 {
			t.Fatalf("%s rotation max age days = %d, want 33", sinkName, sinkPolicy.Rotation.MaxAgeDays)
		}
	}
}

func TestResolveAppliesAdapterWireCaptureRotationDefaults(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	cfg.Adapter.WireCapture.Rotation = config.LoggingRotation{
		MaxSizeMB: 5,
	}

	policies, err := logpolicy.Resolve(*cfg)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	for _, concernName := range []string{
		slogger.ConcernAdapterProviderAnthWire,
		slogger.ConcernAdapterProviderCodexWire,
	} {
		concernPolicy := policies.Concerns[concernName]
		if concernPolicy.Rotation.MaxSizeMB != 5 {
			t.Fatalf("%s rotation max size = %d, want 5", concernName, concernPolicy.Rotation.MaxSizeMB)
		}
		if concernPolicy.Rotation.MaxBackups != 3 {
			t.Fatalf("%s rotation max backups = %d, want 3", concernName, concernPolicy.Rotation.MaxBackups)
		}
		if concernPolicy.Rotation.MaxAgeDays != 2 {
			t.Fatalf("%s rotation max age = %d, want 2", concernName, concernPolicy.Rotation.MaxAgeDays)
		}
	}
}

func TestResolveSloggerSetupConvertsPolicies(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Sinks = config.LoggingSinks{
		Enabled: []string{config.LoggingSinkConcerns, config.LoggingSinkInventory},
	}
	cfg.Logging.Concerns = config.LoggingConcerns{
		slogger.ConcernAdapterChatDispatch: {
			Level: "debug",
		},
	}

	policy, err := logpolicy.ResolveSloggerSetup(*cfg, slogger.ProcessRoleDaemon)
	if err != nil {
		t.Fatalf("ResolveSloggerSetup returned error: %v", err)
	}

	if policy.ProcessSink.Enabled {
		t.Fatalf("daemon process sink should be disabled when omitted from enabled sinks")
	}
	if !policy.AsyncCleanup {
		t.Fatalf("daemon setup should defer cleanup until after logger setup")
	}
	if !policy.InventoryPolicy.Enabled {
		t.Fatalf("inventory sink should be enabled by configured sink set")
	}
	concernPolicy := policy.ConcernPolicies[slogger.ConcernAdapterChatDispatch]
	if concernPolicy.Level == nil || *concernPolicy.Level != slog.LevelDebug {
		t.Fatalf("concern slog level = %v, want debug", concernPolicy.Level)
	}
}

func TestResolveSloggerSetupDisablesConcernFileForProcessSink(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Concerns = config.LoggingConcerns{
		slogger.ConcernAdapterChatDispatch: {
			Sink: config.LoggingSinkDaemon,
		},
	}

	policy, err := logpolicy.ResolveSloggerSetup(*cfg, slogger.ProcessRoleDaemon)
	if err != nil {
		t.Fatalf("ResolveSloggerSetup returned error: %v", err)
	}

	concernPolicy := policy.ConcernPolicies[slogger.ConcernAdapterChatDispatch]
	if concernPolicy.Enabled == nil || *concernPolicy.Enabled {
		t.Fatalf("process-sink concern should disable the concern file handler")
	}
}

func TestResolveRejectsUnknownConcern(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Concerns = config.LoggingConcerns{
		"adapter.nope": {},
	}

	_, err := logpolicy.Resolve(*cfg)
	if err == nil {
		t.Fatalf("Resolve returned nil error")
	}
	if !strings.Contains(err.Error(), "logging.concerns.adapter.nope is not a known concern") {
		t.Fatalf("Resolve error = %q", err.Error())
	}
}

func TestResolveRejectsInvalidCleanup(t *testing.T) {
	negativeMaxAgeDays := -1
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Cleanup.MaxAgeDays = &negativeMaxAgeDays

	_, err := logpolicy.Resolve(*cfg)
	if err == nil {
		t.Fatalf("Resolve returned nil error")
	}
	if !strings.Contains(err.Error(), "logging.cleanup.max_age_days must be >= 0") {
		t.Fatalf("Resolve error = %q", err.Error())
	}
}
