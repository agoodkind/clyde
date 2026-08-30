package capture

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

const (
	redactedValue     = "[REDACTED]"
	utf8ByteOrderMark = "\xef\xbb\xbf"
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
	redactedHeaders := headers.Clone()
	sensitiveValues := make([]string, 0)
	for name, values := range headers {
		if !sensitiveHTTPHeader(name) {
			continue
		}
		for _, value := range values {
			sensitiveValues = appendSensitiveHeaderValues(sensitiveValues, name, value)
		}
		redactedHeaders.Del(name)
	}
	return redactedHeaders, redactHTTPBody(body, sensitiveValues)
}

func sensitiveHTTPHeader(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch sensitiveHeaderName(normalized) {
	case sensitiveHeaderAuthorization, sensitiveHeaderProxyAuthorization,
		sensitiveHeaderCookie, sensitiveHeaderSetCookie,
		sensitiveHeaderChatGPTAccountID, sensitiveHeaderXAPIKey,
		sensitiveHeaderClydeToken, sensitiveHeaderOpenAIIdentity,
		sensitiveHeaderAWSSecurity:
		return true
	}
	return strings.HasSuffix(normalized, "-api-key") ||
		strings.HasSuffix(normalized, "-access-token")
}

func appendSensitiveHeaderValues(values []string, name string, value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" {
		values = append(values, trimmed)
	}
	if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
		if _, token, found := strings.Cut(trimmed, " "); found && strings.TrimSpace(token) != "" {
			values = append(values, strings.TrimSpace(token))
		}
	}
	if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Set-Cookie") {
		for part := range strings.SplitSeq(trimmed, ";") {
			if _, cookieValue, found := strings.Cut(part, "="); found && strings.TrimSpace(cookieValue) != "" {
				values = append(values, strings.TrimSpace(cookieValue))
			}
		}
	}
	return values
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
		if !json.Valid(redacted) {
			if value, valid := redactJSONLines(redacted); valid {
				return value
			}
			return []byte(redactedValue)
		}
		if value, changed := redactJSONValue(redacted); changed {
			return value
		}
		return redacted
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
		redacted, changed := redactJSONValue(value)
		if !changed {
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
		redacted, payloadChanged := redactJSONValue(payload)
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
	if containsSensitiveHTTPHeaderAssignment(body) {
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
		[]byte("access_token="), []byte("refresh_token="), []byte("api_key="),
		[]byte("account_id="), []byte("account_uuid="),
	}
	for _, marker := range markers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func containsSensitiveHTTPHeaderAssignment(body []byte) bool {
	remaining := body
	for {
		assignmentIndex := bytes.IndexByte(remaining, '=')
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
