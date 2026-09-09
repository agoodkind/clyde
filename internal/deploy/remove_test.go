package deploy

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type removalRunner struct {
	commands   []command
	absent     bool
	failAction string
}

func (runner *removalRunner) run(_ context.Context, current command) commandResult {
	runner.commands = append(runner.commands, current)
	if strings.Contains(current.display(), runner.failAction) && runner.failAction != "" {
		return commandResult{exitCode: 1, err: fmt.Errorf("fixture service manager failure")}
	}
	if current.name == "launchctl" && current.args[0] == "print" && runner.absent {
		return commandResult{output: "Could not find service", exitCode: 113, err: fmt.Errorf("missing registration")}
	}
	if current.name == "systemctl" && current.args[1] == "show" {
		if runner.absent {
			return commandResult{output: "not-found\n"}
		}
		return commandResult{output: "loaded\n"}
	}
	return commandResult{output: "active\n"}
}

func TestRemoveNativeServiceUsesExactPlatformCommands(t *testing.T) {
	t.Parallel()
	for _, platform := range []platform{platformDarwin, platformLinux} {
		for _, absent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/absent=%t", platform, absent), func(t *testing.T) {
				fixture := newDeployFixture(t, platform)
				runner := &removalRunner{absent: absent}
				executor := executor{config: fixture.config, files: osFileSystem{}, runner: runner, logger: newLogger(io.Discard), stdout: &fixture.stdout}
				path, err := executor.removableServicePath()
				if err != nil {
					t.Fatal(err)
				}
				if !absent {
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("old Clyde registration"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := executor.removeService(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("registration remains: %v", err)
				}
				var want []command
				if platform == platformDarwin {
					target := fixture.config.LaunchdDomain + "/" + defaultLaunchdLabel
					want = []command{{name: "launchctl", args: []string{"print", target}}}
					if !absent {
						want = append(want, command{name: "launchctl", args: []string{"bootout", target}})
					}
				} else {
					want = []command{{name: "systemctl", args: []string{"--user", "show", defaultSystemdUnit, "--property=LoadState", "--value"}}}
					if !absent {
						want = append(want, command{name: "systemctl", args: []string{"--user", "stop", defaultSystemdUnit}}, command{name: "systemctl", args: []string{"--user", "disable", defaultSystemdUnit}})
					}
					want = append(want, command{name: "systemctl", args: []string{"--user", "daemon-reload"}})
				}
				if !reflect.DeepEqual(runner.commands, want) {
					t.Fatalf("commands = %v, want %v", runner.commands, want)
				}
			})
		}
	}
}

func TestRemoveNativeServicePreservesRegistrationOnManagerFailure(t *testing.T) {
	t.Parallel()
	fixture := newDeployFixture(t, platformLinux)
	runner := &removalRunner{failAction: "stop"}
	executor := executor{config: fixture.config, files: osFileSystem{}, runner: runner, logger: newLogger(io.Discard)}
	path := fixture.config.SystemdUserUnit
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("preserve registration"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := executor.removeService(t.Context()); err == nil {
		t.Fatal("expected service removal failure")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "preserve registration" {
		t.Fatalf("registration changed: %q, %v", body, err)
	}
}

func TestNativeTemplatesPreserveRootEnvironment(t *testing.T) {
	t.Parallel()
	for _, platform := range []platform{platformDarwin, platformLinux} {
		t.Run(string(platform), func(t *testing.T) {
			fixture := newDeployFixture(t, platform)
			expected := []field{{Key: "XDG_CONFIG_HOME", Value: "/tmp/config & data"}, {Key: "XDG_STATE_HOME", Value: "/tmp/state%root"}, {Key: "XDG_CACHE_HOME", Value: "/tmp/cache"}, {Key: "XDG_RUNTIME_DIR", Value: "/tmp/run"}}
			fixture.config.Environment = rootEnvironment(func(key string) (string, bool) {
				for _, entry := range expected {
					if entry.Key == key {
						return entry.Value, true
					}
				}
				return "", false
			})
			template := darwinTemplate
			if platform == platformLinux {
				template = linuxTemplate
			}
			body := fixture.mustRenderTemplate(t, template, "clyde")
			values := decodeServiceEnvironment(t, platform, body)
			for _, entry := range expected {
				if values[entry.Key] != entry.Value {
					t.Fatalf("%s = %q, want %q", entry.Key, values[entry.Key], entry.Value)
				}
			}
		})
	}
}

func TestLinuxInstallAndRemovalShareCustomConfigRoot(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	configRoot := filepath.Join(home, "custom-config")
	cfg, err := loadConfigFromEnv(platformLinux, func(key string) (string, bool) {
		switch key {
		case "HOME":
			return home, true
		case "XDG_CONFIG_HOME":
			return configRoot, true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &removalRunner{}
	var output bytes.Buffer
	if err := run(t.Context(), cfg, dependencies{fileSystem: osFileSystem{}, runner: runner, logWriter: &output}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configRoot, "systemd", "user", defaultSystemdUnit)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unit was not installed in systemd's XDG search path: %v", err)
	}
	if decodeServiceEnvironment(t, platformLinux, body)["XDG_CONFIG_HOME"] != configRoot {
		t.Fatal("launched daemon root differs")
	}
	executor := executor{config: cfg, files: osFileSystem{}, runner: runner, logger: newLogger(&output)}
	if err := executor.removeService(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("custom unit remains: %v", err)
	}
}

func decodeServiceEnvironment(t *testing.T, platform platform, body []byte) map[string]string {
	t.Helper()
	values := make(map[string]string)
	if platform == platformLinux {
		for _, line := range strings.Split(string(body), "\n") {
			value, found := strings.CutPrefix(line, "Environment=")
			if !found {
				continue
			}
			if strings.HasPrefix(value, "\"") {
				var err error
				value, err = strconv.Unquote(value)
				if err != nil {
					t.Fatal(err)
				}
			}
			key, value, _ := strings.Cut(value, "=")
			values[key] = strings.ReplaceAll(value, "%%", "%")
		}
		return values
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	key := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			if err := decoder.DecodeElement(&key, &start); err != nil {
				t.Fatal(err)
			}
		} else if start.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				t.Fatal(err)
			}
			values[key] = value
		}
	}
	return values
}
