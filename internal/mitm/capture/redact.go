package capture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
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
	sensitiveBodyPassword           sensitiveBodyField = "password"
)

// RedactHTTP removes credential and account headers and masks matching values
// and sensitive JSON fields in a body copy. It never changes forwarded bytes.
func RedactHTTP(headers http.Header, body []byte) (http.Header, []byte) {
	return redactHTTP(headers, body, nil, true)
}

// SensitiveHTTPHeaderValuesWithStatus also reports whether every distinct
// credential value fit within the collection bound.
func SensitiveHTTPHeaderValuesWithStatus(headers http.Header) ([]string, bool) {
	values := make([]string, 0)
	complete := true
	for name, headerValues := range headers {
		if !sensitiveHTTPHeader(name) {
			continue
		}
		for _, value := range headerValues {
			if containsShortSensitiveHeaderValue(name, value) {
				complete = false
			}
			var valueComplete bool
			values, valueComplete = appendSensitiveHeaderValuesWithStatus(values, name, value)
			complete = complete && valueComplete
		}
	}
	return values, complete
}

// RedactHTTPWithSensitiveValuesStatus fails closed for a related nonnil body
// when bounded collection could not retain every distinct sensitive value.
func RedactHTTPWithSensitiveValuesStatus(headers http.Header, body []byte, additionalValues []string, additionalValuesComplete bool) (http.Header, []byte) {
	return redactHTTP(headers, body, additionalValues, additionalValuesComplete)
}

func redactHTTP(headers http.Header, body []byte, additionalValues []string, additionalValuesComplete bool) (http.Header, []byte) {
	redactedHeaders := headers.Clone()
	sensitiveValues := make([]string, 0)
	sensitiveValuesComplete := additionalValuesComplete
	shortSensitiveValue := false
	for _, value := range additionalValues {
		if trimmed := strings.TrimSpace(value); trimmed != "" && len(trimmed) < 3 {
			shortSensitiveValue = true
		}
		var valueComplete bool
		sensitiveValues, valueComplete = appendSensitiveBodyValueWithStatus(sensitiveValues, value)
		sensitiveValuesComplete = sensitiveValuesComplete && valueComplete
	}
	for name, values := range headers {
		if !sensitiveHTTPHeader(name) {
			continue
		}
		for _, value := range values {
			if containsShortSensitiveHeaderValue(name, value) {
				shortSensitiveValue = true
			}
			var valueComplete bool
			sensitiveValues, valueComplete = appendSensitiveHeaderValuesWithStatus(sensitiveValues, name, value)
			sensitiveValuesComplete = sensitiveValuesComplete && valueComplete
		}
		delete(redactedHeaders, name)
	}
	if body != nil && (!sensitiveValuesComplete || shortSensitiveValue) {
		redactedHeaders.Del("Content-Length")
		return redactedHeaders, []byte(redactedValue)
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
	redactedBody := redactHTTPBody(redactionBody, sensitiveValues)
	if !bytes.Equal(redactedBody, body) {
		redactedHeaders.Del("Content-Length")
	}
	return redactedHeaders, redactedBody
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
	return normalized == "access-token" || normalized == "refresh-token" || normalized == "id-token" ||
		strings.HasSuffix(normalized, "-api-key") ||
		strings.HasSuffix(normalized, "-access-token") ||
		strings.HasSuffix(normalized, "-refresh-token") ||
		strings.HasSuffix(normalized, "-id-token") ||
		strings.HasSuffix(normalized, "-auth-token")
}

func appendSensitiveHeaderValuesWithStatus(values []string, name string, value string) ([]string, bool) {
	trimmed := strings.TrimSpace(value)
	complete := true
	var valueComplete bool
	values, valueComplete = appendSensitiveBodyValueWithStatus(values, trimmed)
	complete = complete && valueComplete
	if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
		if _, token, found := strings.Cut(trimmed, " "); found && strings.TrimSpace(token) != "" {
			values, valueComplete = appendSensitiveBodyCandidateWithStatus(values, token)
			complete = complete && valueComplete
		}
	}
	if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Set-Cookie") {
		for _, part := range sensitiveCookieParts(name, trimmed) {
			if _, cookieValue, found := strings.Cut(part, "="); found && strings.TrimSpace(cookieValue) != "" {
				values, valueComplete = appendSensitiveBodyCandidateWithStatus(values, unquotedSensitiveCookieValue(cookieValue))
				complete = complete && valueComplete
			}
		}
	}
	return values, complete
}

