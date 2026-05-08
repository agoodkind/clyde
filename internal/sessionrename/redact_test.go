package sessionrename

import (
	"strings"
	"testing"
)

func TestRedactScrubsLongDigits(t *testing.T) {
	policy := DefaultRedactPolicy()
	got := Redact("call ticket 1234567 today", policy)
	want := "call ticket <num> today"
	if got != want {
		t.Fatalf("Redact long digits: got %q, want %q", got, want)
	}
}

func TestRedactKeepsShortDigits(t *testing.T) {
	policy := DefaultRedactPolicy()
	got := Redact("version 12345 ships", policy)
	want := "version 12345 ships"
	if got != want {
		t.Fatalf("Redact preserves 5-digit run: got %q, want %q", got, want)
	}
}

func TestRedactScrubsEmail(t *testing.T) {
	policy := DefaultRedactPolicy()
	got := Redact("ping alice@example.com next", policy)
	want := "ping <email> next"
	if got != want {
		t.Fatalf("Redact email: got %q, want %q", got, want)
	}
}

func TestRedactScrubsPath(t *testing.T) {
	policy := DefaultRedactPolicy()
	got := Redact("read /Users/alex/project/file.go please", policy)
	if !strings.Contains(got, "<path>") {
		t.Fatalf("Redact path: expected <path> marker in %q", got)
	}
}

func TestRedactScrubsKeyPrefix(t *testing.T) {
	policy := DefaultRedactPolicy()
	cases := []string{
		"my key sk-abcdefgh12345",
		"export ghp_aaaaaaaaaaaaaaaa",
		"export gho_bbbbbbbbbbbbbbbb",
		"id AKIAABCDEFGHIJKLMNOP",       // gitleaks:allow
		"google AIzaSyAaBbCcDdEeFfGgHh", // gitleaks:allow
		"slack xoxb-1234567890-abcdefg",
		"slack xoxp-aaaaaaaa-bbbbbbb",
		"slack xoxc-cccccccccccccccc",
	}
	for _, input := range cases {
		got := Redact(input, policy)
		if !strings.Contains(got, "<key>") {
			t.Errorf("Redact key prefix: expected <key> in output for %q, got %q", input, got)
		}
	}
}

func TestRedactPolicyTogglesOff(t *testing.T) {
	allOff := RedactPolicy{}
	got := Redact("alice@example.com sk-abcdefgh12345 1234567 /usr/bin/clyde", allOff)
	if !strings.Contains(got, "alice@example.com") {
		t.Errorf("Emails toggled off: expected raw email, got %q", got)
	}
	if !strings.Contains(got, "sk-abcdefgh12345") {
		t.Errorf("Keys toggled off: expected raw key, got %q", got)
	}
	if !strings.Contains(got, "1234567") {
		t.Errorf("Numbers toggled off: expected raw digits, got %q", got)
	}
	if !strings.Contains(got, "/usr/bin/clyde") {
		t.Errorf("Paths toggled off: expected raw path, got %q", got)
	}
}

func TestRedactCapsMessageLength(t *testing.T) {
	policy := DefaultRedactPolicy()
	long := strings.Repeat("a", MessageCharCap+50)
	got := Redact(long, policy)
	if len(got) != MessageCharCap {
		t.Fatalf("Redact length cap: got %d, want %d", len(got), MessageCharCap)
	}
}

func TestRedactMessagesJoinsAndCaps(t *testing.T) {
	policy := DefaultRedactPolicy()
	messages := []string{
		"first message about ticket 1234567",
		"second message asks alice@example.com to help",
		"third message references /home/alex/repo/main.go",
	}
	got := RedactMessages(messages, policy)
	if !strings.Contains(got, MessageSeparator) {
		t.Fatalf("RedactMessages join: expected separator %q in %q", MessageSeparator, got)
	}
	if !strings.Contains(got, "<num>") || !strings.Contains(got, "<email>") || !strings.Contains(got, "<path>") {
		t.Fatalf("RedactMessages scrub: expected all markers in %q", got)
	}
	if len(got) > JoinedCharCap {
		t.Fatalf("RedactMessages cap: got len %d, want <=%d", len(got), JoinedCharCap)
	}
}

func TestRedactMessagesHandlesFewerThanThree(t *testing.T) {
	policy := DefaultRedactPolicy()
	messages := []string{"only one message"}
	got := RedactMessages(messages, policy)
	if got != "only one message" {
		t.Fatalf("RedactMessages with one input: got %q, want %q", got, "only one message")
	}
}

func TestRedactMessagesCapsJoinedLength(t *testing.T) {
	policy := DefaultRedactPolicy()
	long := strings.Repeat("a", MessageCharCap)
	messages := []string{long, long, long, long}
	got := RedactMessages(messages, policy)
	if len(got) > JoinedCharCap {
		t.Fatalf("RedactMessages joined cap: got %d, want <=%d", len(got), JoinedCharCap)
	}
}
