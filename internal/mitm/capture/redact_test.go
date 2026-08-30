package capture

import (
	"bytes"
	"encoding/json"
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
		{name: "truncated escaped key json", body: []byte(`{"safe":"kept","access\u005ftoken":"truncated-secret"`)},
		{name: "malformed json without marker", body: []byte(`{"safe":`)},
		{name: "malformed json scalar", body: []byte(`"unterminated`)},
		{name: "mixed valid and malformed json lines", body: []byte("{\"safe\":1}\n{\"access\\u005ftoken\":\"truncated-secret\"")},
		{name: "truncated sse", body: []byte("data: {\"account_id\":\"truncated-account\"\n\n")},
		{name: "truncated escaped key sse", body: []byte("data: {\"access\\u005ftoken\":\"truncated-secret\"\n\n")},
		{name: "malformed sse without marker", body: []byte("data: {\"safe\":\n\n")},
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

func TestRedactHTTPHandlesJSONLines(t *testing.T) {
	body := []byte("{\"access\\u005ftoken\":\"first-secret\",\"safe\":1}\n{\"account_id\":\"second-secret\",\"safe\":2}")
	_, redacted := RedactHTTP(nil, body)
	if bytes.Contains(redacted, []byte("first-secret")) || bytes.Contains(redacted, []byte("second-secret")) {
		t.Fatalf("JSON lines leaked a secret: %s", redacted)
	}
	if bytes.Equal(redacted, []byte(redactedValue)) || bytes.Count(redacted, []byte(`"safe"`)) != 2 {
		t.Fatalf("JSON lines were not preserved: %s", redacted)
	}
	lines := bytes.Split(redacted, []byte("\n"))
	if len(lines) != 2 || !json.Valid(lines[0]) || !json.Valid(lines[1]) {
		t.Fatalf("JSON lines are not two valid frames: %s", redacted)
	}
}
