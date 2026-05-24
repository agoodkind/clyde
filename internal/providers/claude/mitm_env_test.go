package claude

import "testing"

func TestSanitizeMITMListRemovesMarkedBaseURL(t *testing.T) {
	got := SanitizeMITMList([]string{
		AnthropicBaseURLEnv + "=http://example.invalid",
		ClydeMITMAnthropicBaseURLEnv + "=1",
		"KEEP=1",
	})

	if envValue(got, AnthropicBaseURLEnv) != "" {
		t.Fatalf("%s was not removed: %v", AnthropicBaseURLEnv, got)
	}
	if envValue(got, ClydeMITMAnthropicBaseURLEnv) != "" {
		t.Fatalf("%s was not removed: %v", ClydeMITMAnthropicBaseURLEnv, got)
	}
	if envValue(got, "KEEP") != "1" {
		t.Fatalf("KEEP missing from sanitized env: %v", got)
	}
}

func TestSanitizeMITMListRemovesLegacyLoopbackBaseURL(t *testing.T) {
	got := SanitizeMITMList([]string{
		AnthropicBaseURLEnv + "=http://[::1]:50067",
		"KEEP=1",
	})

	if envValue(got, AnthropicBaseURLEnv) != "" {
		t.Fatalf("legacy loopback %s was not removed: %v", AnthropicBaseURLEnv, got)
	}
}

func TestSanitizeMITMListPreservesExternalBaseURL(t *testing.T) {
	got := SanitizeMITMList([]string{
		AnthropicBaseURLEnv + "=https://old.example",
		"KEEP=1",
	})

	if envValue(got, AnthropicBaseURLEnv) != "https://old.example" {
		t.Fatalf("%s changed unexpectedly: %v", AnthropicBaseURLEnv, got)
	}
}

func TestSanitizeMITMMapRemovesLegacyLoopbackBaseURL(t *testing.T) {
	env := map[string]string{
		AnthropicBaseURLEnv: "http://localhost:50067",
		"KEEP":              "1",
	}

	SanitizeMITMMap(env)

	if _, ok := env[AnthropicBaseURLEnv]; ok {
		t.Fatalf("legacy loopback %s was not removed: %#v", AnthropicBaseURLEnv, env)
	}
	if env["KEEP"] != "1" {
		t.Fatalf("KEEP changed unexpectedly: %#v", env)
	}
}

func TestApplyMITMMapMarksBaseURLAsClydeOwned(t *testing.T) {
	env := map[string]string{}

	ApplyMITMMap(env, " http://[::1]:50067 ")

	if env[AnthropicBaseURLEnv] != "http://[::1]:50067" {
		t.Fatalf("%s = %q", AnthropicBaseURLEnv, env[AnthropicBaseURLEnv])
	}
	if env[ClydeMITMAnthropicBaseURLEnv] != "1" {
		t.Fatalf("%s = %q", ClydeMITMAnthropicBaseURLEnv, env[ClydeMITMAnthropicBaseURLEnv])
	}
}

func TestApplyClaudeConfigDirMapMarksConfigDirAsClydeOwned(t *testing.T) {
	env := map[string]string{}

	ApplyClaudeConfigDirMap(env, " /tmp/scratch ")

	if env[ClaudeConfigDirEnv] != "/tmp/scratch" {
		t.Fatalf("%s = %q", ClaudeConfigDirEnv, env[ClaudeConfigDirEnv])
	}
	if env[ClydeLaunchClaudeConfigDirEnv] != "1" {
		t.Fatalf("%s = %q", ClydeLaunchClaudeConfigDirEnv, env[ClydeLaunchClaudeConfigDirEnv])
	}
}

func TestApplyClaudeConfigDirMapIgnoresEmpty(t *testing.T) {
	env := map[string]string{}

	ApplyClaudeConfigDirMap(env, "   ")

	if _, ok := env[ClaudeConfigDirEnv]; ok {
		t.Fatalf("%s set for empty config dir: %#v", ClaudeConfigDirEnv, env)
	}
	if _, ok := env[ClydeLaunchClaudeConfigDirEnv]; ok {
		t.Fatalf("%s set for empty config dir: %#v", ClydeLaunchClaudeConfigDirEnv, env)
	}
}

func TestSanitizeMITMMapScrubsMarkedClaudeConfigDir(t *testing.T) {
	env := map[string]string{
		ClaudeConfigDirEnv:            "/tmp/stale-scratch",
		ClydeLaunchClaudeConfigDirEnv: "1",
		"KEEP":                        "1",
	}

	SanitizeMITMMap(env)

	if _, ok := env[ClaudeConfigDirEnv]; ok {
		t.Fatalf("marked %s was not scrubbed: %#v", ClaudeConfigDirEnv, env)
	}
	if _, ok := env[ClydeLaunchClaudeConfigDirEnv]; ok {
		t.Fatalf("marker %s was not removed: %#v", ClydeLaunchClaudeConfigDirEnv, env)
	}
	if env["KEEP"] != "1" {
		t.Fatalf("KEEP changed unexpectedly: %#v", env)
	}
}

func TestSanitizeMITMMapPreservesUnmarkedClaudeConfigDir(t *testing.T) {
	env := map[string]string{
		ClaudeConfigDirEnv: "/home/user/.claude",
	}

	SanitizeMITMMap(env)

	if env[ClaudeConfigDirEnv] != "/home/user/.claude" {
		t.Fatalf("user-owned %s changed: %#v", ClaudeConfigDirEnv, env)
	}
}

func TestSanitizeMITMListScrubsMarkedClaudeConfigDir(t *testing.T) {
	got := SanitizeMITMList([]string{
		ClaudeConfigDirEnv + "=/tmp/stale-scratch",
		ClydeLaunchClaudeConfigDirEnv + "=1",
		"KEEP=1",
	})

	if envValue(got, ClaudeConfigDirEnv) != "" {
		t.Fatalf("marked %s was not scrubbed: %v", ClaudeConfigDirEnv, got)
	}
	if envValue(got, ClydeLaunchClaudeConfigDirEnv) != "" {
		t.Fatalf("marker %s was not removed: %v", ClydeLaunchClaudeConfigDirEnv, got)
	}
	if envValue(got, "KEEP") != "1" {
		t.Fatalf("KEEP missing from sanitized env: %v", got)
	}
}

func TestSanitizeMITMListPreservesUnmarkedClaudeConfigDir(t *testing.T) {
	got := SanitizeMITMList([]string{
		ClaudeConfigDirEnv + "=/home/user/.claude",
	})

	if envValue(got, ClaudeConfigDirEnv) != "/home/user/.claude" {
		t.Fatalf("user-owned %s changed: %v", ClaudeConfigDirEnv, got)
	}
}
