package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAdapterSecretReferences(t *testing.T) {
	const environment = "CLYDE_TEST_ADAPTER_SECRET"
	t.Setenv(environment, "environment-credential")

	for _, table := range []string{
		"adapter",
		"adapter.openai_compat_passthrough",
		"adapter.passthrough_overrides.test",
	} {
		for _, source := range []string{"env", "file"} {
			t.Run(
				table+"/"+source,
				func(t *testing.T) {
					directory := t.TempDir()
					if err := os.WriteFile(
						filepath.Join(directory, "credential"),
						[]byte("file-credential\n"),
						0o600,
					); err != nil {
						t.Fatal(err)
					}

					key := "api_key_"
					if table == "adapter" {
						key = "require_token_"
					}

					value := environment
					want := "environment-credential"
					if source == "file" {
						value = "credential"
						want = "file-credential"
					}

					writeModelConfig(
						t,
						directory,
						fmt.Sprintf("[%s]\n%s%s = %q\n", table, key, source, value),
					)
					cfg, err := loadConfig(directory)
					if err != nil {
						t.Fatal(err)
					}

					var actual string
					switch table {
					case "adapter":
						actual, err = cfg.Adapter.ResolveRequireToken()
					case "adapter.openai_compat_passthrough":
						entry := cfg.Adapter.OpenAICompatPassthrough
						actual, err = ResolvePassthroughAPIKey(
							table,
							entry.APIKey,
							entry.APIKeyEnv,
							entry.APIKeyFile,
						)
					default:
						entry := cfg.Adapter.PassthroughOverrides["test"]
						actual, err = ResolvePassthroughAPIKey(
							table,
							entry.APIKey,
							entry.APIKeyEnv,
							entry.APIKeyFile,
						)
					}

					if err != nil || actual != want {
						t.Fatalf("credential resolution failed: %v", err)
					}
				},
			)
		}
	}
}

func TestLoadAdapterRejectsInvalidSecretReferences(t *testing.T) {
	const environment = "CLYDE_TEST_INVALID_ADAPTER_SECRET"
	t.Setenv(environment, "")

	tests := []struct {
		name    string
		fields  string
		setting string
	}{
		{
			"absent environment",
			`require_token_env = "CLYDE_TEST_ABSENT_SECRET"`,
			"require_token_env",
		},
		{
			"empty environment",
			`require_token_env = "CLYDE_TEST_INVALID_ADAPTER_SECRET"`,
			"require_token_env",
		},
		{"blank environment name", `require_token_env = "  "`, "require_token_env"},
		{"empty environment name", `require_token_env = ""`, "require_token_env"},
		{"missing file", `require_token_file = "missing"`, "require_token_file"},
		{"empty file", `require_token_file = "empty"`, "require_token_file"},
		{"blank file", `require_token_file = "blank"`, "require_token_file"},
		{"unreadable file", `require_token_file = "."`, "require_token_file"},
		{"blank file path", `require_token_file = "  "`, "require_token_file"},
		{"empty file path", `require_token_file = ""`, "require_token_file"},
		{
			"literal and environment",
			"require_token = \"literal\"\nrequire_token_env = \"CLYDE_TEST_INVALID_ADAPTER_SECRET\"",
			"require_token",
		},
		{
			"literal and file",
			"require_token = \"literal\"\nrequire_token_file = \"blank\"",
			"require_token",
		},
		{
			"environment and file",
			"require_token_env = \"CLYDE_TEST_INVALID_ADAPTER_SECRET\"\nrequire_token_file = \"blank\"",
			"require_token",
		},
	}

	for _, test := range tests {
		t.Run(
			test.name,
			func(t *testing.T) {
				directory := t.TempDir()

				for name, contents := range map[string]string{"empty": "", "blank": " \t\n"} {
					if err := os.WriteFile(
						filepath.Join(directory, name),
						[]byte(contents),
						0o600,
					); err != nil {
						t.Fatal(err)
					}
				}

				writeModelConfig(t, directory, "[adapter]\n"+test.fields+"\n")
				_, err := loadConfig(directory)
				if err == nil || !strings.Contains(err.Error(), test.setting) {
					t.Fatalf("expected setting %s in error: %v", test.setting, err)
				}
			},
		)
	}
}

func TestLoadGlobalRejectsMissingAdapterSecretFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	directory := filepath.Join(root, "clyde")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	writeModelConfig(t, directory, "[adapter]\nrequire_token_file = \"missing\"\n")
	if _, err := LoadGlobalOrDefault(); err == nil {
		t.Fatal("missing credential file loaded default configuration")
	}
}

func TestLoadPassthroughSecretConflictsAndLiteralCompatibility(t *testing.T) {
	for _, table := range []string{
		"adapter.openai_compat_passthrough",
		"adapter.passthrough_overrides.test",
	} {
		for _, fields := range []string{
			"api_key = \"literal\"\napi_key_file = \"missing\"",
			"api_key_env = \"ENV\"\napi_key_file = \"missing\"",
			"api_key_file = \"\"",
		} {
			directory := t.TempDir()
			writeModelConfig(t, directory, "["+table+"]\n"+fields+"\n")
			if _, err := loadConfig(directory); err == nil ||
				!strings.Contains(err.Error(), table+".api_key_file") {
				t.Fatalf("expected file source conflict: %v", err)
			}
		}

		directory := t.TempDir()
		writeModelConfig(
			t,
			directory,
			"["+table+"]\napi_key = \"literal\"\napi_key_env = \"CLYDE_TEST_ABSENT_SECRET\"\n",
		)
		if _, err := loadConfig(directory); err != nil {
			t.Fatalf("literal precedence failed: %v", err)
		}
	}
}