func appendSensitiveBodyCandidateWithStatus(values []string, value string) ([]string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || slices.Contains(values, trimmed) {
		return values, true
	}
	if len(values) >= maxSensitiveBodyValues {
		return values, false
	}
	return append(values, trimmed), true
}

func containsShortSensitiveHeaderValue(name string, value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" && len(trimmed) < 3 {
		return true
	}
	if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
		_, token, found := strings.Cut(trimmed, " ")
		return found && strings.TrimSpace(token) != "" && len(strings.TrimSpace(token)) < 3
	}
	if !strings.EqualFold(name, "Cookie") && !strings.EqualFold(name, "Set-Cookie") {
		return false
	}
	for _, part := range sensitiveCookieParts(name, trimmed) {
		_, cookieValue, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		cookieValue = unquotedSensitiveCookieValue(cookieValue)
		if cookieValue != "" && len(cookieValue) < 3 {
			return true
		}
	}
	return false
}

func sensitiveCookieParts(name string, value string) []string {
	parts := strings.Split(value, ";")
	if strings.EqualFold(name, "Set-Cookie") && len(parts) > 1 {
		return parts[:1]
	}
	return parts
}

func unquotedSensitiveCookieValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if unquoted, err := strconv.Unquote(trimmed); err == nil {
		return unquoted
	}
	return trimmed
}

