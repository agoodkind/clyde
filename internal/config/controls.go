package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

type ControlType string

const (
	ControlTypeBool   ControlType = "bool"
	ControlTypeEnum   ControlType = "enum"
	ControlTypeString ControlType = "string"
	ControlTypePath   ControlType = "path"
)

type controlKey string

const (
	controlKeyDefaultsRemote       controlKey = "defaults.remote_control"
	controlKeyMITMEnabledDefault   controlKey = "mitm.enabled_default"
	controlKeyMITMProviders        controlKey = "mitm.providers"
	controlKeyLoggingRawCapture    controlKey = "logging.raw_capture.enabled"
	controlKeyLoggingCleanup       controlKey = "logging.cleanup.enabled"
	controlKeyMITMCaptureDirectory controlKey = "mitm.capture_dir"
)

type ControlOption struct {
	Value       string
	Label       string
	Description string
}

type ControlDescriptor struct {
	Key          string
	Section      string
	Label        string
	Description  string
	Type         ControlType
	Value        string
	DefaultValue string
	Options      []ControlOption
	Sensitive    bool
	ReadOnly     bool
}

func ListControlDescriptors(cfg *Config) []ControlDescriptor {
	if cfg == nil {
		cfg = NewConfigWithDefaults()
	}
	return []ControlDescriptor{
		{
			Key:          "defaults.remote_control",
			Section:      "Session Defaults",
			Label:        "Remote control default",
			Description:  "Whether new sessions should launch with --remote-control by default.",
			Type:         ControlTypeBool,
			Value:        boolString(cfg.Defaults.RemoteControl),
			DefaultValue: "false",
		},
		{
			Key:          "mitm.enabled_default",
			Section:      "MITM Capture",
			Label:        "MITM capture default",
			Description:  "Whether supported subprocess launches should capture traffic by default.",
			Type:         ControlTypeBool,
			Value:        boolString(cfg.MITM.EnabledDefault),
			DefaultValue: "false",
		},
		{
			Key:          "mitm.providers",
			Section:      "MITM Capture",
			Label:        "MITM providers",
			Description:  "Comma-separated provider families that should route through the local capture proxy, or all.",
			Type:         ControlTypeString,
			Value:        formatMITMProviders(cfg.MITM.Providers),
			DefaultValue: "all",
			Options: []ControlOption{
				{Value: "all", Label: "All", Description: "Capture every configured provider family."},
				{Value: "claude", Label: "Claude", Description: "Capture Claude CLI traffic."},
				{Value: "codex", Label: "Codex", Description: "Capture Codex CLI and app-server traffic."},
				{Value: "cursor", Label: "Cursor", Description: "Capture Cursor traffic."},
			},
		},
		{
			Key:          "logging.raw_capture.enabled",
			Section:      "Logging",
			Label:        "Raw capture",
			Description:  "Whether raw request and response bodies should be written to local sidecar files.",
			Type:         ControlTypeBool,
			Value:        boolPointerString(cfg.Logging.RawCapture.Enabled, false),
			DefaultValue: "false",
		},
		{
			Key:          "logging.cleanup.enabled",
			Section:      "Logging",
			Label:        "Log cleanup",
			Description:  "Whether Clyde should delete rotated logs after the configured retention window.",
			Type:         ControlTypeBool,
			Value:        boolPointerString(cfg.Logging.Cleanup.Enabled, true),
			DefaultValue: "true",
		},
		{
			Key:          "mitm.capture_dir",
			Section:      "MITM Capture",
			Label:        "MITM capture dir",
			Description:  "Directory where the append-only MITM capture JSONL file is written.",
			Type:         ControlTypePath,
			Value:        cfg.MITM.CaptureDir,
			DefaultValue: filepath.Join(DefaultStateDir(), "mitm"),
		},
	}
}

func UpdateControlValue(cfg *Config, key, value string) error {
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	switch controlKey(strings.TrimSpace(key)) {
	case controlKeyDefaultsRemote:
		v, err := parseControlBool(value)
		if err != nil {
			return err
		}
		cfg.Defaults.RemoteControl = v
	case controlKeyMITMEnabledDefault:
		v, err := parseControlBool(value)
		if err != nil {
			return err
		}
		cfg.MITM.EnabledDefault = v
	case controlKeyMITMProviders:
		providers, err := parseMITMProviderControlValue(value)
		if err != nil {
			return err
		}
		cfg.MITM.Providers = providers
	case controlKeyLoggingRawCapture:
		v, err := parseControlBool(value)
		if err != nil {
			return err
		}
		cfg.Logging.RawCapture.Enabled = &v
		cfg.MITM.RawCaptureEnabled = v
	case controlKeyLoggingCleanup:
		v, err := parseControlBool(value)
		if err != nil {
			return err
		}
		cfg.Logging.Cleanup.Enabled = &v
	case controlKeyMITMCaptureDirectory:
		cfg.MITM.CaptureDir = strings.TrimSpace(value)
	default:
		return fmt.Errorf("unknown config control %q", key)
	}
	return applyLoggingDefaultsAndValidate(cfg)
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func boolPointerString(v *bool, fallback bool) string {
	if v == nil {
		return boolString(fallback)
	}
	return boolString(*v)
}

func parseControlBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid bool value %q", v)
	}
}
