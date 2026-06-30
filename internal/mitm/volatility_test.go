package mitm

import "testing"

// TestHeaderValueIsVolatile pins the name-agnostic churn detector: byte sizes,
// per-session ids, and attestation blobs are volatile, while product
// user-agents, beta sets, and version strings are stable identity.
func TestHeaderValueIsVolatile(t *testing.T) {
	t.Parallel()
	type tc struct {
		name  string
		value string
		want  bool
	}
	cases := []tc{
		{name: "content length", value: "1367204", want: true},
		{name: "small content length", value: "11034", want: true},
		{name: "session uuid", value: "48749d33-ea61-4b6c-8692-33dcda68a533", want: true},
		{name: "attestation blob", value: "eyJhbGciOiJSUzI1NiJ9aGVsbG93b3JsZGZvb2Jhcg", want: true},
		{name: "user agent", value: "claude-cli/2.1.123 (external, sdk-cli)", want: false},
		{name: "beta set", value: "oauth-2025-04-20,claude-code-20250219", want: false},
		{name: "anthropic version", value: "2023-06-01", want: false},
		{name: "content type", value: "application/json", want: false},
		{name: "encoding", value: "gzip", want: false},
		{name: "empty", value: "", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := headerValueIsVolatile(c.value); got != c.want {
				t.Fatalf("headerValueIsVolatile(%q) = %v, want %v", c.value, got, c.want)
			}
		})
	}
}

// TestTemplateChurningPath pins that per-session id segments collapse to {id}
// while real API path words are preserved, so cloud-session endpoints dedupe to
// one corpus row and the messages path is untouched.
func TestTemplateChurningPath(t *testing.T) {
	t.Parallel()
	type tc struct {
		name string
		path string
		want string
	}
	cases := []tc{
		{name: "messages untouched", path: "/v1/messages", want: "/v1/messages"},
		{name: "count_tokens untouched", path: "/v1/messages/count_tokens", want: "/v1/messages/count_tokens"},
		{name: "mcp_servers untouched", path: "/v1/mcp_servers", want: "/v1/mcp_servers"},
		{name: "event_logging untouched", path: "/api/event_logging/v2/batch", want: "/api/event_logging/v2/batch"},
		{
			name: "prefixed session id",
			path: "/v1/code/sessions/cse_01GCDZ4cWW1qo9SHkFuaPZhf/worker/events",
			want: "/v1/code/sessions/{id}/worker/events",
		},
		{
			name: "uuid session id",
			path: "/v1/code/sessions/48749d33-ea61-4b6c-8692-33dcda68a533/worker/heartbeat",
			want: "/v1/code/sessions/{id}/worker/heartbeat",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := templateChurningPath(c.path); got != c.want {
				t.Fatalf("templateChurningPath(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}
