package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
)

type trackedUsage struct {
	usage      Usage
	rawPrompt  int
	rawTotal   int
	rolledFrom int
}

type contextUsageTracker struct {
	mu    sync.Mutex
	state map[string]contextUsageState
}

type contextUsageState struct {
	LatestPrompt     int
	CumulativeOutput int
	LastTotal        int
}

func newContextUsageTracker() *contextUsageTracker {
	return &contextUsageTracker{
		state: make(map[string]contextUsageState), mu: sync.
			Mutex{},
	}
}

func (t *contextUsageTracker) Track(key string, raw Usage) trackedUsage {
	if t == nil || strings.TrimSpace(key) == "" {
		return trackedUsage{usage: raw, rawPrompt: raw.PromptTokens, rawTotal: raw.TotalTokens, rolledFrom: 0}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.state[key]
	rolledFrom := st.CumulativeOutput
	if shouldResetTrackedContext(st, raw) {
		st = contextUsageState{LatestPrompt: 0, CumulativeOutput: 0, LastTotal: 0}
		rolledFrom = 0
	}

	surfaced := raw
	surfaced.PromptTokens = raw.PromptTokens + rolledFrom
	surfaced.TotalTokens = surfaced.PromptTokens + surfaced.CompletionTokens

	st.LatestPrompt = raw.PromptTokens
	st.CumulativeOutput = rolledFrom + raw.CompletionTokens
	st.LastTotal = surfaced.TotalTokens
	t.state[key] = st

	return trackedUsage{
		usage:      surfaced,
		rawPrompt:  raw.PromptTokens,
		rawTotal:   raw.TotalTokens,
		rolledFrom: rolledFrom,
	}
}

func shouldResetTrackedContext(prev contextUsageState, raw Usage) bool {
	if prev.LatestPrompt == 0 && prev.CumulativeOutput == 0 && prev.LastTotal == 0 {
		return false
	}
	// When the prompt collapses hard relative to the previous turn, the
	// conversation likely compacted or restarted. Keeping the old output
	// tail would inflate the surfaced "context used" forever.
	if raw.PromptTokens > 0 && prev.LatestPrompt > 0 && raw.PromptTokens*2 < prev.LatestPrompt {
		return true
	}
	if raw.TotalTokens > 0 && prev.LastTotal > 0 && raw.TotalTokens*2 < prev.LastTotal {
		return true
	}
	return false
}

func requestContextTrackerKey(req ChatRequest, modelAlias string) string {
	ic := ingressContextForRequest(req)
	return requestContextTrackerKeyFromIngress(ic, req, modelAlias)
}

func requestContextTrackerKeyFromIngress(ic ingresscontract.IngressContext, req ChatRequest, modelAlias string) string {
	if conversation := strings.TrimSpace(ic.ConversationID); conversation != "" {
		return "cursor:" + conversation
	}
	if v := strings.TrimSpace(ic.User); v != "" {
		return "user:" + v
	}
	if v := metadataString(req.Metadata, "conversation_id", "conversationId", "composerId", "composer_id", "thread_id", "threadId", "chat_id", "chatId"); v != "" {
		return "meta:" + v
	}
	firstUser := ""
	for _, msg := range req.Messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			firstUser = strings.TrimSpace(FlattenContent(msg.Content))
			if firstUser != "" {
				break
			}
		}
	}
	if firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(modelAlias + "\n" + firstUser))
	return "fingerprint:" + hex.EncodeToString(sum[:16])
}

// ingressContextForRequest is the boundary-safe entry point free
// functions use to translate a ChatRequest through the registered
// ingress contract. When no ingress is registered the function
// returns a zero IngressContext so the caller's primitive code path
// degrades gracefully (the production path always has Cursor wired).
func ingressContextForRequest(req ChatRequest) ingresscontract.IngressContext {
	contract, ok := activeIngressContract()
	if !ok || contract == nil {
		return ingresscontract.IngressContext{
			User:              "",
			RequestID:         "",
			ConversationID:    "",
			GenerationID:      "",
			WorkspacePath:     "",
			NormalizedModel:   "",
			PathKind:          "",
			Mode:              "",
			RawToolNames:      nil,
			HasSubagentTool:   false,
			CanSwitchMode:     false,
			HasCreatePlanTool: false,
			HasApplyPatchTool: false,
			MCPToolNames:      nil,
		}
	}
	return contract.Translate(ingresscontract.ChatRequestPrimitive{Body: req})
}

func metadataString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := m[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}
