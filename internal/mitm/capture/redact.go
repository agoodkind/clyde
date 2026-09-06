package capture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	redactedValue          = "[REDACTED]"
	utf8ByteOrderMark      = "\xef\xbb\xbf"
	maxSensitiveBodyValues = 64
)

type sensitiveHeaderName string

const (
	sensitiveHeaderAuthorization      sensitiveHeaderName = "authorization"
	sensitiveHeaderProxyAuthorization sensitiveHeaderName = "proxy-authorization"
	sensitiveHeaderCookie             sensitiveHeaderName = "cookie"
	sensitiveHeaderSetCookie          sensitiveHeaderName = "set-cookie"
	sensitiveHeaderChatGPTAccountID   sensitiveHeaderName = "chatgpt-account-id"
	sensitiveHeaderXAPIKey            sensitiveHeaderName = "x-api-key"
	sensitiveHeaderClydeToken         sensitiveHeaderName = "x-clyde-token"
	sensitiveHeaderOpenAIIdentity     sensitiveHeaderName = "openai-api-" + "key"
	sensitiveHeaderAWSSecurity        sensitiveHeaderName = "x-amz-security-" + "token"
	sensitiveHeaderAPIKey             sensitiveHeaderName = "api-key"
	sensitiveHeaderXAuthToken         sensitiveHeaderName = "x-auth-" + "token"
)

type sensitiveBodyField string

const (
	sensitiveBodyAuthorization      sensitiveBodyField = "authorization"
	sensitiveBodyProxyAuthorization sensitiveBodyField = "proxyauthorization"
	sensitiveBodyAccessToken        sensitiveBodyField = "accesstoken"
	sensitiveBodyRefreshToken       sensitiveBodyField = "refreshtoken"
	sensitiveBodyIDToken            sensitiveBodyField = "idtoken"
	sensitiveBodyToken              sensitiveBodyField = "token"
	sensitiveBodyAPIKey             sensitiveBodyField = "apikey"
	sensitiveBodyXAPIKey            sensitiveBodyField = "xapikey"
	sensitiveBodyCookie             sensitiveBodyField = "cookie"
	sensitiveBodyCookies            sensitiveBodyField = "cookies"
	sensitiveBodySetCookie          sensitiveBodyField = "setcookie"
	sensitiveBodyAccountID          sensitiveBodyField = "accountid"
	sensitiveBodyAccountUUID        sensitiveBodyField = "accountuuid"
	sensitiveBodyChatGPTAccountID   sensitiveBodyField = "chatgptaccountid"
)

// RedactHTTP removes credential and account headers and masks matching values
// and sensitive JSON fields in a body copy. It never changes forwarded bytes.
func RedactHTTP(headers http.Header, body []byte) (http.Header, []byte) {
	return redactHTTP(headers, body, nil)
}

// SensitiveHTTPHeaderValues returns the bounded credential values that body
// redaction uses to remove echoed request credentials from later responses.
func SensitiveHTTPHeaderValues(headers http.Header) []string {
	values := make([]string, 0)
	for name, headerValues := range headers {
		if !sensitiveHTTPHeader(name) {
			continue
		}
		for _, value := range headerValues {
			values = appendSensitiveHeaderValues(values, name, value)
		}
	}
	return values
}

// RedactHTTPWithSensitiveValues applies the header and body redaction policy
// plus values discovered from a related request.
func RedactHTTPWithSensitiveValues(headers http.Header, body []byte, additionalValues []string) (http.Header, []byte) {
	return redactHTTP(headers, body, additionalValues)
}

