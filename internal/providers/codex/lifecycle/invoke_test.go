package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	codexprovider "goodkind.io/clyde/internal/providers/codex"
	"goodkind.io/clyde/internal/session"
)

func TestLifecycleResumeInteractiveInvokesCodexResume(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record.txt")
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf 'args=%s\\n' \"$*\" > " + shellQuote(record) + "\nprintf 'cwd=%s\\n' \"$PWD\" >> " + shellQuote(record) + "\nprintf 'session=%s\\n' \"$CLYDE_SESSION_NAME\" >> " + shellQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	old := BinaryPathFunc
	BinaryPathFunc = func() string { return bin }
	defer func() { BinaryPathFunc = old }()

	workDir := filepath.Join(dir, "work")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	sess := session.NewSession("codex-session", "codex-123")
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "codex-123"},
	}

	err := NewLifecycle().ResumeInteractive(context.Background(), session.ResumeRequest{
		Session: sess,
		Options: session.ResumeOptions{
			CurrentWorkDir: workDir,
		},
	})
	if err != nil {
		t.Fatalf("ResumeInteractive returned error: %v", err)
	}
	gotBytes, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	got := string(gotBytes)
	wd, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("eval work dir: %v", err)
	}
	for _, want := range []string{
		"args=resume codex-123",
		"cwd=" + wd,
		"session=codex-session",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("record missing %q:\n%s", want, got)
		}
	}
}

