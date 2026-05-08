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
