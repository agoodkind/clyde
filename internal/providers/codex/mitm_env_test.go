package codex

import "testing"

func TestSanitizeLaunchListRemovesMarkedProxyEnv(t *testing.T) {
	got := SanitizeLaunchList([]string{
		HTTPSProxyEnv + "=http://example.invalid",
		ClydeMITMProxyEnv + "=1",
		"KEEP=1",
	})

	if envValue(got, HTTPSProxyEnv) != "" {
		t.Fatalf("%s was not removed: %v", HTTPSProxyEnv, got)
	}
	if envValue(got, ClydeMITMProxyEnv) != "" {
		t.Fatalf("%s was not removed: %v", ClydeMITMProxyEnv, got)
	}
	if envValue(got, "KEEP") != "1" {
		t.Fatalf("KEEP missing from sanitized env: %v", got)
	}
}

func TestSanitizeLaunchListRemovesMarkedOpenAIBaseURL(t *testing.T) {
	got := SanitizeLaunchList([]string{
		OpenAIBaseURLEnv + "=http://[::1]:11434/v1",
		ClydeOpenAIBaseURLEnv + "=1",
		"KEEP=1",
	})

	if envValue(got, OpenAIBaseURLEnv) != "" {
		t.Fatalf("%s was not removed: %v", OpenAIBaseURLEnv, got)
	}
	if envValue(got, ClydeOpenAIBaseURLEnv) != "" {
		t.Fatalf("%s was not removed: %v", ClydeOpenAIBaseURLEnv, got)
	}
}

func TestSanitizeLaunchListRemovesLegacyLoopbackProxyEnv(t *testing.T) {
	got := SanitizeLaunchList([]string{
		HTTPSProxyEnv + "=http://[::1]:50067",
		"KEEP=1",
	})

	if envValue(got, HTTPSProxyEnv) != "" {
		t.Fatalf("legacy loopback %s was not removed: %v", HTTPSProxyEnv, got)
	}
}

func TestSanitizeLaunchListPreservesExternalProxyEnv(t *testing.T) {
	got := SanitizeLaunchList([]string{
		HTTPSProxyEnv + "=https://proxy.example",
		"KEEP=1",
	})

	if envValue(got, HTTPSProxyEnv) != "https://proxy.example" {
		t.Fatalf("%s changed unexpectedly: %v", HTTPSProxyEnv, got)
	}
}

func TestApplyLaunchListMarksAllClydeOwnedEnv(t *testing.T) {
	env := ApplyLaunchList(nil, " http://[::1]:11434/v1 ", " http://[::1]:50067 ", " /tmp/clyde-mitm-ca.crt ")

	if envValue(env, OpenAIBaseURLEnv) != "http://[::1]:11434/v1" {
		t.Fatalf("%s = %q", OpenAIBaseURLEnv, envValue(env, OpenAIBaseURLEnv))
	}
	if envValue(env, ClydeOpenAIBaseURLEnv) != "1" {
		t.Fatalf("%s = %q", ClydeOpenAIBaseURLEnv, envValue(env, ClydeOpenAIBaseURLEnv))
	}
	for _, key := range codexProxyEnvKeys {
		if envValue(env, key) != "http://[::1]:50067" {
			t.Fatalf("%s = %q", key, envValue(env, key))
		}
	}
	if envValue(env, ClydeMITMProxyEnv) != "1" {
		t.Fatalf("%s = %q", ClydeMITMProxyEnv, envValue(env, ClydeMITMProxyEnv))
	}
	for _, key := range codexCertEnvKeys {
		if envValue(env, key) != "/tmp/clyde-mitm-ca.crt" {
			t.Fatalf("%s = %q", key, envValue(env, key))
		}
	}
	if envValue(env, ClydeMITMCertEnv) != "1" {
		t.Fatalf("%s = %q", ClydeMITMCertEnv, envValue(env, ClydeMITMCertEnv))
	}
}
