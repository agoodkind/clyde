package capture

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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

func TestRedactHTTPRedactsCredentialHeadersAndPreservesProviderMetadata(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Cursor-Api-Key", "cursor-secret")
	headers.Set("OpenAI-Api-Key", "openai-secret")
	headers.Set("Api-Key", "api-key-secret")
	headers.Set("X-Auth-Token", "auth-token-secret")
	headers.Set("OpenAI-Processing-Ms", "123")
	headers.Set("X-Cursor-Client-Version", "0.48")
	redactedHeaders, redactedBody := RedactHTTP(
		headers,
		[]byte(`{"safe":"cursor-secret openai-secret api-key-secret auth-token-secret 123 0.48"}`),
	)
	for _, header := range []string{"X-Cursor-Api-Key", "OpenAI-Api-Key", "Api-Key", "X-Auth-Token"} {
		if redactedHeaders.Get(header) != "" {
			t.Fatalf("sensitive header %s persisted: %v", header, redactedHeaders)
		}
	}
	for _, secret := range []string{"cursor-secret", "openai-secret", "api-key-secret", "auth-token-secret"} {
		if bytes.Contains(redactedBody, []byte(secret)) {
			t.Fatalf("redacted body leaked %q: %s", secret, redactedBody)
		}
	}
	for header, value := range map[string]string{
		"OpenAI-Processing-Ms":    "123",
		"X-Cursor-Client-Version": "0.48",
	} {
		if got := redactedHeaders.Get(header); got != value {
			t.Fatalf("provider metadata %s = %q, want %q", header, got, value)
		}
		if !bytes.Contains(redactedBody, []byte(value)) {
			t.Fatalf("redacted body removed provider metadata value %q: %s", value, redactedBody)
		}
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
		{name: "bom prefixed truncated escaped key sse", body: []byte("\xef\xbb\xbfdata: {\"access\\u005ftoken\":\"truncated-secret\"\n\n")},
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
	if bytes.Contains(redacted, []byte("nested-secret")) ||
		bytes.Contains(redacted, []byte(`"account_id":7`)) ||
		!bytes.Contains(redacted, []byte(`"safe":"kept"`)) {
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

func TestRedactHTTPFailsClosedForSensitiveStructuredScalarValues(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "top-level string", body: `"access_token=top-level-secret"`},
		{name: "JSON Lines string", body: "\"refresh_token=json-lines-secret\"\n{\"safe\":\"kept\"}"},
		{name: "object string", body: `{"safe":"api_key=object-secret"}`},
		{name: "array string", body: `["account_id=array-secret"]`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, redacted := RedactHTTP(nil, []byte(testCase.body))
			if string(redacted) != redactedValue {
				t.Fatalf("redacted body = %q, want fail-closed marker", redacted)
			}
		})
	}
}

func TestRedactHTTPFailsClosedForDuplicateJSONScalarValues(t *testing.T) {
	body := []byte(`{"safe":"api_key=first-secret","safe":"kept"}`)
	_, redacted := RedactHTTP(nil, body)
	if string(redacted) != redactedValue {
		t.Fatalf("duplicate JSON scalar body = %q, want fail-closed marker", redacted)
	}
}

func TestRedactHTTPScansJSONScalarsAfterFieldRedaction(t *testing.T) {
	body := []byte(`{"token":"field-secret","safe":"api_key=remaining-secret"}`)
	_, redacted := RedactHTTP(nil, body)
	if string(redacted) != redactedValue {
		t.Fatalf("field-redacted JSON body = %q, want fail-closed marker", redacted)
	}
}

func TestRedactHTTPFailsClosedForStructuredSSEJSONScalars(t *testing.T) {
	tests := [][]byte{
		[]byte("data: {\"safe\":\"api_key=sse-secret\"}\n\n"),
		[]byte("data: {\"outer\":[\"account_id=nested-secret\"]}\n\n"),
		[]byte("data: {\"label api_key=name-secret\":true}\n\n"),
	}
	for _, body := range tests {
		_, redacted := RedactHTTP(nil, body)
		if string(redacted) != redactedValue {
			t.Fatalf("structured SSE body = %q, want fail-closed marker", redacted)
		}
	}
}

