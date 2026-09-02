package codex

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/transcript"
)

// RawResponsesCompactionV2Recovery carries private state for Task 5.
type RawResponsesCompactionV2Recovery struct {
	transcript string
	complete   func() bool
	release    func() bool
}

// CompleteRecovery removes the matched recovery after Task 5 persists it.
func (r *RawResponsesCompactionV2Recovery) CompleteRecovery() bool {
	if r == nil || r.complete == nil {
		return false
	}
	return r.complete()
}

// ReleaseRecovery makes a recovery reservation available after persistence fails.
func (r *RawResponsesCompactionV2Recovery) ReleaseRecovery() bool {
	if r == nil || r.release == nil {
		return false
	}
	r.once.Do(func() {
		r.finalized = finalization
		if finalization == rawResponsesCompactionV2RecoveryCompleted && r.complete != nil {
			r.success = r.complete()
		}
		if finalization == rawResponsesCompactionV2RecoveryReleased && r.release != nil {
			r.success = r.release()
		}
	})
	return r.finalized == finalization && r.success
}

// InjectRawResponsesCompactionV2Recovery adds an armed transcript after its
// matching encrypted compaction item in a regular native request.
func InjectRawResponsesCompactionV2Recovery(
	request RawResponsesRequest,
	registry *RawResponsesCompactionV2Registry,
) (RawResponsesRequest, *RawResponsesCompactionV2Recovery, bool) {
	if DetectRawResponsesCompactionProtocol(request.Header) != RawResponsesCompactionNone {
		return request, nil, false
	}
	if registry == nil || bytes.Contains(request.Body, []byte(reorienttag.PreCompactionTranscriptOpen)) ||
		bytes.Contains(request.Body, []byte(reorienttag.PreCompactionTranscriptClose)) {
		return request, nil, false
	}
	sessionID, ok := rawResponsesCompactionV2SessionID(request)
	if !ok {
		return request, nil, false
	}
	inputStart, inputEnd, ok := jsonObjectFieldValueRange(request.Body, "input")
	if !ok {
		return request, nil, false
	}
	var input []json.RawMessage
	if json.Unmarshal(request.Body[inputStart:inputEnd], &input) != nil {
		return request, nil, false
	}
	compactionIndex, encryptedContent, ok := rawResponsesCompactionV2InjectedCompaction(input)
	if !ok {
		return request, nil, false
	}
	transcript, ok := registry.Match(sessionID, encryptedContent)
	if !ok {
		return request, nil, false
	}
	release := rawResponsesCompactionV2Release(registry, sessionID, encryptedContent)
	item, err := json.Marshal(struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "message",
		Role: "assistant",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "output_text", Text: wrappedRawCompactionTranscript(transcript)}},
	})
	if err != nil {
		release()
		return request, nil, false
	}
	mutatedInput := make([]json.RawMessage, 0, len(input)+1)
	mutatedInput = append(mutatedInput, input[:compactionIndex+1]...)
	mutatedInput = append(mutatedInput, item)
	mutatedInput = append(mutatedInput, input[compactionIndex+1:]...)
	encodedInput, err := marshalRawArray(mutatedInput)
	if err != nil {
		release()
		return request, nil, false
	}
	transformed := request
	transformed.Body = replaceByteRange(request.Body, inputStart, inputEnd, encodedInput)
	return transformed, &RawResponsesCompactionV2Recovery{
		transcript: transcript,
		complete:   rawResponsesCompactionV2Completion(registry, sessionID, encryptedContent),
		release:    release,
	}, true
}

func rawResponsesCompactionV2Completion(registry *RawResponsesCompactionV2Registry, sessionID, encryptedContent string) func() bool {
	var once sync.Once
	return func() bool {
		completed := false
		once.Do(func() {
			completed = registry.Complete(sessionID, encryptedContent)
		})
		return completed
	}
}

func rawResponsesCompactionV2Release(registry *RawResponsesCompactionV2Registry, sessionID, encryptedContent string) func() bool {
	return func() bool {
		return registry.Release(sessionID, encryptedContent)
	}
}

func rawResponsesCompactionV2InjectedCompaction(input []json.RawMessage) (int, string, bool) {
	normalized := codexstore.NormalizeResponseInputItems(input)
	if len(normalized) != len(input) {
		return 0, "", false
	}
	index := -1
	encryptedContent := ""
	for itemIndex, item := range normalized {
		if item.Kind != transcript.CompactedContextItemKindCompaction || item.Compaction == nil {
			continue
		}
		candidate := strings.TrimSpace(item.Compaction.EncryptedContent)
		if index >= 0 || candidate == "" {
			return 0, "", false
		}
		index = itemIndex
		encryptedContent = item.Compaction.EncryptedContent
	}
	return index, encryptedContent, index >= 0
}
