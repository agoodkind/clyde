package statusproof_test

import (
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/daemon"
)

const (
	statusListenerStep        = "listener"
	statusListenerCountersKey = "counters"
	statusListenerPrefix      = "mitm.cli.statusproof."
	statusListenerIPv6Address = "[::1]:48799"
	statusListenerIPv4Address = "127.0.0.1:48799"

	statusListenerDaemonConfig = ingestionProofDaemonConfig + `
[mitm.cli.statusproof]
host = "localhost"
port = 48799
`
)

func statusListenerObjectStart(t *testing.T, decoder *json.Decoder, body string) {
	t.Helper()
	token, err := decoder.Token()
	if err != nil {
		t.Fatalf("step=%s operation=read_object_start err=%v body=%s", statusListenerStep, err, body)
	}
	if delimiter, isDelimiter := token.(json.Delim); !isDelimiter || delimiter != '{' {
		t.Fatalf("step=%s operation=read_object_start token=%v body=%s", statusListenerStep, token, body)
	}
}

func statusListenerObjectKey(t *testing.T, decoder *json.Decoder, body string) string {
	t.Helper()
	token, err := decoder.Token()
	if err != nil {
		t.Fatalf("step=%s operation=read_object_key err=%v body=%s", statusListenerStep, err, body)
	}
	key, isString := token.(string)
	if !isString {
		t.Fatalf("step=%s operation=read_object_key token=%v body=%s", statusListenerStep, token, body)
	}
	return key
}

func statusListenerSkipValue(t *testing.T, decoder *json.Decoder, body string) {
	t.Helper()
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("step=%s operation=read_object_value err=%v body=%s", statusListenerStep, err, body)
	}
}

func statusListenerRepeatedCounterKeys(t *testing.T, body string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	statusListenerObjectStart(t, decoder, body)
	seen := make(map[string]bool)
	var repeated []string
	for decoder.More() {
		if statusListenerObjectKey(t, decoder, body) != statusListenerCountersKey {
			statusListenerSkipValue(t, decoder, body)
			continue
		}
		statusListenerObjectStart(t, decoder, body)
		for decoder.More() {
			name := statusListenerObjectKey(t, decoder, body)
			if seen[name] {
				repeated = append(repeated, name)
			}
			seen[name] = true
			statusListenerSkipValue(t, decoder, body)
		}
		if _, err := decoder.Token(); err != nil {
			t.Fatalf("step=%s operation=read_object_end err=%v body=%s", statusListenerStep, err, body)
		}
	}
	if len(seen) == 0 {
		t.Fatalf("step=%s operation=read_counters count=0 body=%s", statusListenerStep, body)
	}
	return repeated
}

func statusListenerRepeatedTextNames(text string) []string {
	seen := make(map[string]bool)
	var repeated []string
	for line := range strings.SplitSeq(text, "\n") {
		name, _, _ := strings.Cut(line, " ")
		if seen[name] {
			repeated = append(repeated, name)
		}
		seen[name] = true
	}
	return repeated
}

func TestStatusListenerFieldsAreUniquePerAddressFamily(t *testing.T) {
	startIngestionProofDaemonWithConfig(t, statusListenerDaemonConfig)
	waitForStatusProof(t, statusListenerStep, ingestionProofPassTimeout, func(daemon.SemanticStatus) bool { return true })

	document, body := statusProofJSON(t, statusListenerStep)
	if repeated := statusListenerRepeatedCounterKeys(t, body); len(repeated) != 0 {
		t.Errorf("step=%s repeated_json_counters=%v body=%s", statusListenerStep, repeated, body)
	}
	assertStatusProofJSONValue(t, statusListenerStep, document, statusListenerPrefix+"ipv6.address", `"`+statusListenerIPv6Address+`"`)
	assertStatusProofJSONValue(t, statusListenerStep, document, statusListenerPrefix+"ipv4.address", `"`+statusListenerIPv4Address+`"`)
	assertStatusProofJSONValue(t, statusListenerStep, document, statusListenerPrefix+"ipv6.up", "false")
	assertStatusProofJSONValue(t, statusListenerStep, document, statusListenerPrefix+"ipv4.up", "false")

	text := runStatusProofCommand(t, statusListenerStep)
	if repeated := statusListenerRepeatedTextNames(text); len(repeated) != 0 {
		t.Errorf("step=%s repeated_text_names=%v body=\n%s", statusListenerStep, repeated, text)
	}
	assertStatusProofLines(t, statusListenerStep, text,
		statusListenerPrefix+"ipv6.address "+statusListenerIPv6Address,
		statusListenerPrefix+"ipv4.address "+statusListenerIPv4Address,
		statusListenerPrefix+"ipv6.up false",
		statusListenerPrefix+"ipv4.up false",
	)
}
