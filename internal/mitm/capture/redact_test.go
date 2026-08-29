package capture

import (
	"bytes"
	"net/http"
	"testing"
)

func TestRedactHTTPCoversHeadersJSONAndSSE(t *testing.T) {
	headers := http.Header{
		"Authorization":      {"Bearer header-oauth-secret"},
		"Chatgpt-Account-Id": {"header-account-secret"},
		"Cookie":             {"session=header-cookie-secret"},
		"Content-Type":       {"application/json"},
	}
	body := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"access_token\":\"body-oauth-secret\",\"account_id\":\"body-account-secret\",\"cookie\":\"body-cookie-secret\",\"safe\":\"kept\"}\n\n")

	redactedHeaders, redactedBody := RedactHTTP(headers, body)
	for _, header := range []string{"Authorization", "Chatgpt-Account-Id", "Cookie"} {
		if redactedHeaders.Get(header) != "" {
			t.Fatalf("sensitive header %s persisted: %v", header, redactedHeaders)
		}
	}
	for _, secret := range []string{
		"header-oauth-secret", "header-account-secret", "header-cookie-secret",
		"body-oauth-secret", "body-account-secret", "body-cookie-secret",
	} {
		if bytes.Contains(redactedBody, []byte(secret)) {
			t.Fatalf("redacted body leaked %q: %s", secret, redactedBody)
		}
	}
	if !bytes.Contains(redactedBody, []byte(`"safe":"kept"`)) {
		t.Fatalf("redacted body lost safe content: %s", redactedBody)
	}
}

func TestRedactHTTPFailsClosedForMalformedSensitiveBodies(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{name: "truncated json", body: []byte(`{"safe":"kept","access_token":"truncated-secret"`)},
		{name: "truncated sse", body: []byte("data: {\"account_id\":\"truncated-account\"\n\n")},
		{name: "mixed valid and truncated sse", body: []byte("data: {\"access_token\":\"first-secret\"}\n\ndata: {\"account_id\":\"truncated-account\"\n\n")},
		{name: "plain assignment", body: []byte("cookie=session-secret")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, body := RedactHTTP(nil, testCase.body)
			if string(body) != redactedValue {
				t.Fatalf("redacted body = %q, want fail-closed marker", body)
			}
		})
	}
}

func TestRedactHTTPHandlesNestedWrongTypeFields(t *testing.T) {
	body := []byte(`{"outer":[{"access_token":{"unexpected":"nested-secret"}},{"account_id":7}],"safe":"kept"}`)
	_, redacted := RedactHTTP(nil, body)
	if bytes.Contains(redacted, []byte("nested-secret")) || !bytes.Contains(redacted, []byte(`"safe":"kept"`)) {
		t.Fatalf("nested redaction = %s", redacted)
	}
}