func redactHTTP(headers http.Header, body []byte, additionalValues []string) (http.Header, []byte) {
	redactedHeaders := headers.Clone()
	sensitiveValues := make([]string, 0)
	for _, value := range additionalValues {
		sensitiveValues = appendSensitiveBodyValue(sensitiveValues, value)
	}
	for name, values := range headers {
		if !sensitiveHTTPHeader(name) {
			continue
		}
		for _, value := range values {
			sensitiveValues = appendSensitiveHeaderValues(sensitiveValues, name, value)
		}
		redactedHeaders.Del(name)
	}
	redactionBody := body
	contentEncoding := strings.TrimSpace(headers.Get("Content-Encoding"))
	if body != nil && contentEncoding != "" && !captureBodyUsesZstd(contentEncoding) {
		redactedHeaders.Del("Content-Encoding")
		redactedHeaders.Del("Content-Length")
		return redactedHeaders, []byte(redactedValue)
	}
	if body != nil && captureBodyUsesZstd(contentEncoding) {
		redactedHeaders.Del("Content-Encoding")
		redactedHeaders.Del("Content-Length")
		decoded, ok := decodeZstdCaptureBody(body)
		if !ok {
			return redactedHeaders, []byte(redactedValue)
		}
		redactionBody = decoded
	}
	return redactedHeaders, redactHTTPBody(redactionBody, sensitiveValues)
}

func captureBodyUsesZstd(contentEncoding string) bool {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	return encoding == "zstd" || encoding == "zstandard"
}

func decodeZstdCaptureBody(body []byte) ([]byte, bool) {
	decoder, err := zstd.NewReader(
		bytes.NewReader(body),
		zstd.WithDecoderMaxMemory(DefaultMaxBodyBytes),
	)
	if err != nil {
		return nil, false
	}
	defer decoder.Close()
	decoded, err := io.ReadAll(io.LimitReader(decoder, DefaultMaxBodyBytes+1))
	if err != nil || len(decoded) > DefaultMaxBodyBytes {
		return nil, false
	}
	return decoded, true
}

func sensitiveHTTPHeader(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch sensitiveHeaderName(normalized) {
	case sensitiveHeaderAuthorization, sensitiveHeaderProxyAuthorization,
		sensitiveHeaderCookie, sensitiveHeaderSetCookie,
		sensitiveHeaderChatGPTAccountID, sensitiveHeaderXAPIKey,
		sensitiveHeaderClydeToken, sensitiveHeaderOpenAIIdentity,
		sensitiveHeaderAWSSecurity, sensitiveHeaderAPIKey,
		sensitiveHeaderXAuthToken:
		return true
	}
	return normalized == "access-token" ||
		strings.HasSuffix(normalized, "-api-key") ||
		strings.HasSuffix(normalized, "-access-token")
}

func appendSensitiveHeaderValues(values []string, name string, value string) []string {
	trimmed := strings.TrimSpace(value)
	values = appendSensitiveBodyValue(values, trimmed)
	if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
		if _, token, found := strings.Cut(trimmed, " "); found && strings.TrimSpace(token) != "" {
			values = appendSensitiveBodyValue(values, token)
		}
	}
	if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Set-Cookie") {
		for part := range strings.SplitSeq(trimmed, ";") {
			if _, cookieValue, found := strings.Cut(part, "="); found && strings.TrimSpace(cookieValue) != "" {
				values = appendSensitiveBodyValue(values, cookieValue)
			}
		}
	}
	return values
}

func appendSensitiveBodyValue(values []string, value string) []string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 3 || len(values) >= maxSensitiveBodyValues {
		return values
	}
	if slices.Contains(values, trimmed) {
		return values
	}
	return append(values, trimmed)
}

func redactHTTPBody(body []byte, sensitiveValues []string) []byte {
	redacted := bytes.TrimPrefix(bytes.Clone(body), []byte(utf8ByteOrderMark))
	for _, value := range sensitiveValues {
		if len(value) < 3 {
			continue
		}
		redacted = bytes.ReplaceAll(redacted, []byte(value), []byte(redactedValue))
	}
	if looksLikeJSONValue(redacted) {
		return redactJSONCaptureBody(redacted)
	}
	if value, handled := redactSSEJSON(redacted); handled {
		if containsSensitiveSSENonDataMarker(value) {
			return []byte(redactedValue)
		}
		return value
	}
	if containsSensitiveBodyMarker(redacted) {
		return []byte(redactedValue)
	}
	return redacted
}

func redactJSONCaptureBody(body []byte) []byte {
	if json.Valid(body) {
		return redactValidJSONCaptureBody(body)
	}
	if value, valid := redactJSONLines(body); valid {
		return value
	}
	return []byte(redactedValue)
}

