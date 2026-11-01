package localbackendcli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorienttag"
)

const (
	historyProbeUser      = "historyprobe retained user chat"
	historyProbeAssistant = "historyprobe retained assistant chat"
	historyProbeTool      = "HISTORYPROBE_TOOL_SENTINEL"
	historyProbeSummary   = "historyprobe summary before the transcript"
	historyProbeLaterUser = "historyprobe later user question"
	historyProbeLater     = "historyprobe later answer"
	historyProbePrompt    = "historyprobe compaction prompt"
	historyProbeSession   = "019e0000-3a00-7010-bd9f-a6ee715591"
	historyProbeConfig    = "[mitm.capture_store]\ndb_path = %q\n"
	historyProbeBaseTime  = "2026-07-01T11:00:02.000Z"
	historyProbeUserTail  = "retained user chat"

	historyProbeTruncatedBytes = 200
)

// historyProbeCase stores one saved assistant item, its capture exchanges, its
// prior rollout items, the expected export outcome, and the chat text that a
// successful chat export must contain.
type historyProbeCase struct {
	name     string
	session  string
	itemID   string
	item     string
	prior    []string
	exchange []historyProbeExchange
	outcome  historyProbeOutcome
	chat     []string
}

type historyProbeExchange struct {
	requestID string
	request   string
	upstream  string
	client    string
	truncated bool
}

type historyProbeOutcome int

const (
	historyProbeRecovered historyProbeOutcome = iota
	historyProbeOrdinary
	historyProbeUnresolved
)

func historyProbeMessage(role string, kind string, text string, t *testing.T) string {
	t.Helper()
	return `{"type":"message","role":"` + role + `","content":[{"type":"` + kind + `","text":` + injectedProbeJSON(t, text) + `}]}`
}

func historyProbeTranscriptItems(t *testing.T) []string {
	t.Helper()
	return []string{
		historyProbeMessage("user", "input_text", "historyprobe oldest message", t),
		historyProbeMessage("user", "input_text", historyProbeUser, t),
		`{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo ` + historyProbeTool + `\"}","call_id":"call-history"}`,
		`{"type":"function_call_output","call_id":"call-history","output":"` + historyProbeTool + ` output"}`,
		historyProbeMessage("assistant", "output_text", historyProbeAssistant, t),
	}
}

func historyProbeLegacyInjection() string {
	return historyProbeUserInjection(historyProbeUser)
}

func historyProbeUserInjection(userText string) string {
	retained := "### User\n\n" + userText + "\n\n" + historyProbeToolTranscript("{\"cmd\":\"echo "+historyProbeTool+"\"}")
	return reorienttag.WrapInjection(retained, "")
}

func historyProbeAtomicInjection(arguments string) string {
	return reorienttag.WrapInjection(historyProbeToolTranscript(arguments), "")
}

func historyProbeToolTranscript(arguments string) string {
	return "### Assistant\n\n" + arguments + "\n\n### Assistant\n\n" + historyProbeTool + " output\n\n### Assistant\n\n" + historyProbeAssistant
}

// historyProbeMarkedItem returns an assistant item with an ordinary summary
// part and a transcript part that producer metadata marks.
func historyProbeMarkedItem(t *testing.T, id string, summary string, injection string) string {
	t.Helper()
	return `{"id":"` + id + `","type":"message","role":"assistant","internal_chat_message_metadata_passthrough":{"content_item_kinds":[null,"` + codexstore.InjectedTranscriptContentKind + `"]},"content":[` +
		`{"type":"output_text","annotations":[],"text":` + injectedProbeJSON(t, summary) + `},` +
		`{"type":"output_text","annotations":[],"text":` + injectedProbeJSON(t, injection) + `}]}`
}

func historyProbeRequest(t *testing.T, items []string) string {
	t.Helper()
	all := append(append([]string(nil), items...), historyProbeMessage("user", "input_text", historyProbePrompt, t))
	return `{"model":"gpt-history","input":[` + strings.Join(all, ",") + `]}`
}