func TestCodexResumeArgsSupportIDNameLastAndAll(t *testing.T) {
	cases := []struct {
		name string
		req  session.OpaqueResumeRequest
		want []string
	}{
		{
			name: "thread id",
			req:  session.OpaqueResumeRequest{Query: "019de9aa-3a00-7010-bd9f-a6ee71559357"},
			want: []string{"resume", "019de9aa-3a00-7010-bd9f-a6ee71559357"},
		},
		{
			name: "thread name",
			req:  session.OpaqueResumeRequest{Query: "visible-name"},
			want: []string{"resume", "visible-name"},
		},
		{
			name: "last shorthand",
			req:  session.OpaqueResumeRequest{Query: "last"},
			want: []string{"resume", "--last"},
		},
		{
			name: "native last flag",
			req:  session.OpaqueResumeRequest{Query: "--last"},
			want: []string{"resume", "--last"},
		},
		{
			name: "cwd all flag",
			req:  session.OpaqueResumeRequest{Query: "visible-name", AdditionalArgs: []string{"--all"}},
			want: []string{"resume", "visible-name", "--all"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codexResumeArgs(tc.req)
			if err != nil {
				t.Fatalf("codexResumeArgs returned error: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("codexResumeArgs = %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestApplyMITMEnvAddsProxyEnvForWrapperLaunch(t *testing.T) {
	writeMITMConfig(t, true)
	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(_ context.Context, provider string) ([]*clydev1.EnvironmentVariable, error) {
		if provider != "codex" {
			t.Fatalf("provider = %q, want codex", provider)
		}
		return []*clydev1.EnvironmentVariable{
			{Key: codexprovider.OpenAIBaseURLEnv, Value: "http://[::1]:11434/v1"},
			{Key: codexprovider.HTTPSProxyEnv, Value: "http://daemon.example"},
			{Key: codexprovider.SSLCertFileEnv, Value: "/tmp/clyde-mitm-ca.crt"},
		}, nil
	}

	got := applyMITMEnv(context.Background(), []string{codexprovider.HTTPSProxyEnv + "=https://old.example", "KEEP=1"})

	if codexproviderEnvValue(got, "KEEP") != "1" {
		t.Fatalf("KEEP missing from env: %v", got)
	}
	if codexproviderEnvValue(got, codexprovider.OpenAIBaseURLEnv) != "http://[::1]:11434/v1" {
		t.Fatalf("%s = %q, want adapter base URL", codexprovider.OpenAIBaseURLEnv, codexproviderEnvValue(got, codexprovider.OpenAIBaseURLEnv))
	}
	for _, key := range []string{
		codexprovider.HTTPProxyEnv,
		codexprovider.HTTPSProxyEnv,
		codexprovider.AllProxyEnv,
		"http_proxy",
		"https_proxy",
		"all_proxy",
	} {
		if codexproviderEnvValue(got, key) != "http://daemon.example" {
			t.Fatalf("%s = %q, want daemon proxy", key, codexproviderEnvValue(got, key))
		}
	}
	for _, key := range []string{codexprovider.SSLCertFileEnv, codexprovider.NodeExtraCACertsEnv} {
		if codexproviderEnvValue(got, key) != "/tmp/clyde-mitm-ca.crt" {
			t.Fatalf("%s = %q, want Clyde MITM cert path", key, codexproviderEnvValue(got, key))
		}
	}
	if codexproviderEnvValue(got, codexprovider.ClydeOpenAIBaseURLEnv) != "1" {
		t.Fatalf("%s = %q, want 1", codexprovider.ClydeOpenAIBaseURLEnv, codexproviderEnvValue(got, codexprovider.ClydeOpenAIBaseURLEnv))
	}
	if codexproviderEnvValue(got, codexprovider.ClydeMITMProxyEnv) != "1" {
		t.Fatalf("%s = %q, want 1", codexprovider.ClydeMITMProxyEnv, codexproviderEnvValue(got, codexprovider.ClydeMITMProxyEnv))
	}
	if codexproviderEnvValue(got, codexprovider.ClydeMITMCertEnv) != "1" {
		t.Fatalf("%s = %q, want 1", codexprovider.ClydeMITMCertEnv, codexproviderEnvValue(got, codexprovider.ClydeMITMCertEnv))
	}
}

func TestApplyMITMEnvFailsOpenWhenDaemonUnavailable(t *testing.T) {
	writeMITMConfig(t, true)
	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(context.Context, string) ([]*clydev1.EnvironmentVariable, error) {
		return nil, errors.New("daemon unavailable")
	}

	got := applyMITMEnv(context.Background(), []string{codexprovider.HTTPSProxyEnv + "=https://proxy.example", "KEEP=1"})

	if codexproviderEnvValue(got, "KEEP") != "1" {
		t.Fatalf("KEEP missing from env: %v", got)
	}
	if codexproviderEnvValue(got, codexprovider.HTTPSProxyEnv) != "https://proxy.example" {
		t.Fatalf("%s = %q, want original proxy after fail open", codexprovider.HTTPSProxyEnv, codexproviderEnvValue(got, codexprovider.HTTPSProxyEnv))
	}
}

func TestApplyMITMEnvDropsInheritedLoopbackWhenDaemonUnavailable(t *testing.T) {
	writeMITMConfig(t, true)
	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(context.Context, string) ([]*clydev1.EnvironmentVariable, error) {
		return nil, errors.New("daemon unavailable")
	}

	got := applyMITMEnv(context.Background(), []string{
		codexprovider.OpenAIBaseURLEnv + "=http://[::1]:11434/v1",
		codexprovider.ClydeOpenAIBaseURLEnv + "=1",
		codexprovider.HTTPSProxyEnv + "=http://[::1]:50067",
		codexprovider.ClydeMITMProxyEnv + "=1",
		codexprovider.SSLCertFileEnv + "=/tmp/clyde-mitm-ca.crt",
		codexprovider.ClydeMITMCertEnv + "=1",
		"KEEP=1",
	})

	if codexproviderEnvValue(got, "KEEP") != "1" {
		t.Fatalf("KEEP missing from env: %v", got)
	}
	if codexproviderEnvValue(got, codexprovider.OpenAIBaseURLEnv) != "" {
		t.Fatalf("%s = %q, want stale Clyde adapter base URL removed", codexprovider.OpenAIBaseURLEnv, codexproviderEnvValue(got, codexprovider.OpenAIBaseURLEnv))
	}
	if codexproviderEnvValue(got, codexprovider.HTTPSProxyEnv) != "" {
		t.Fatalf("%s = %q, want stale Clyde proxy removed", codexprovider.HTTPSProxyEnv, codexproviderEnvValue(got, codexprovider.HTTPSProxyEnv))
	}
	if codexproviderEnvValue(got, codexprovider.SSLCertFileEnv) != "" {
		t.Fatalf("%s = %q, want stale Clyde cert removed", codexprovider.SSLCertFileEnv, codexproviderEnvValue(got, codexprovider.SSLCertFileEnv))
	}
}

func TestLifecycleRecentContextMessagesUsesCodexHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first user"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"first assistant","phase":"commentary"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:07.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second user"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:08.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"123456789012345"}],"phase":"final"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	sess := session.NewSession("codex-session", "codex-123")
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "codex-123"},
		Artifacts: session.ProviderArtifacts{
			TranscriptPath: path,
		},
	}

	recent := NewLifecycle().RecentContextMessages(sess, 2, 12)
	if len(recent) != 2 {
		t.Fatalf("recent len = %d, want 2", len(recent))
	}
	if recent[0].Role != "user" || recent[0].Text != "second user" {
		t.Fatalf("recent[0] = %#v", recent[0])
	}
	if recent[1].Role != "assistant" || recent[1].Text != "123456789012..." {
		t.Fatalf("recent[1] = %#v", recent[1])
	}
}

func writeMITMConfig(t *testing.T, enabledDefault bool) {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	enabled := "false"
	if enabledDefault {
		enabled = "true"
	}
	cfg := []byte("[mitm]\nenabled_default = " + enabled + "\nproviders = [\"codex\"]\nbody_mode = \"summary\"\ncapture_dir = \"" + t.TempDir() + "\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func codexproviderEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value
		}
	}
	return ""
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