func redactValidJSONCaptureBody(body []byte) []byte {
	if containsSensitiveJSONScalar(body) {
		return []byte(redactedValue)
	}
	value, changed := redactJSONValue(body)
	if changed {
		if containsSensitiveJSONScalar(value) {
			return []byte(redactedValue)
		}
		return value
	}
	if containsSensitiveBodyMarker(body) {
		return []byte(redactedValue)
	}
	return body
}

func looksLikeJSONValue(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '{', '[', '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 't', 'f', 'n':
		return true
	default:
		return false
	}
}

func redactJSONLines(body []byte) ([]byte, bool) {
	lines := bytes.SplitAfter(body, []byte("\n"))
	valueCount := 0
	for index, line := range lines {
		value := bytes.TrimSpace(line)
		if len(value) == 0 {
			continue
		}
		if !json.Valid(value) {
			return body, false
		}
		valueCount++
		if containsSensitiveJSONScalar(value) {
			return []byte(redactedValue), true
		}
		redacted, changed := redactJSONValue(value)
		if containsSensitiveJSONScalar(redacted) {
			return []byte(redactedValue), true
		}
		if !changed {
			if containsSensitiveBodyMarker(value) {
				return []byte(redactedValue), true
			}
			continue
		}
		before, after, found := bytes.Cut(line, value)
		if !found {
			return body, false
		}
		lines[index] = bytes.Join([][]byte{before, redacted, after}, nil)
	}
	if valueCount < 2 {
		return body, false
	}
	return bytes.Join(lines, nil), true
}

func containsSensitiveJSONScalar(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if containsSensitiveJSONValue(decoder) {
		return true
	}
	return decoder.More()
}

func containsSensitiveJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return true
	}
	if value, ok := token.(string); ok {
		return containsSensitiveBodyMarker([]byte(value))
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return false
	}
	switch delimiter {
	case '{':
		return containsSensitiveJSONObject(decoder)
	case '[':
		return containsSensitiveJSONArray(decoder)
	default:
		return true
	}
}

func containsSensitiveJSONObject(decoder *json.Decoder) bool {
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return true
		}
		name, ok := key.(string)
		if !ok || containsSensitiveBodyMarker([]byte(name)) {
			return true
		}
		if containsSensitiveJSONValue(decoder) {
			return true
		}
	}
	return consumesJSONClosingDelimiter(decoder, '}')
}

func containsSensitiveJSONArray(decoder *json.Decoder) bool {
	for decoder.More() {
		if containsSensitiveJSONValue(decoder) {
			return true
		}
	}
	return consumesJSONClosingDelimiter(decoder, ']')
}

func consumesJSONClosingDelimiter(decoder *json.Decoder, expected json.Delim) bool {
	closing, err := decoder.Token()
	return err != nil || closing != expected
}

func redactJSONValue(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, false
	}
	switch trimmed[0] {
	case '{':
		return redactJSONObject(raw)
	case '[':
		return redactJSONArray(raw)
	default:
		return raw, false
	}
}