func TestRedactHTTPScansNonDataSSELinesAfterFrameRedaction(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "comment",
			body: ": access_token=comment-secret\ndata: {\"safe\":true}\n\n",
		},
		{
			name: "event field",
			body: "event: cookie=event-secret\ndata: {\"safe\":true}\n\n",
		},
		{
			name: "Clyde token comment",
			body: ": x-clyde-token=clyde-secret\ndata: {\"safe\":true}\n\n",
		},
		{
			name: "OpenAI key event field",
			body: "event: openai-api-key=openai-secret\ndata: {\"safe\":true}\n\n",
		},
		{
			name: "AWS security token comment",
			body: ": x-amz-security-token=aws-secret\ndata: {\"safe\":true}\n\n",
		},
		{
			name: "mixed case Clyde token comment",
			body: ": X-ClYdE-ToKeN=mixed-secret\ndata: {\"safe\":true}\n\n",
		},
		{
			name: "whitespace before AWS security token assignment",
			body: "event: x-amz-security-token \t = whitespace-secret\ndata: {\"safe\":true}\n\n",
		},
		{
			name: "colon form authorization line",
			body: "event: Authorization" + ": Bearer colon-sensitive-marker\ndata: {\"safe\":true}\n\n",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, body := RedactHTTP(nil, []byte(testCase.body))
			if string(body) != redactedValue {
				t.Fatalf("redacted body = %q, want fail-closed marker", body)
			}
		})
	}
}

func TestRedactHTTPRedactsScalarSSEData(t *testing.T) {
	body := []byte("event: response.future\ndata: \"access_" + "token=scalar-sensitive-marker\"\n\n")
	_, redacted := RedactHTTP(nil, body)
	if bytes.Contains(redacted, []byte("scalar-sensitive-marker")) ||
		!bytes.Contains(redacted, []byte(`"[REDACTED]"`)) {
		t.Fatalf("scalar SSE data was not redacted: %s", redacted)
	}
}

func TestRedactHTTPPreservesSafeScalarSSEData(t *testing.T) {
	body := []byte("event: response.future\ndata: \"safe scalar\"\n\n")
	_, redacted := RedactHTTP(nil, body)
	if !bytes.Equal(redacted, body) {
		t.Fatalf("safe scalar SSE data changed:\n got: %q\nwant: %q", redacted, body)
	}
}

func TestRedactHTTPDecodesZstdCopyBeforeRedaction(t *testing.T) {
	body, err := json.Marshal(map[string]string{
		"access_" + "token": "compressed-sensitive-marker",
		"safe":              "kept",
	})
	if err != nil {
		t.Fatalf("marshal zstd body: %v", err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("create zstd encoder: %v", err)
	}
	compressed := encoder.EncodeAll(body, nil)
	encoder.Close()
	headers := http.Header{
		"Content-Encoding": {"zstd"},
		"Content-Length":   {"123"},
	}

	redactedHeaders, redacted := RedactHTTP(headers, compressed)
	if bytes.Contains(redacted, []byte("compressed-sensitive-marker")) ||
		!bytes.Contains(redacted, []byte(`"safe":"kept"`)) {
		t.Fatalf("compressed body redaction = %s", redacted)
	}
	if redactedHeaders.Get("Content-Encoding") != "" ||
		redactedHeaders.Get("Content-Length") != "" {
		t.Fatalf("decoded capture retained wire encoding headers: %v", redactedHeaders)
	}
}

func TestRedactHTTPFailsClosedForInvalidZstdCopy(t *testing.T) {
	headers := zstdFailureHeadersForTest()
	redactedHeaders, redacted := RedactHTTP(headers, []byte("not-zstd"))
	if string(redacted) != redactedValue {
		t.Fatalf("invalid zstd body = %q, want fail-closed marker", redacted)
	}
	assertZstdFailureHeadersRedacted(t, redactedHeaders)
}

func TestRedactHTTPFailsClosedForOversizedDecodedZstdCopy(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("create zstd encoder: %v", err)
	}
	compressed := encoder.EncodeAll(bytes.Repeat([]byte("x"), DefaultMaxBodyBytes+1), nil)
	encoder.Close()
	headers := zstdFailureHeadersForTest()

	redactedHeaders, redacted := RedactHTTP(headers, compressed)
	if string(redacted) != redactedValue {
		t.Fatalf("oversized decoded zstd body = %q, want fail-closed marker", redacted)
	}
	assertZstdFailureHeadersRedacted(t, redactedHeaders)
}

