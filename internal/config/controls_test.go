package config

import "testing"

func TestListControlDescriptorsIncludesMITMAndRemoteControl(t *testing.T) {
	cfg := NewConfigWithDefaults()
	controls := ListControlDescriptors(cfg)
	if len(controls) < 5 {
		t.Fatalf("controls=%d want at least 5", len(controls))
	}
	var foundRemote bool
	var foundMITM bool
	for _, control := range controls {
		if control.Key == "defaults.remote_control" {
			foundRemote = true
		}
		if control.Key == "mitm.capture_dir" {
			foundMITM = true
		}
	}
	if !foundRemote {
		t.Fatalf("defaults.remote_control missing")
	}
	if !foundMITM {
		t.Fatalf("mitm.capture_dir missing")
	}
}

func TestUpdateControlValueAppliesAndValidates(t *testing.T) {
	cfg := NewConfigWithDefaults()
	if err := UpdateControlValue(cfg, "mitm.enabled_default", "true"); err != nil {
		t.Fatalf("enable_default: %v", err)
	}
	if !cfg.MITM.EnabledDefault {
		t.Fatalf("EnabledDefault=false want true")
	}
	if err := UpdateControlValue(cfg, "mitm.providers", "claude"); err != nil {
		t.Fatalf("providers: %v", err)
	}
	if len(cfg.MITM.Providers) != 1 || cfg.MITM.Providers[0] != "claude" {
		t.Fatalf("Providers=%q want claude", cfg.MITM.Providers)
	}
	if err := UpdateControlValue(cfg, "mitm.capture_dir", " /tmp/captures "); err != nil {
		t.Fatalf("capture_dir: %v", err)
	}
	if cfg.MITM.CaptureDir != "/tmp/captures" {
		t.Fatalf("CaptureDir=%q want /tmp/captures", cfg.MITM.CaptureDir)
	}
	if err := UpdateControlValue(cfg, "mitm.body_mode", "bogus"); err == nil {
		t.Fatalf("expected invalid body_mode error")
	}
}

func TestMITMProviderControlAcceptsCursorSet(t *testing.T) {
	cfg := NewConfigWithDefaults()
	if err := UpdateControlValue(cfg, "mitm.providers", "cursor,codex"); err != nil {
		t.Fatalf("providers: %v", err)
	}
	if !cfg.MITM.EnabledFor("cursor") {
		t.Fatalf("cursor provider disabled")
	}
	if !cfg.MITM.EnabledFor("codex") {
		t.Fatalf("codex provider disabled")
	}
	if cfg.MITM.EnabledFor("claude") {
		t.Fatalf("claude provider enabled")
	}
}