func historyProbeResponseItem(t *testing.T, id string, parts ...string) string {
	t.Helper()
	encoded := make([]string, 0, len(parts))
	for _, part := range parts {
		encoded = append(encoded, `{"type":"output_text","annotations":[],"text":`+injectedProbeJSON(t, part)+`}`)
	}
	return `{"id":"` + id + `","type":"message","role":"assistant","content":[` + strings.Join(encoded, ",") + `]}`
}

func historyProbeResponse(item string) string {
	return `{"id":"resp-history","status":"completed","output":[` + item + `]}`
}

func historyProbeRollout(t *testing.T, id string, prior []string, replacement string) string {
	t.Helper()
	lines := []string{strings.TrimSuffix(exportProbeSessionMeta(id), "\n")}
	for index, item := range prior {
		lines = append(lines, fmt.Sprintf(`{"timestamp":"2026-07-01T11:00:01.%03dZ","type":"response_item","payload":%s}`, index, item))
	}
	lines = append(lines,
		`{"timestamp":"`+historyProbeBaseTime+`","type":"compacted","payload":{"message":"","replacement_history":[`+replacement+`]}}`,
		`{"timestamp":"2026-07-01T11:00:03.000Z","type":"event_msg","payload":{"type":"user_message","message":"`+historyProbeLaterUser+`"}}`,
		`{"timestamp":"2026-07-01T11:00:04.000Z","type":"event_msg","payload":{"type":"agent_message","message":"`+historyProbeLater+`"}}`,
	)
	return strings.Join(lines, "\n") + "\n"
}

func historyProbeRecord(t *testing.T, store *capture.Store, exchange historyProbeExchange) {
	t.Helper()
	timestamp, err := time.Parse(time.RFC3339Nano, historyProbeBaseTime)
	if err != nil {
		t.Fatalf("The test could not parse the probe time: %v", err)
	}
	record := func(client string, requestBody string, responseBody string) {
		store.Record(capture.Record{
			Timestamp: timestamp, Client: client, Provider: codexProbeProvider, Concern: client, Host: "local", Method: http.MethodPost,
			Path: "/v1/responses", Status: http.StatusOK, RequestID: exchange.requestID, UpstreamRequestID: "", SessionID: "", ConversationID: "",
			ConversationSource: "", TraceID: "", RequestHeaders: nil, ResponseHeaders: nil, RequestBody: []byte(requestBody),
			ResponseBody: []byte(responseBody), RequestType: "application/json", ResponseType: "application/json", DecodedRequest: nil, Duration: 0,
		})
	}
	record("adapter.codex", exchange.request, exchange.upstream)
	record("adapter.ingress", exchange.request, exchange.client)
}

// historyProbeWriteCapture records exchanges with the given body limit. A
// limit of zero uses the default.
func historyProbeWriteCapture(t *testing.T, databasePath string, maxBodyBytes int, exchanges []historyProbeExchange) {
	t.Helper()
	store, err := capture.Open(context.Background(), capture.Config{DBPath: databasePath, MaxBodyBytes: maxBodyBytes}, nil)
	if err != nil {
		t.Fatalf("The test could not open the capture store with capture.Open: %v", err)
	}
	for _, exchange := range exchanges {
		historyProbeRecord(t, store, exchange)
	}
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("The test could not close the capture store with store.Close: %v", err)
	}
}