func redactJSONObject(raw []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw, false
	}
	changed := false
	for name, value := range fields {
		if sensitiveJSONField(name) {
			fields[name] = json.RawMessage(`"` + redactedValue + `"`)
			changed = true
			continue
		}
		if redacted, nestedChanged := redactJSONValue(value); nestedChanged {
			fields[name] = redacted
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func redactJSONArray(raw []byte) ([]byte, bool) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw, false
	}
	changed := false
	for index, item := range items {
		if redacted, nestedChanged := redactJSONValue(item); nestedChanged {
			items[index] = redacted
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func sensitiveJSONField(name string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(strings.ToLower(name))
	switch sensitiveBodyField(normalized) {
	case sensitiveBodyAuthorization, sensitiveBodyProxyAuthorization,
		sensitiveBodyAccessToken, sensitiveBodyRefreshToken, sensitiveBodyIDToken,
		sensitiveBodyToken, sensitiveBodyAPIKey, sensitiveBodyXAPIKey,
		sensitiveBodyCookie, sensitiveBodyCookies, sensitiveBodySetCookie,
		sensitiveBodyAccountID, sensitiveBodyAccountUUID, sensitiveBodyChatGPTAccountID:
		return true
	}
	return false
}

func redactSSEJSON(body []byte) ([]byte, bool) {
	lines := bytes.SplitAfter(body, []byte("\n"))
	changed := false
	handled := false
	for index, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		handled = true
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if !json.Valid(payload) {
			return []byte(redactedValue), true
		}
		if bytes.HasPrefix(payload, []byte(`"`)) {
			redacted, payloadChanged := redactSensitiveSSEScalar(payload)
			if !payloadChanged {
				continue
			}
			before, after, found := bytes.Cut(line, payload)
			if !found {
				continue
			}
			lines[index] = bytes.Join([][]byte{before, redacted, after}, nil)
			changed = true
			continue
		}
		if containsSensitiveJSONScalar(payload) {
			return []byte(redactedValue), true
		}
		redacted, payloadChanged := redactJSONValue(payload)
		if containsSensitiveJSONScalar(redacted) {
			return []byte(redactedValue), true
		}
		if !payloadChanged {
			continue
		}
		before, after, found := bytes.Cut(line, payload)
		if !found {
			continue
		}
		lines[index] = bytes.Join([][]byte{before, redacted, after}, nil)
		changed = true
	}
	if !changed {
		return body, handled
	}
	return bytes.Join(lines, nil), true
}

func redactSensitiveSSEScalar(payload []byte) ([]byte, bool) {
	var value string
	if json.Unmarshal(payload, &value) != nil ||
		!containsSensitiveBodyMarker([]byte(value)) {
		return payload, false
	}
	return []byte(`"` + redactedValue + `"`), true
}

func containsSensitiveSSENonDataMarker(body []byte) bool {
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		if containsSensitiveBodyMarker(trimmed) {
			return true
		}
	}
	return false
}

func containsSensitiveBodyMarker(body []byte) bool {
	if containsSensitiveHTTPHeaderAssignment(body, '=') ||
		containsSensitiveHTTPHeaderAssignment(body, ':') {
		return true
	}
	lower := bytes.ToLower(body)
	markers := [][]byte{
		[]byte(`"authorization"`), []byte(`"proxy-authorization"`),
		[]byte(`"access_token"`), []byte(`"accesstoken"`),
		[]byte(`"refresh_token"`), []byte(`"refreshtoken"`),
		[]byte(`"id_token"`), []byte(`"idtoken"`), []byte(`"token"`),
		[]byte(`"api_key"`), []byte(`"apikey"`), []byte(`"x-api-key"`),
		[]byte(`"cookie"`), []byte(`"cookies"`), []byte(`"set-cookie"`),
		[]byte(`"account_id"`), []byte(`"accountid"`),
		[]byte(`"account_uuid"`), []byte(`"accountuuid"`),
		[]byte(`"chatgpt-account-id"`),
		[]byte("access_token="), []byte("refresh_token="), []byte("id_token="),
		[]byte("token="), []byte("api_key="),
		[]byte("account_id="), []byte("account_uuid="),
	}
	for _, marker := range markers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func containsSensitiveHTTPHeaderAssignment(body []byte, separator byte) bool {
	remaining := body
	for {
		assignmentIndex := bytes.IndexByte(remaining, separator)
		if assignmentIndex < 0 {
			return false
		}
		nameEnd := assignmentIndex
		for nameEnd > 0 && (remaining[nameEnd-1] == ' ' || remaining[nameEnd-1] == '\t') {
			nameEnd--
		}
		nameStart := nameEnd
		for nameStart > 0 && sensitiveHTTPHeaderNameByte(remaining[nameStart-1]) {
			nameStart--
		}
		if nameStart < nameEnd && sensitiveHTTPHeader(string(remaining[nameStart:nameEnd])) {
			return true
		}
		remaining = remaining[assignmentIndex+1:]
	}
}

func sensitiveHTTPHeaderNameByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-'
}