func appendSensitiveBodyValueWithStatus(values []string, value string) ([]string, bool) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 3 || slices.Contains(values, trimmed) {
		return values, true
	}
	if len(values) >= maxSensitiveBodyValues {
		return values, false
	}
	return append(values, trimmed), true
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
		return redactJSONCaptureBody(redacted, sensitiveValues)
	}
	if value, handled := redactSSEJSON(redacted, sensitiveValues); handled {
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

func redactJSONCaptureBody(body []byte, sensitiveValues []string) []byte {
	if json.Valid(body) {
		return redactValidJSONCaptureBody(body, sensitiveValues)
	}
	if value, valid := redactJSONLines(body, sensitiveValues); valid {
		return value
	}
	return []byte(redactedValue)
}

func redactValidJSONCaptureBody(body []byte, sensitiveValues []string) []byte {
	if containsSensitiveJSONScalar(body, sensitiveValues) {
		return []byte(redactedValue)
	}
	value, changed := redactJSONValue(body)
	if changed {
		if containsSensitiveJSONScalar(value, sensitiveValues) {
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
	case '{', '[', '"':
		return true
	default:
		return json.Valid(trimmed)
	}
}

func redactJSONLines(body []byte, sensitiveValues []string) ([]byte, bool) {
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
		if containsSensitiveJSONScalar(value, sensitiveValues) {
			return []byte(redactedValue), true
		}
		redacted, changed := redactJSONValue(value)
		if containsSensitiveJSONScalar(redacted, sensitiveValues) {
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

func containsSensitiveJSONScalar(raw []byte, sensitiveValues []string) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if containsSensitiveJSONValue(decoder, sensitiveValues) {
		return true
	}
	return decoder.More()
}

func containsSensitiveJSONValue(decoder *json.Decoder, sensitiveValues []string) bool {
	token, err := decoder.Token()
	if err != nil {
		return true
	}
	if value, ok := token.(string); ok {
		return containsSensitiveBodyMarker([]byte(value)) || containsSensitiveValue(value, sensitiveValues)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return false
	}
	switch delimiter {
	case '{':
		return containsSensitiveJSONObject(decoder, sensitiveValues)
	case '[':
		return containsSensitiveJSONArray(decoder, sensitiveValues)
	default:
		return true
	}
}

func containsSensitiveJSONObject(decoder *json.Decoder, sensitiveValues []string) bool {
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return true
		}
		name, ok := key.(string)
		if !ok ||
			containsSensitiveBodyMarker([]byte(name)) ||
			containsSensitiveValue(name, sensitiveValues) {
			return true
		}
		if containsSensitiveJSONValue(decoder, sensitiveValues) {
			return true
		}
	}
	return consumesJSONClosingDelimiter(decoder, '}')
}

func containsSensitiveJSONArray(decoder *json.Decoder, sensitiveValues []string) bool {
	for decoder.More() {
		if containsSensitiveJSONValue(decoder, sensitiveValues) {
			return true
		}
	}
	return consumesJSONClosingDelimiter(decoder, ']')
}

func containsSensitiveValue(value string, sensitiveValues []string) bool {
	for _, sensitiveValue := range sensitiveValues {
		if len(sensitiveValue) >= 3 && strings.Contains(value, sensitiveValue) {
			return true
		}
	}
	return false
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
		if containsSensitiveBodyMarker(value) {
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
		sensitiveBodyToken, sensitiveBodyField("authtoken"), sensitiveBodyAPIKey, sensitiveBodyXAPIKey,
		sensitiveBodyCookie, sensitiveBodyCookies, sensitiveBodySetCookie,
		sensitiveBodyAccountID, sensitiveBodyAccountUUID, sensitiveBodyChatGPTAccountID,
		sensitiveBodyField("clientsecret"), sensitiveBodyPassword:
		return true
	}
	return false
}

func redactSSEJSON(body []byte, sensitiveValues []string) ([]byte, bool) {
	lines := splitCaptureLines(body)
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
			if looksLikeJSONValue(payload) || containsSensitiveBodyMarker(payload) {
				return []byte(redactedValue), true
			}
			continue
		}
		if bytes.HasPrefix(payload, []byte(`"`)) {
			redacted, payloadChanged := redactSensitiveSSEScalar(payload, sensitiveValues)
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
		if containsSensitiveJSONScalar(payload, sensitiveValues) {
			return []byte(redactedValue), true
		}
		redacted, payloadChanged := redactJSONValue(payload)
		if containsSensitiveJSONScalar(redacted, sensitiveValues) {
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

func redactSensitiveSSEScalar(payload []byte, sensitiveValues []string) ([]byte, bool) {
	var value string
	if json.Unmarshal(payload, &value) != nil ||
		!containsSensitiveBodyMarker([]byte(value)) && !containsSensitiveValue(value, sensitiveValues) {
		return payload, false
	}
	return []byte(`"` + redactedValue + `"`), true
}

func containsSensitiveSSENonDataMarker(body []byte) bool {
	for _, line := range splitCaptureLines(body) {
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

func splitCaptureLines(body []byte) [][]byte {
	lines := make([][]byte, 0)
	lineStart := 0
	for index := 0; index < len(body); index++ {
		if body[index] != '\r' && body[index] != '\n' {
			continue
		}
		lineEnd := index + 1
		if body[index] == '\r' && lineEnd < len(body) && body[lineEnd] == '\n' {
			lineEnd++
			index++
		}
		lines = append(lines, body[lineStart:lineEnd])
		lineStart = lineEnd
	}
	if lineStart < len(body) {
		lines = append(lines, body[lineStart:])
	}
	return lines
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
		[]byte(`"password"`), []byte(`"client_secret"`),
		[]byte(`"api_key"`), []byte(`"apikey"`), []byte(`"x-api-key"`),
		[]byte(`"cookie"`), []byte(`"cookies"`), []byte(`"set-cookie"`),
		[]byte(`"account_id"`), []byte(`"accountid"`),
		[]byte(`"account_uuid"`), []byte(`"accountuuid"`),
		[]byte(`"chatgpt-account-id"`),
		[]byte("access_token="), []byte("refresh_token="), []byte("id_token="),
		[]byte("token="), []byte("api_key="), []byte("password="), []byte("client_secret="),
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
		if nameStart < nameEnd {
			name := string(remaining[nameStart:nameEnd])
			if sensitiveHTTPHeader(name) || sensitiveJSONField(name) {
				return true
			}
		}
		remaining = remaining[assignmentIndex+1:]
	}
}

func sensitiveHTTPHeaderNameByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '_' || value == '.'
}
