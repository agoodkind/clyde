package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

func TestRedactHTTPRedactsNoncanonicalAuthTokenAndClientSecret(t *testing.T) {
	headers := http.Header{"x-auth-token": {"header-secret"}}
	_, redactedBody := RedactHTTP(headers, []byte(`{"client_secret":"body-secret","safe":"kept"}`))
	if _, exists := headers["x-auth-token"]; !exists {
		t.Fatal("test header lost before redaction")
	}
	redactedHeaders, _ := RedactHTTP(headers, nil)
	if _, exists := redactedHeaders["x-auth-token"]; exists {
		t.Fatalf("noncanonical auth token persisted: %v", redactedHeaders)
	}
	if bytes.Contains(redactedBody, []byte("body-secret")) {
		t.Fatalf("client secret persisted: %s", redactedBody)
	}
}

func TestRedactHTTPFailsClosedForShortHeaderCredential(t *testing.T) {
	headers := http.Header{"X-Auth-Token": {"xy"}}
	_, redactedBody := RedactHTTP(headers, []byte(`{"echo":"xy","safe":"kept"}`))
	if string(redactedBody) != redactedValue {
		t.Fatalf("short credential body = %q, want %q", redactedBody, redactedValue)
	}
}

func TestRedactHTTPRedactsNestedScalarMarker(t *testing.T) {
	_, redactedBody := RedactHTTP(nil, []byte(`{"outer":{"note":"token=secret","safe":"kept"}}`))
	if bytes.Contains(redactedBody, []byte("token=secret")) {
		t.Fatalf("nested scalar marker persisted: %s", redactedBody)
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

func TestRedactHTTPRedactsCredentialInCRDelimitedSSE(t *testing.T) {
	headers := http.Header{"Authorization": {"Bearer cr-sensitive-value"}}
	body := []byte("event: response.future\rdata: {\"safe\":true}\r\revent: response.future\rdata: \"\\u0063r-sensitive-value\"\r\r")
	_, redacted := RedactHTTP(headers, body)
	if bytes.Contains(redacted, []byte("cr-sensitive-value")) || bytes.Contains(redacted, []byte(`\u0063r-sensitive-value`)) {
		t.Fatalf("CR-delimited credential survived: %q", redacted)
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

func TestRedactHTTPRedactsPasswordFields(t *testing.T) {
	fieldName := "pass" + "word"
	jsonSecret := "body-sensitive-marker"
	formSecret := "form-sensitive-marker"
	for _, body := range [][]byte{
		[]byte(`{"` + fieldName + `":"` + jsonSecret + `","safe":"kept"}`),
		[]byte(fieldName + "=" + formSecret),
	} {
		_, redacted := RedactHTTP(nil, body)
		if bytes.Contains(redacted, []byte(jsonSecret)) || bytes.Contains(redacted, []byte(formSecret)) {
			t.Fatalf("password persisted: %s", redacted)
		}
	}
}

func TestRedactHTTPRedactsClientSecretFormField(t *testing.T) {
	fieldName := "client_" + "secret"
	_, redacted := RedactHTTP(nil, []byte(fieldName+"=form-sensitive-marker"))
	if string(redacted) != redactedValue {
		t.Fatalf("client secret form body = %q", redacted)
	}
}

func TestRedactHTTPRedactsEscapedCredentialEcho(t *testing.T) {
	headers := http.Header{"Authorization": {"Bearer secret"}}
	_, redacted := RedactHTTP(headers, []byte(`{"echo":"\u0073ecret","safe":"kept"}`))
	var decoded map[string]string
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("decode redacted body: %v", err)
	}
	if decoded["echo"] != redactedValue || decoded["safe"] != "kept" {
		t.Fatalf("escaped echo redaction = %#v", decoded)
	}
}

func TestRedactHTTPFailsClosedForShortCookieComponent(t *testing.T) {
	headers := http.Header{"Cookie": {"a=xy"}}
	_, redacted := RedactHTTP(headers, []byte(`{"echo":"xy","safe":"kept"}`))
	if string(redacted) != redactedValue {
		t.Fatalf("short cookie body = %q", redacted)
	}
}

func TestRedactHTTPRedactsAuthTokenField(t *testing.T) {
	_, redacted := RedactHTTP(nil, []byte(`{"auth_token":"secret","safe":"kept"}`))
	if bytes.Contains(redacted, []byte("secret")) || !bytes.Contains(redacted, []byte("kept")) {
		t.Fatalf("auth token field was not redacted: %s", redacted)
	}
}

func TestRedactHTTPIgnoresSetCookieAttributes(t *testing.T) {
	for _, path := range []string{"/", "/api"} {
		t.Run(path, func(t *testing.T) {
			headers := http.Header{"Set-Cookie": {"sid=abcdef; Path=" + path}}
			body := []byte(`{"path":` + strconv.Quote(path) + `,"safe":"kept"}`)
			_, redacted := RedactHTTP(headers, body)
			if !bytes.Equal(redacted, body) {
				t.Fatalf("Set-Cookie attribute changed safe body: got %s want %s", redacted, body)
			}
		})
	}
	headers := http.Header{"Set-Cookie": {"sid=abcdef; Path=/"}}
	_, redacted := RedactHTTP(headers, []byte(`{"echo":"abcdef","safe":"kept"}`))
	if bytes.Contains(redacted, []byte("abcdef")) {
		t.Fatalf("Set-Cookie value survived: %s", redacted)
	}
}

func TestRedactHTTPUnquotesCookieValuesForBodyRedaction(t *testing.T) {
	headers := http.Header{"Cookie": {`session="secret"`}}
	_, redacted := RedactHTTP(headers, []byte(`{"echo":"secret","safe":"kept"}`))
	var decoded map[string]string
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("decode redacted body: %v", err)
	}
	if decoded["echo"] != redactedValue || decoded["safe"] != "kept" {
		t.Fatalf("quoted cookie redaction = %#v", decoded)
	}
}

func TestRedactHTTPPreservesSafePlainText(t *testing.T) {
	body := []byte("not found")
	headers := http.Header{"Content-Length": {strconv.Itoa(len(body))}}
	redactedHeaders, redacted := RedactHTTP(headers, body)
	if !bytes.Equal(redacted, body) {
		t.Fatalf("safe text changed: got %q want %q", redacted, body)
	}
	if redactedHeaders.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("safe content length = %q, want preserved", redactedHeaders.Get("Content-Length"))
	}
}

func TestRedactHTTPDeletesContentLengthWhenBodyChanges(t *testing.T) {
	headers := http.Header{"Content-Length": {"57"}}
	redactedHeaders, redacted := RedactHTTP(headers, []byte(`{"access_token":"secret","safe":"kept"}`))
	if redactedHeaders.Get("Content-Length") != "" {
		t.Fatalf("changed body retained content length: %v", redactedHeaders)
	}
	if bytes.Contains(redacted, []byte("secret")) {
		t.Fatalf("changed body retained secret: %s", redacted)
	}
}

func TestRedactHTTPPreservesSafeNonJSONSSEData(t *testing.T) {
	body := []byte("event: response.future\ndata: hello\n\n")
	_, redacted := RedactHTTP(nil, body)
	if !bytes.Equal(redacted, body) {
		t.Fatalf("safe SSE changed: got %q want %q", redacted, body)
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
	values, complete := SensitiveHTTPHeaderValuesWithStatus(http.Header{"Cookie": {cookies.String()}})
	if len(values) != 2 {
		t.Fatalf("credential values = %d, want header and one cookie value", len(values))
	}
	if !complete {
		t.Fatal("duplicate credential values incorrectly exhausted the collection bound")
	}
}

func TestSensitiveHTTPHeaderValuesFailsClosedAfterUniqueCookieCap(t *testing.T) {
	var cookies strings.Builder
	lastValue := ""
	for i := 0; i < maxSensitiveBodyValues; i++ {
		if i > 0 {
			cookies.WriteString("; ")
		}
		lastValue = fmt.Sprintf("cookie-value-%03d-suffix", i)
		fmt.Fprintf(&cookies, "cookie%d=%s", i, lastValue)
	}
	values, complete := SensitiveHTTPHeaderValuesWithStatus(http.Header{"Cookie": {cookies.String()}})
	if complete {
		t.Fatal("unique credential values unexpectedly fit within the collection bound")
	}
	if len(values) != maxSensitiveBodyValues {
		t.Fatalf("credential values = %d, want %d", len(values), maxSensitiveBodyValues)
	}
	body := []byte(`{"echo":"` + lastValue + `","safe":"kept"}`)
	_, redacted := RedactHTTPWithSensitiveValuesStatus(nil, body, values, complete)
	if string(redacted) != redactedValue {
		t.Fatalf("incomplete collection body = %q, want fail-closed marker", redacted)
	}
}

func TestSensitiveHTTPHeaderValuesMarksShortCredentialsIncomplete(t *testing.T) {
	values, complete := SensitiveHTTPHeaderValuesWithStatus(http.Header{"X-API-Key": {"xy"}})
	if complete {
		t.Fatal("short credential collection reported complete")
	}
	_, redacted := RedactHTTPWithSensitiveValuesStatus(nil, []byte(`{"echo":"xy"}`), values, complete)
	if string(redacted) != redactedValue {
		t.Fatalf("short credential echo = %q, want fail-closed marker", redacted)
	}
}

func TestRedactHTTPBoundsAdditionalSensitiveValues(t *testing.T) {
	values := make([]string, maxSensitiveBodyValues+1)
	for index := range values {
		values[index] = fmt.Sprintf("additional-value-%03d", index)
	}
	lastValue := values[len(values)-1]
	_, redacted := RedactHTTPWithSensitiveValuesStatus(
		nil,
		[]byte(`{"echo":`+strconv.Quote(lastValue)+`}`),
		values,
		true,
	)
	if string(redacted) != redactedValue {
		t.Fatalf("overflowed additional values = %q, want fail-closed marker", redacted)
	}
	duplicates := make([]string, maxSensitiveBodyValues+1)
	for index := range duplicates {
		duplicates[index] = "duplicate-additional-value"
	}
	safeBody := []byte(`{"safe":"kept"}`)
	_, redacted = RedactHTTPWithSensitiveValuesStatus(nil, safeBody, duplicates, true)
	if !bytes.Equal(redacted, safeBody) {
		t.Fatalf("duplicate additional values changed safe body: %s", redacted)
	}
}

func TestRedactHTTPNormalizesSensitiveAssignmentNames(t *testing.T) {
	for _, name := range []string{
		"api_key", "api-key", "apikey", "apiKey",
		"client_secret", "client-secret", "clientsecret", "clientSecret",
		"access_token", "access-token", "accesstoken", "accessToken",
		"refresh_token", "refresh-token", "refreshtoken", "refreshToken",
		"id_token", "id-token", "idtoken", "idToken",
		"password", "account_id", "accountId", "account_uuid", "accountUuid",
	} {
		t.Run(name, func(t *testing.T) {
			_, redacted := RedactHTTP(nil, []byte(name+"=secret"))
			if string(redacted) != redactedValue {
				t.Fatalf("assignment %q was not redacted: %q", name, redacted)
			}
		})
	}
}