func historyProbeCases(t *testing.T) []historyProbeCase {
	t.Helper()
	items := historyProbeTranscriptItems(t)
	injection := historyProbeLegacyInjection()
	full := historyProbeSummary + injection
	request := historyProbeRequest(t, items)
	otherRequest := historyProbeRequest(t, []string{historyProbeMessage("user", "input_text", "historyprobe unrelated", t)})
	newCase := func(name string, suffix int, id string, item string, prior []string, outcome historyProbeOutcome, exchanges ...historyProbeExchange) historyProbeCase {
		return historyProbeCase{
			name: name, session: fmt.Sprintf("%s%02d", historyProbeSession, suffix), itemID: id, item: item, prior: prior, exchange: exchanges, outcome: outcome,
			chat: []string{historyProbeSummary, historyProbeUser, historyProbeAssistant},
		}
	}
	unchanged := func(id string) (string, historyProbeExchange) {
		saved := historyProbeResponseItem(t, id, full)
		return saved, historyProbeExchange{
			requestID: id + "-unchanged", request: request, upstream: historyProbeResponse(saved), client: historyProbeResponse(saved), truncated: false,
		}
	}
	proven := func(requestBody string, id string) historyProbeExchange {
		return historyProbeExchange{
			requestID: id + "-" + requestBody[len(requestBody)-12:len(requestBody)-4], request: requestBody,
			upstream: historyProbeResponse(historyProbeResponseItem(t, id, historyProbeSummary)),
			client:   historyProbeResponse(historyProbeResponseItem(t, id, full)), truncated: false,
		}
	}
	savedChanged := `{"id":"msg-changed","type":"message","role":"assistant","content":[{"type":"output_text","annotations":["changed"],"text":` + injectedProbeJSON(t, full) + `}]}`
	quoted, quotedExchange := unchanged("msg-quoted")
	mixed, mixedUnchanged := unchanged("msg-mixed")
	cut := proven(request, "msg-truncated")
	cut.truncated = true
	cutRollout := proven(request, "msg-truncated-rollout")
	cutRollout.truncated = true
	markedAtomic := newCase("marked whole atomic first segment", 13, "msg-atomic-whole",
		historyProbeMarkedItem(t, "msg-atomic-whole", historyProbeSummary, historyProbeAtomicInjection("{\"cmd\":\"echo "+historyProbeTool+"\"}")), items, historyProbeRecovered)
	markedAtomic.chat = []string{historyProbeSummary, historyProbeAssistant}
	markedTail := newCase("marked partial text first segment", 15, "msg-text-tail",
		historyProbeMarkedItem(t, "msg-text-tail", historyProbeSummary, historyProbeUserInjection(historyProbeUserTail)), items, historyProbeRecovered)
	markedTail.chat = []string{historyProbeSummary, historyProbeUserTail, historyProbeAssistant}
	return []historyProbeCase{
		newCase("truncated capture without source", 8, "msg-truncated", historyProbeResponseItem(t, "msg-truncated", full), nil, historyProbeOrdinary, cut),
		newCase("proven by capture", 1, "msg-proven", historyProbeResponseItem(t, "msg-proven", full), nil, historyProbeRecovered, proven(request, "msg-proven")),
		newCase("proven with unusable request", 2, "msg-unresolved", historyProbeResponseItem(t, "msg-unresolved", full), nil, historyProbeUnresolved, proven(otherRequest, "msg-unresolved")),
		newCase("unchanged capture with rollout source", 3, "msg-quoted", quoted, items, historyProbeOrdinary, quotedExchange),
		newCase("saved part differs from capture", 4, "msg-changed", savedChanged, nil, historyProbeOrdinary, proven(request, "msg-changed")),
		newCase("conflicting evidence", 5, "msg-conflict", historyProbeResponseItem(t, "msg-conflict", full), nil, historyProbeOrdinary, proven(request, "msg-conflict"), proven(otherRequest, "msg-conflict")),
		newCase("unchanged capture beside appended capture", 9, "msg-mixed", mixed, items, historyProbeOrdinary, proven(request, "msg-mixed"), mixedUnchanged),
		newCase("expired capture with rollout source", 6, "msg-rollout", historyProbeResponseItem(t, "msg-rollout", full), items, historyProbeOrdinary),
		newCase("truncated capture with rollout source", 10, "msg-truncated-rollout", historyProbeResponseItem(t, "msg-truncated-rollout", full), items, historyProbeOrdinary, cutRollout),
		newCase("expired capture without source", 7, "msg-absent", historyProbeResponseItem(t, "msg-absent", full), nil, historyProbeOrdinary),
		newCase("marked expired capture with rollout source", 11, "msg-marked", historyProbeMarkedItem(t, "msg-marked", historyProbeSummary, injection), items, historyProbeRecovered),
		newCase("marked expired capture without source", 12, "msg-marked-absent", historyProbeMarkedItem(t, "msg-marked-absent", historyProbeSummary, injection), nil, historyProbeUnresolved),
		markedAtomic,
		newCase("marked partial atomic first segment", 14, "msg-atomic-partial",
			historyProbeMarkedItem(t, "msg-atomic-partial", historyProbeSummary, historyProbeAtomicInjection("echo "+historyProbeTool+"\"}")), items, historyProbeUnresolved),
		markedTail,
	}
}

