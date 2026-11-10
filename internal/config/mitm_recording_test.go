package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

func TestCaptureRecordingLoad(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
		deadline string
		invalid  bool
	}{
		{name: "unset"},
		{name: "empty", settings: "record_until=''"},
		{name: "future", settings: "record_until='2100-01-01T01:02:03Z'", deadline: "2100-01-01T01:02:03Z"},
		{name: "expired", settings: "record_until='2000-01-01T01:02:03-07:00'", deadline: "2000-01-01T01:02:03-07:00"},
		{name: "malformed", settings: "record_until='tomorrow'", invalid: true},
		{name: "missing zone", settings: "record_until='2100-01-01T01:02:03'", invalid: true},
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
				if err == nil || !strings.Contains(err.Error(), "mitm.capture_store.record_until") {
					t.Fatalf("invalid recording deadline error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			deadline, err := cfg.MITM.CaptureStore.RecordingDeadline()
			if err != nil {
				t.Fatal(err)
			}
			if test.deadline == "" {
				if !deadline.IsZero() {
					t.Fatalf("default deadline = %v", deadline)
				}
				return
			}
			if deadline.Format(time.RFC3339) != test.deadline {
				t.Fatalf("deadline = %v, want %s", deadline, test.deadline)
			}
		})
	}
}