func zstdFailureHeadersForTest() http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer zstd-sensitive-authorization")
	headers.Set("Cookie", "session=zstd-sensitive-cookie")
	headers.Set("X-API-Key", "zstd-sensitive-api-key")
	headers.Set("ChatGPT-Account-ID", "zstd-sensitive-account")
	headers.Set("Content-Encoding", "zstd")
	headers.Set("Content-Length", "123")
	return headers
}

func assertZstdFailureHeadersRedacted(t *testing.T, headers http.Header) {
	t.Helper()
	for _, name := range []string{
		"Authorization", "Cookie", "X-API-Key", "ChatGPT-Account-ID",
		"Content-Encoding", "Content-Length",
	} {
		if headers.Get(name) != "" {
			t.Fatalf("failed zstd capture retained %s: %v", name, headers)
		}
	}
}

func TestRedactHTTPPreservesSafeSSEFrame(t *testing.T) {
	body := []byte("event: response.completed\n: keepalive\ndata: { \"safe\" : true }\n\n")
	_, redacted := RedactHTTP(nil, body)
	if !bytes.Equal(redacted, body) {
		t.Fatalf("safe SSE body changed:\n got: %q\nwant: %q", redacted, body)
	}
}

func TestRedactHTTPFailsClosedForUnsupportedContentEncoding(t *testing.T) {
	headers := http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"42"}}
	redactedHeaders, redactedBody := RedactHTTP(headers, []byte("credential-bearing-wire-bytes"))
	if string(redactedBody) != redactedValue {
		t.Fatalf("unsupported encoding body = %q", redactedBody)
	}
	if redactedHeaders.Get("Content-Encoding") != "" || redactedHeaders.Get("Content-Length") != "" {
		t.Fatalf("unsupported encoding headers = %v", redactedHeaders)
	}
}

func TestRedactHTTPRedactsBareCredentialHeaders(t *testing.T) {
	headers := http.Header{"Api-Key": {"bare-api-key"}, "Access-Token": {"bare-access-token"}}
	redactedHeaders, redactedBody := RedactHTTP(headers, []byte("bare-api-key bare-access-token"))
	if redactedHeaders.Get("Api-Key") != "" || redactedHeaders.Get("Access-Token") != "" {
		t.Fatalf("bare credential headers persisted: %v", redactedHeaders)
	}
	if bytes.Contains(redactedBody, []byte("bare-api-key")) || bytes.Contains(redactedBody, []byte("bare-access-token")) {
		t.Fatalf("bare credential values persisted: %q", redactedBody)
	}
}

func TestRedactHTTPScansValidJSONAndJSONLinesScalars(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`"id_token=json-secret"`),
		[]byte("\"safe\"\n\"token=jsonl-secret\""),
	} {
		_, redacted := RedactHTTP(nil, body)
		if string(redacted) != redactedValue {
			t.Fatalf("valid scalar body = %q", redacted)
		}
	}
}

func TestRedactHTTPRedactsFormTokenFields(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("id_token=form-secret"),
		[]byte("token=form-secret"),
	} {
		_, redacted := RedactHTTP(nil, body)
		if string(redacted) != redactedValue {
			t.Fatalf("form token body = %q", redacted)
		}
	}
}

func TestSensitiveHTTPHeaderValuesBoundsAndDeduplicatesCookies(t *testing.T) {
	var cookies strings.Builder
	for i := 0; i < maxSensitiveBodyValues*4; i++ {
		if i > 0 {
			cookies.WriteString("; ")
		}
		cookies.WriteString("session=duplicate-cookie-value")
	}
	values := SensitiveHTTPHeaderValues(http.Header{"Cookie": {cookies.String()}})
	if len(values) != 2 {
		t.Fatalf("credential values = %d, want header and one cookie value", len(values))
	}
}
