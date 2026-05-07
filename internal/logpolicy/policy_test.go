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
	if daemonPolicy.Retention.CleanupMode != logpolicy.CleanupOff {
		t.Fatalf("daemon cleanup mode = %q, want %q", daemonPolicy.Retention.CleanupMode, logpolicy.CleanupOff)
	}
	if _, ok := policies.Concerns[slogger.ConcernAdapterHTTPRaw]; !ok {
		t.Fatalf("policy set should include registered concern %q", slogger.ConcernAdapterHTTPRaw)
	}
}

func TestResolveAppliesSinkAndConcernPrecedence(t *testing.T) {
	enabled := false
	maxTotalMB := 512
	maxSizeMB := 7
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Level = "info"
	cfg.Logging.Sinks = config.LoggingSinks{
		config.LoggingSinkConcerns: {
			Level:   "warn",
			Detail:  "verbose",
			Enabled: &enabled,
			Retention: config.LoggingRetention{
				MaxTotalMB:  &maxTotalMB,
				CleanupMode: "audit_only",
			},
		},
	}
	cfg.Logging.Concerns = config.LoggingConcerns{
		slogger.ConcernAdapterHTTPRaw: {
			Level:  "debug",
			Detail: "raw",
			Rotation: config.LoggingRotation{
				MaxSizeMB: maxSizeMB,
			},
		},
	}

	policies, err := logpolicy.Resolve(*cfg)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	sinkPolicy := policies.Sinks[logpolicy.SinkConcerns]
	if sinkPolicy.Level != logpolicy.LevelWarn {
		t.Fatalf("concerns sink level = %q, want %q", sinkPolicy.Level, logpolicy.LevelWarn)
	}
	if sinkPolicy.Enabled {
		t.Fatalf("concerns sink should use explicit disabled override")
	}
	if sinkPolicy.Retention.MaxTotalMB == nil || *sinkPolicy.Retention.MaxTotalMB != 512 {
		t.Fatalf("concerns sink max total MB = %v, want 512", sinkPolicy.Retention.MaxTotalMB)
	}

	concernPolicy := policies.Concerns[slogger.ConcernAdapterHTTPRaw]
	if concernPolicy.Enabled {
		t.Fatalf("concern should inherit disabled sink state")
	}
	if concernPolicy.Level != logpolicy.LevelDebug {
		t.Fatalf("concern level = %q, want %q", concernPolicy.Level, logpolicy.LevelDebug)
	}
	if concernPolicy.Detail != logpolicy.DetailRaw {
		t.Fatalf("concern detail = %q, want %q", concernPolicy.Detail, logpolicy.DetailRaw)
	}
	if concernPolicy.Rotation.MaxSizeMB != maxSizeMB {
		t.Fatalf("concern rotation max size = %d, want %d", concernPolicy.Rotation.MaxSizeMB, maxSizeMB)
	}
	if concernPolicy.Retention.CleanupMode != logpolicy.CleanupAuditOnly {
		t.Fatalf("concern cleanup mode = %q, want %q", concernPolicy.Retention.CleanupMode, logpolicy.CleanupAuditOnly)
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
	enabled := false
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Sinks = config.LoggingSinks{
		config.LoggingSinkDaemon: {
			Enabled: &enabled,
		},
	}
	cfg.Logging.Concerns = config.LoggingConcerns{
		slogger.ConcernAdapterHTTPRaw: {
			Level: "debug",
		},
	}

	policy, err := logpolicy.ResolveSloggerSetup(*cfg, slogger.ProcessRoleDaemon)
	if err != nil {
		t.Fatalf("ResolveSloggerSetup returned error: %v", err)
	}

	if policy.ProcessSink.Enabled {
		t.Fatalf("daemon process sink should use disabled override")
	}
	concernPolicy := policy.ConcernPolicies[slogger.ConcernAdapterHTTPRaw]
	if concernPolicy.Level == nil || *concernPolicy.Level != slog.LevelDebug {
		t.Fatalf("concern slog level = %v, want debug", concernPolicy.Level)
	}
}

func TestResolveSloggerSetupDisablesConcernFileForProcessSink(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Concerns = config.LoggingConcerns{
		slogger.ConcernAdapterHTTPRaw: {
			Sink: config.LoggingSinkDaemon,
		},
	}

	policy, err := logpolicy.ResolveSloggerSetup(*cfg, slogger.ProcessRoleDaemon)
	if err != nil {
		t.Fatalf("ResolveSloggerSetup returned error: %v", err)
	}

	concernPolicy := policy.ConcernPolicies[slogger.ConcernAdapterHTTPRaw]
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

func TestResolveRejectsInvalidRetention(t *testing.T) {
	negativeMaxAgeDays := -1
	cfg := config.NewConfigWithDefaults()
	cfg.Logging.Sinks = config.LoggingSinks{
		config.LoggingSinkDaemon: {
			Retention: config.LoggingRetention{
				MaxAgeDays: &negativeMaxAgeDays,
			},
		},
	}

	_, err := logpolicy.Resolve(*cfg)
	if err == nil {
		t.Fatalf("Resolve returned nil error")
	}
	if !strings.Contains(err.Error(), "logging.sinks.daemon.retention.max_age_days must be >= 0") {
		t.Fatalf("Resolve error = %q", err.Error())
	}
}
