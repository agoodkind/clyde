package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
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

func TestLifecycleGetSessionNameFallsBackToSessionIndex(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)

	paths, err := codexstore.ResolveStorePathsFromEnv()
	if err != nil {
		t.Fatalf("ResolveStorePathsFromEnv: %v", err)
	}
	body := `{"id":"thread-1","thread_name":"My renamed thread","updated_at":"2026-05-02T17:02:00Z"}` + "\n"
	if err := os.WriteFile(paths.SessionIndexPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write session index: %v", err)
	}

	sess := session.NewSession("codex-thread-1", "thread-1")
	sess.Metadata.DisplayTitle = "stale title"
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "thread-1"},
	}

	got, err := NewLifecycle().GetSessionName(context.Background(), sess)
	if err != nil {
		t.Fatalf("GetSessionName returned error: %v", err)
	}
	if got != "My renamed thread" {
		t.Fatalf("GetSessionName = %q, want %q", got, "My renamed thread")
	}
}

func TestLifecycleRenameSessionAppendsCodexThreadName(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)

	paths, err := codexstore.ResolveStorePathsFromEnv()
	if err != nil {
		t.Fatalf("ResolveStorePathsFromEnv: %v", err)
	}
	sess := session.NewSession("codex-thread-1", "thread-1")
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "thread-1"},
	}

	err = NewLifecycle().RenameSession(context.Background(), sess, "  Provider title  ")
	if err != nil {
		t.Fatalf("RenameSession returned error: %v", err)
	}
	idx, err := codexstore.ReadSessionIndex(paths.SessionIndexPath)
	if err != nil {
		t.Fatalf("ReadSessionIndex returned error: %v", err)
	}
	if got := idx.ThreadName("thread-1"); got != "Provider title" {
		t.Fatalf("ThreadName = %q, want Provider title", got)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
