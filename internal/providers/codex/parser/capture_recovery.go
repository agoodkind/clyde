package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/mitm/capture"
	codexcompaction "goodkind.io/clyde/internal/providers/codex/compaction"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorientinject"
	"goodkind.io/clyde/internal/reorienttag"
)

const (
	captureEvidenceWindow    = 24 * time.Hour
	captureEvidenceLimit     = 16
	captureSSEDataPrefix     = "data:"
	captureSSECompletedType  = "response.completed"
	captureContentField      = "content"
	captureTextField         = "text"
	captureTypeField         = "type"
	captureIDField           = "id"
	captureMetadataField     = "internal_chat_message_metadata_passthrough"
	captureOutputTextType    = "output_text"
	captureWrapperBoundaries = " \t\r\n"
	captureUnmarkedIndex     = -1
	// A rollout does not store the compaction prompt, and the Codex decoder
	// requires a prompt after the counted messages.
	rolloutPromptItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"compaction prompt"}]}`
)

// capturedItem stores decoded item fields and content parts without producer
// metadata.
type capturedItem struct {
	fields capturedFields
	parts  []capturedFields
}

type capturedFields map[string]json.RawMessage

// captureTranscriptResolver returns nil unless export requests transcript
// resolution. The returned resolver reads the capture database path from the
// global configuration. If configuration loading fails, the resolver logs a
// warning and cannot use capture evidence.
func captureTranscriptResolver(opts conversation.LoadOptions) codexstore.UnmarkedTranscriptResolver {
	if !opts.ResolveInjectedTranscripts {
		return nil
	}
	databasePath := ""
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.Warn("providers.codex.capture_recovery_config_failed", "concern", concern, "err", err)
	} else {
		databasePath = cfg.MITM.CaptureStore.DBPath
	}
	return func(request codexstore.TranscriptRequest) (codexstore.UnmarkedTranscript, bool) {
		return resolveWithOriginProof(databasePath, request)
	}
}

// resolveWithOriginProof requires capture evidence that Clyde appended an
// unmarked item's text because exact reconstruction cannot distinguish an
// ordinary quotation from generated text. The content kind marker proves the
// origin of a marked part. For a marked part, the function uses capture spans
// when they exist and otherwise rebuilds spans from the rollout.
func resolveWithOriginProof(databasePath string, request codexstore.TranscriptRequest) (codexstore.UnmarkedTranscript, bool) {
	proven, ok := resolveFromCapture(databasePath, request)
	if request.MarkedIndex == captureUnmarkedIndex {
		return proven, ok
	}
	if ok && proven.PartIndex == request.MarkedIndex && len(proven.Spans) > 0 {
		return proven, true
	}
	return reconstructFromRollout(request)
}

// resolveFromCapture reads exchanges with the saved message ID near its
// rollout time. It rejects the result if any exchange leaves the item
// unchanged or appended exchanges disagree.
func resolveFromCapture(databasePath string, request codexstore.TranscriptRequest) (codexstore.UnmarkedTranscript, bool) {
	empty := codexstore.UnmarkedTranscript{PartIndex: 0, Start: 0, Spans: nil}
	saved, ok := decodeCapturedItem(request.RawItem)
	if !ok || databasePath == "" {
		return empty, false
	}
	messageID := fieldText(saved.fields, captureIDField)
	if messageID == "" {
		return empty, false
	}
	query := capture.ExchangeEvidenceQuery{
		DBPath:    databasePath,
		MessageID: messageID,
		Start:     request.Timestamp.Add(-captureEvidenceWindow),
		End:       request.Timestamp.Add(captureEvidenceWindow),
		Limit:     captureEvidenceLimit,
	}
	var proven []codexstore.UnmarkedTranscript
	unchanged := false
	err := capture.ReadExchangeEvidence(context.Background(), query, func(evidence capture.ExchangeEvidence) bool {
		result, verdict := exchangeVerdict(saved, messageID, evidence)
		switch verdict {
		case captureVerdictUnchanged:
			unchanged = true
			return false
		case captureVerdictAppended:
			proven = append(proven, result)
		case captureVerdictInconclusive:
		}
		return true
	})
	if err != nil {
		slog.Warn("providers.codex.capture_recovery_read_failed", "concern", concern, "err", err)
		return empty, false
	}
	if unchanged {
		return empty, false
	}
	return agreedTranscript(proven)
}

// agreedTranscript returns the first result when every result has the same
// part, start, span boundaries, and segment role, kind, and text. It returns
// false for any difference.
func agreedTranscript(proven []codexstore.UnmarkedTranscript) (codexstore.UnmarkedTranscript, bool) {
	empty := codexstore.UnmarkedTranscript{PartIndex: 0, Start: 0, Spans: nil}
	if len(proven) == 0 {
		return empty, false
	}
	first := proven[0]
	for _, candidate := range proven[1:] {
		if !sameTranscript(first, candidate) {
			return empty, false
		}
	}
	return first, true
}

func sameTranscript(first codexstore.UnmarkedTranscript, second codexstore.UnmarkedTranscript) bool {
	return first.PartIndex == second.PartIndex &&
		first.Start == second.Start &&
		slices.EqualFunc(first.Spans, second.Spans, sameSpan)
}

func sameSpan(first reorienttag.TranscriptSpan, second reorienttag.TranscriptSpan) bool {
	return first.Start == second.Start && first.End == second.End && slices.Equal(first.Segments, second.Segments)
}

// captureVerdict records what one complete exchange shows about the origin of
// a saved assistant item.
type captureVerdict int

const (
	// captureVerdictInconclusive applies when the exchange does not match the
	// saved item or changes the item in a way other than one appended transcript.
	captureVerdictInconclusive captureVerdict = iota
	// captureVerdictUnchanged applies when the upstream item equals the client
	// item. The upstream model wrote the text, and Clyde did not add it.
	captureVerdictUnchanged
	// captureVerdictAppended applies when the client item equals the upstream item
	// except for one appended transcript.
	captureVerdictAppended
)

// exchangeVerdict compares the saved item with the client item and the client
// item with the upstream item. It returns the transcript location and typed
// spans only for captureVerdictAppended.
func exchangeVerdict(saved capturedItem, messageID string, evidence capture.ExchangeEvidence) (codexstore.UnmarkedTranscript, captureVerdict) {
	empty := codexstore.UnmarkedTranscript{PartIndex: 0, Start: 0, Spans: nil}
	upstreamRaw, hasUpstream := terminalMessage(evidence.UpstreamResponse, messageID)
	clientRaw, hasClient := terminalMessage(evidence.ClientResponse, messageID)
	if !hasUpstream || !hasClient {
		return empty, captureVerdictInconclusive
	}
	upstream, upstreamOK := decodeCapturedItem(upstreamRaw)
	client, clientOK := decodeCapturedItem(clientRaw)
	if !upstreamOK || !clientOK || !sameFields(upstream.fields, client.fields) || !savedMatchesClient(saved, client) {
		return empty, captureVerdictInconclusive
	}
	if slices.EqualFunc(upstream.parts, client.parts, sameFields) {
		return empty, captureVerdictUnchanged
	}
	partIndex, start, appended := appendedTranscript(upstream.parts, client.parts)
	if !appended {
		return empty, captureVerdictInconclusive
	}
	text := partText(client.parts[partIndex])
	return codexstore.UnmarkedTranscript{
		PartIndex: partIndex,
		Start:     start,
		Spans:     appendedSpans(text, start, evidence.ClientRequest),
	}, captureVerdictAppended
}

// decodeCapturedItem decodes all fields and content parts, then removes
// content and internal_chat_message_metadata_passthrough from the fields.
func decodeCapturedItem(raw json.RawMessage) (capturedItem, bool) {
	empty := capturedItem{fields: nil, parts: nil}
	var fields capturedFields
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return empty, false
	}
	var parts []capturedFields
	if json.Unmarshal(fields[captureContentField], &parts) != nil || parts == nil || slices.ContainsFunc(parts, nilFields) {
		return empty, false
	}
	delete(fields, captureContentField)
	delete(fields, captureMetadataField)
	return capturedItem{fields: fields, parts: parts}, true
}

// savedMatchesClient requires each saved field and each saved part field to
// equal the corresponding client value because native Codex can omit fields
// from a saved item.
func savedMatchesClient(saved capturedItem, client capturedItem) bool {
	if len(saved.parts) != len(client.parts) || !fieldsPresentIn(saved.fields, client.fields) {
		return false
	}
	for index, part := range saved.parts {
		if !fieldsPresentIn(part, client.parts[index]) {
			return false
		}
	}
	return true
}

func nilFields(fields capturedFields) bool {
	return fields == nil
}

func fieldsPresentIn(saved capturedFields, client capturedFields) bool {
	for key, value := range saved {
		captured, present := client[key]
		if !present || !sameJSON(value, captured) {
			return false
		}
	}
	return true
}

func sameFields(first capturedFields, second capturedFields) bool {
	return len(first) == len(second) && fieldsPresentIn(first, second)
}

// sameJSON compares two JSON values without regard to object key order or
// string escape sequences. It compares object fields, array items, and decoded
// strings, and compares every other value by its bytes.
func sameJSON(first json.RawMessage, second json.RawMessage) bool {
	firstValue := bytes.TrimSpace(first)
	secondValue := bytes.TrimSpace(second)
	if len(firstValue) == 0 || len(secondValue) == 0 || firstValue[0] != secondValue[0] {
		return false
	}
	switch firstValue[0] {
	case '{':
		var firstFields, secondFields capturedFields
		return json.Unmarshal(firstValue, &firstFields) == nil &&
			json.Unmarshal(secondValue, &secondFields) == nil &&
			sameFields(firstFields, secondFields)
	case '[':
		var firstList, secondList []json.RawMessage
		return json.Unmarshal(firstValue, &firstList) == nil &&
			json.Unmarshal(secondValue, &secondList) == nil &&
			slices.EqualFunc(firstList, secondList, sameJSON)
	case '"':
		var firstText, secondText string
		return json.Unmarshal(firstValue, &firstText) == nil &&
			json.Unmarshal(secondValue, &secondText) == nil &&
			firstText == secondText
	default:
		return bytes.Equal(firstValue, secondValue)
	}
}

func fieldText(fields capturedFields, key string) string {
	var text string
	if json.Unmarshal(fields[key], &text) != nil {
		return ""
	}
	return text
}

func partText(part capturedFields) string {
	return fieldText(part, captureTextField)
}

// appendedTranscript accepts one added output_text part or one text suffix in
// one part. Every other upstream part must equal the corresponding client
// part.
func appendedTranscript(upstream []capturedFields, client []capturedFields) (int, int, bool) {
	if len(client) == len(upstream)+1 {
		last := len(upstream)
		isOutputText := fieldText(client[last], captureTypeField) == captureOutputTextType
		if !slices.EqualFunc(upstream, client[:last], sameFields) || !isOutputText {
			return 0, 0, false
		}
		return last, 0, completeWrapper(partText(client[last]))
	}
	if len(client) != len(upstream) {
		return 0, 0, false
	}
	changed := -1
	for index := range client {
		if sameFields(client[index], upstream[index]) {
			continue
		}
		if changed >= 0 || !sameFieldsWithoutText(client[index], upstream[index]) {
			return 0, 0, false
		}
		changed = index
	}
	if changed < 0 {
		return 0, 0, false
	}
	upstreamText := partText(upstream[changed])
	suffix, hasPrefix := strings.CutPrefix(partText(client[changed]), upstreamText)
	if !hasPrefix {
		return 0, 0, false
	}
	return changed, len(upstreamText), completeWrapper(suffix)
}

func sameFieldsWithoutText(first capturedFields, second capturedFields) bool {
	if len(first) != len(second) {
		return false
	}
	for key, value := range first {
		if key != captureTextField && !sameJSON(value, second[key]) {
			return false
		}
	}
	return true
}

// completeWrapper reports whether trimmed text starts with the transcript open
// tag and ends with the close tag.
func completeWrapper(text string) bool {
	trimmed := strings.Trim(text, captureWrapperBoundaries)
	return strings.HasPrefix(trimmed, reorienttag.PreCompactionTranscriptOpen) &&
		strings.HasSuffix(trimmed, reorienttag.PreCompactionTranscriptClose)
}

// appendedSpans returns framed spans from the appended text. For unframed
// text, it reconstructs segments from the client compaction request and
// returns nil when reconstruction fails.
func appendedSpans(text string, start int, clientRequest []byte) []reorienttag.TranscriptSpan {
	suffix := text[start:]
	if framed := reorienttag.ParseTranscriptSpans(suffix); len(framed) > 0 {
		shifted := make([]reorienttag.TranscriptSpan, 0, len(framed))
		for _, span := range framed {
			span.Start += start
			span.End += start
			shifted = append(shifted, span)
		}
		return shifted
	}
	request, ok := codexcompaction.NewProvider().ParseCompaction(clientRequest)
	if !ok {
		return nil
	}
	wrapperStart := start + strings.Index(suffix, reorienttag.PreCompactionTranscriptOpen)
	segments, wrapperEnd, ok := reconstructWrapper(request, text, wrapperStart)
	if !ok {
		return nil
	}
	return []reorienttag.TranscriptSpan{{Start: wrapperStart, End: wrapperEnd, Segments: segments}}
}

// reconstructWrapper requires the close tag at the end of the text and
// reconstructs the wrapper body.
func reconstructWrapper(request reorientinject.ParsedRequest, text string, wrapperStart int) ([]reorienttag.Segment, int, bool) {
	closeStart := strings.LastIndex(text, reorienttag.PreCompactionTranscriptClose)
	innerStart := wrapperStart + len(reorienttag.PreCompactionTranscriptOpen)
	if closeStart < innerStart || strings.TrimRight(text[closeStart+len(reorienttag.PreCompactionTranscriptClose):], captureWrapperBoundaries) != "" {
		return nil, 0, false
	}
	inner := strings.Trim(text[innerStart:closeStart], captureWrapperBoundaries)
	segments, ok := reorientinject.ReconstructUnframedTranscript(request, inner)
	return segments, closeStart + len(reorienttag.PreCompactionTranscriptClose), ok
}

// reconstructFromRollout rebuilds the wrapper at the end of the marked part
// from typed items in the compaction window. Callers use this function only
// for marked parts.
func reconstructFromRollout(request codexstore.TranscriptRequest) (codexstore.UnmarkedTranscript, bool) {
	empty := codexstore.UnmarkedTranscript{PartIndex: 0, Start: 0, Spans: nil}
	saved, ok := decodeCapturedItem(request.RawItem)
	if !ok || len(saved.parts) == 0 || len(request.PriorItems) == 0 {
		return empty, false
	}
	parsed, ok := rolloutWindowRequest(request.PriorItems)
	if !ok {
		return empty, false
	}
	partIndex := request.MarkedIndex
	if partIndex < 0 || partIndex >= len(saved.parts) {
		return empty, false
	}
	return uniqueRolloutSpan(parsed, partText(saved.parts[partIndex]), partIndex)
}

// rolloutWindowRequest adds the required prompt item, then decodes the window
// with the production compaction parser.
func rolloutWindowRequest(window []json.RawMessage) (reorientinject.ParsedRequest, bool) {
	items := make([]json.RawMessage, 0, len(window)+1)
	for _, item := range window {
		if rolloutWindowItem(item) {
			items = append(items, item)
		}
	}
	items = append(items, json.RawMessage(rolloutPromptItem))
	body, err := json.Marshal(struct {
		Input []json.RawMessage `json:"input"`
	}{Input: items})
	if err != nil {
		slog.Warn("providers.codex.capture_recovery_window_encode_failed", "concern", concern, "err", err)
		return reorientinject.ParsedRequest{SessionID: "", Messages: nil, RetainStart: 0, InstructionStart: 0, Arguments: nil}, false
	}
	return codexcompaction.NewProvider().ParseCompaction(body)
}

func rolloutWindowItem(item json.RawMessage) bool {
	var identity struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(item, &identity) != nil {
		return false
	}
	return identity.Type != "additional_tools" && identity.Type != "configuration_update"
}

// uniqueRolloutSpan tests each wrapper start and each window end. It returns a
// result only when every exact reconstruction agrees.
func uniqueRolloutSpan(parsed reorientinject.ParsedRequest, text string, partIndex int) (codexstore.UnmarkedTranscript, bool) {
	empty := codexstore.UnmarkedTranscript{PartIndex: 0, Start: 0, Spans: nil}
	matches := make([]codexstore.UnmarkedTranscript, 0, 1)
	for offset := 0; offset < len(text); {
		found := strings.Index(text[offset:], reorienttag.PreCompactionTranscriptOpen)
		if found < 0 {
			break
		}
		wrapperStart := offset + found
		offset = wrapperStart + len(reorienttag.PreCompactionTranscriptOpen)
		for end := parsed.InstructionStart; end > parsed.RetainStart; end-- {
			window := parsed
			window.InstructionStart = end
			segments, wrapperEnd, ok := reconstructWrapper(window, text, wrapperStart)
			if !ok {
				continue
			}
			matches = append(matches, codexstore.UnmarkedTranscript{
				PartIndex: partIndex,
				Start:     wrapperStart,
				Spans:     []reorienttag.TranscriptSpan{{Start: wrapperStart, End: wrapperEnd, Segments: segments}},
			})
		}
	}
	if len(matches) == 0 {
		return empty, false
	}
	return agreedTranscript(matches)
}

// terminalMessage returns the message with messageID from a JSON response or a
// response.completed event.
func terminalMessage(body []byte, messageID string) (json.RawMessage, bool) {
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(body, &response) == nil {
		return outputMessage(response.Output, messageID)
	}
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		data, isData := bytes.CutPrefix(bytes.TrimSpace(line), []byte(captureSSEDataPrefix))
		if !isData {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &event) == nil && event.Type == captureSSECompletedType {
			return outputMessage(event.Response.Output, messageID)
		}
	}
	return nil, false
}

func outputMessage(output []json.RawMessage, messageID string) (json.RawMessage, bool) {
	for _, item := range output {
		var message struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(item, &message) == nil && message.ID == messageID {
			return item, true
		}
	}
	return nil, false
}
