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
			Key:          "mitm.body_mode",
			Section:      "MITM Capture",
			Label:        "MITM body mode",
			Description:  "How much request/response body content to record in capture logs.",
			Type:         ControlTypeEnum,
			Value:        normalizeMITMBodyMode(cfg.MITM.BodyMode),
			DefaultValue: "summary",
			Options: []ControlOption{
				{Value: "summary", Label: "Summary", Description: "Record request/response shape only."},
				{Value: "raw", Label: "Raw", Description: "Record raw truncated bodies for local debugging."},
				{Value: "off", Label: "Off", Description: "Disable body capture while still logging metadata."},
			},
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
	switch strings.TrimSpace(key) {
	case "defaults.remote_control":
		v, err := parseControlBool(value)
		if err != nil {
			return err
		}
		cfg.Defaults.RemoteControl = v
	case "mitm.enabled_default":
		v, err := parseControlBool(value)
		if err != nil {
			return err
		}
		cfg.MITM.EnabledDefault = v
	case "mitm.providers":
		providers, err := parseMITMProviderControlValue(value)
		if err != nil {
			return err
		}
		cfg.MITM.Providers = providers
	case "mitm.body_mode":
		cfg.MITM.BodyMode = normalizeMITMBodyMode(value)
	case "mitm.capture_dir":
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