// TestExportRecoversHistoricalCodexTranscripts exports saved Codex items
// through the CLI and clyde_export_transcript with appended, unchanged,
// truncated, conflicting, and absent capture evidence. It verifies that
// unmarked text stays ordinary without capture proof that Clyde appended it,
// even when exact rollout reconstruction succeeds. It also verifies that
// marked parts recover chat after captures expire and that a partial atomic
// first segment causes a filtered export to fail.
func TestExportRecoversHistoricalCodexTranscripts(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "capture.db")
	cases := historyProbeCases(t)
	complete := make([]historyProbeExchange, 0, len(cases))
	truncated := make([]historyProbeExchange, 0, 1)
	for _, testCase := range cases {
		for _, exchange := range testCase.exchange {
			if exchange.truncated {
				truncated = append(truncated, exchange)
				continue
			}
			complete = append(complete, exchange)
		}
		rolloutPath := codexProbeRolloutPath(home, testCase.session)
		installLocalProofTranscript(t, rolloutPath, historyProbeRollout(t, testCase.session, testCase.prior, testCase.item))
	}
	historyProbeWriteCapture(t, databasePath, 0, complete)
	historyProbeWriteCapture(t, databasePath, historyProbeTruncatedBytes, truncated)

	configRoot := filepath.Join(t.TempDir(), "config")
	writeHistoryProbeConfig(t, configRoot, databasePath)
	environment := environmentWith(append(localProofProviderEnvironment(t, home), "XDG_CONFIG_HOME="+configRoot)...)
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	writeHistoryProbeConfig(t, sandboxDaemon.roots.Config, databasePath)

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, testCase.session)
			assertHistoryProbeExports(t, binaryPath, environment, sandboxDaemon.environment, testCase)
		})
	}
}

func writeHistoryProbeConfig(t *testing.T, root string, databasePath string) {
	t.Helper()
	directory := filepath.Join(root, "clyde")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("The test could not create the config directory: %v", err)
	}
	body := fmt.Sprintf(historyProbeConfig, databasePath)
	if err := os.WriteFile(filepath.Join(directory, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("The test could not write the config: %v", err)
	}
}

func assertHistoryProbeExports(t *testing.T, binaryPath string, cliEnvironment []string, mcpEnvironment []string, testCase historyProbeCase) {
	t.Helper()
	if testCase.outcome == historyProbeUnresolved {
		assertCodexProbeFilteredExportFails(t, binaryPath, cliEnvironment, mcpEnvironment, testCase.session)
		return
	}
	chat := injectedProbeExport{name: "chat", provider: codexProbeProvider, session: testCase.session, only: injectedProbeChatOnly, maxTokens: "", output: "", lastN: codexProbeFullLastN}
	bodies := map[string]string{
		"cli": injectedProbeCLI(t, binaryPath, cliEnvironment, chat),
		"mcp": injectedProbeMCPBody(t, binaryPath, mcpEnvironment, chat),
	}
	for surface, body := range bodies {
		label := surface + " " + testCase.name
		assertCodexProbeContains(t, label, body, testCase.chat...)
		if testCase.outcome == historyProbeRecovered {
			assertCodexProbeOmits(t, label, body, historyProbeTool)
			continue
		}
		assertCodexProbeContains(t, label, body, historyProbeTool)
	}
}

