package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

func TestCaptureRetentionLoad(t *testing.T) {
	for _, test := range []struct {
		name, settings string
		age            time.Duration
		bytes          int64
		invalid        bool
	}{
		{name: "defaults", age: 168 * time.Hour, bytes: 5_000_000_000},
		{name: "disable age", settings: "retention_max_age='0s'", bytes: 5_000_000_000},
		{name: "disable size", settings: "retention_max_bytes=0", age: 168 * time.Hour},
		{name: "disable both", settings: "retention_max_age='0s'\nretention_max_bytes=0", invalid: true},
		{name: "negative age", settings: "retention_max_age='-1s'", invalid: true},
		{name: "negative bytes", settings: "retention_max_bytes=-1", invalid: true},
		{name: "zero interval", settings: "retention_interval='0s'", invalid: true},
		{name: "negative interval", settings: "retention_interval='-1s'", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", directory)
			configDirectory := filepath.Join(directory, "clyde")
			if err := os.MkdirAll(configDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDirectory, "config.toml"), []byte("[mitm.capture_store]\n"+test.settings), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadGlobalOrDefault()
			if test.invalid {
				if err == nil {
					t.Fatal("invalid retention configuration loaded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MITM.CaptureStore.RetentionMaxAge.AsDuration() != test.age || cfg.MITM.CaptureStore.RetentionMaxBytes != test.bytes || cfg.MITM.CaptureStore.RetentionInterval.AsDuration() != time.Second {
				t.Fatalf("unexpected retention: %+v", cfg.MITM.CaptureStore)
			}
		})
	}
}