// TestExportReconstructsSuccessiveCodexCompactions saves two compactions
// without capture evidence. It verifies that a marked compaction item and a
// marked live message recover chat from the second window. It also verifies
// that a marked transcript from the replaced window causes a filtered export
// to fail and that an unmarked quotation stays ordinary.
func TestExportReconstructsSuccessiveCodexCompactions(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	injection := historyProbeLegacyInjection()
	stale := reorienttag.WrapInjection("### User\n\nhistoryprobe replaced source text", "")
	secondItem := historyProbeMarkedItem(t, "msg-second", historyProbeSummary, injection)
	liveItem := historyProbeMarkedItem(t, "msg-live", historyProbeSummary, injection)
	compacted := func(replacement ...string) string {
		return `{"timestamp":"2026-07-01T11:00:02.000Z","type":"compacted","payload":{"message":"","replacement_history":[` + strings.Join(replacement, ",") + `]}}`
	}
	sessions := map[string]string{
		"second window":   historyProbeSession + "20",
		"replaced window": historyProbeSession + "21",
		"live message":    historyProbeSession + "22",
	}
	rollouts := map[string][]string{
		"second window":   {compacted(secondItem, historyProbeResponseItem(t, "msg-stale", stale))},
		"replaced window": {compacted(historyProbeMarkedItem(t, "msg-replaced", historyProbeSummary, stale))},
		"live message": {
			compacted(historyProbeMessage("user", "input_text", historyProbeUser, t)),
			`{"timestamp":"2026-07-01T11:00:02.100Z","type":"event_msg","payload":{"type":"agent_message","message":` + injectedProbeJSON(t, historyProbeSummary+injection) + `,"phase":"final_answer"}}`,
			`{"timestamp":"2026-07-01T11:00:02.200Z","type":"response_item","payload":` + liveItem + `}`,
		},
	}
	for name, session := range sessions {
		installLocalProofTranscript(t, codexProbeRolloutPath(home, session), historyProbeSuccessiveRollout(t, session, rollouts[name]))
	}

	environment := environmentWith(localProofProviderEnvironment(t, home)...)
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)

	for name, session := range sessions {
		t.Run(name, func(t *testing.T) {
			waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, session)
			if name == "replaced window" {
				assertCodexProbeFilteredExportFails(t, binaryPath, environment, sandboxDaemon.environment, session)
				return
			}
			chat := injectedProbeExport{name: "chat", provider: codexProbeProvider, session: session, only: injectedProbeChatOnly, maxTokens: "", output: "", lastN: codexProbeFullLastN}
			bodies := map[string]string{
				"cli": injectedProbeCLI(t, binaryPath, environment, chat),
				"mcp": injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, chat),
			}
			for surface, body := range bodies {
				assertCodexProbeContains(t, surface, body, historyProbeSummary, historyProbeUser, historyProbeAssistant)
				assertCodexProbeOmits(t, surface, body, historyProbeTool)
				if name == "second window" && !strings.Contains(body, injectedProbeJSONBody(t, stale)) {
					t.Errorf("The %s export changed an unmarked quotation even though the compaction window does not prove that Clyde generated it:\n%s", surface, body)
				}
			}
		})
	}
}

// historyProbeSuccessiveRollout returns a rollout with one compaction, later
// tool and assistant items, and trailing records from the caller.
func historyProbeSuccessiveRollout(t *testing.T, session string, trailing []string) string {
	t.Helper()
	first := []string{
		historyProbeMessage("user", "input_text", "historyprobe oldest message", t),
		historyProbeMessage("user", "input_text", historyProbeUser, t),
	}
	following := []string{
		`{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"echo ` + historyProbeTool + `\"}","call_id":"call-history"}`,
		`{"type":"function_call_output","call_id":"call-history","output":"` + historyProbeTool + ` output"}`,
		historyProbeMessage("assistant", "output_text", historyProbeAssistant, t),
	}
	lines := []string{
		strings.TrimSuffix(exportProbeSessionMeta(session), "\n"),
		`{"timestamp":"2026-07-01T11:00:00.100Z","type":"response_item","payload":` + historyProbeMessage("user", "input_text", "historyprobe replaced source text", t) + `}`,
		`{"timestamp":"2026-07-01T11:00:01.000Z","type":"compacted","payload":{"message":"","replacement_history":[` + strings.Join(first, ",") + `]}}`,
	}
	for index, item := range following {
		lines = append(lines, fmt.Sprintf(`{"timestamp":"2026-07-01T11:00:01.%03dZ","type":"response_item","payload":%s}`, index+1, item))
	}
	lines = append(lines, trailing...)
	lines = append(lines, `{"timestamp":"2026-07-01T11:00:03.000Z","type":"event_msg","payload":{"type":"user_message","message":"`+historyProbeLaterUser+`"}}`)
	return strings.Join(lines, "\n") + "\n"
}

func injectedProbeJSONBody(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("The test could not encode the probe text: %v", err)
	}
	return strings.Trim(string(encoded), `"`)
}
