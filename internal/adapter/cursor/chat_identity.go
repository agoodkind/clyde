package cursor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

const (
	// ChatKeySourceNative means the chat key came from client-supplied
	// conversation metadata.
	ChatKeySourceNative = "native"
	// ChatKeySourceDerived means Clyde derived the chat key from sanitized
	// request lineage because Cursor did not send native chat metadata.
	ChatKeySourceDerived = "derived"
)

// ChatIdentity describes the privacy-safe chat partition selected for one
// Cursor request.
type ChatIdentity struct {
	ChatKey        string
	ChatKeySource  string
	ChatRootKey    string
	ChatBranchKey  string
	Fork           ChatFork
	MessageCount   int
	ToolCount      int
	LineageEntries []string
}

// ChatFork is populated when a derived root first gains a second branch.
type ChatFork struct {
	Detected        bool
	PriorBranchKey  string
	CommonPrefixLen int
}

type chatBranch struct {
	key     string
	lineage []string
}

type chatRoot struct {
	branches      []chatBranch
	forkAnnounced bool
}

// ChatIdentityResolver keeps per-daemon branch state for Cursor BYOK requests
// that arrive without native chat identifiers.
type ChatIdentityResolver struct {
	mu    sync.Mutex
	roots map[string]*chatRoot
}

// NewChatIdentityResolver returns an empty per-daemon Cursor chat identity
// resolver.
func NewChatIdentityResolver() *ChatIdentityResolver {
	return &ChatIdentityResolver{
		mu:    sync.Mutex{},
		roots: make(map[string]*chatRoot),
	}
}

// Resolve returns the effective chat identity for a Cursor request.
func (r *ChatIdentityResolver) Resolve(corr correlation.Context, cursorReq Request, req adapteropenai.ChatRequest) ChatIdentity {
	if existing := strings.TrimSpace(clydeingress.ChatKey(corr)); existing != "" {
		return nativeChatIdentity(existing)
	}
	if strings.TrimSpace(cursorReq.ConversationID) != "" {
		return nativeChatIdentity(cursorReq.ConversationID)
	}

	rootKey := DeriveChatKey(req)
	if rootKey == "" {
		return emptyChatIdentity()
	}
	lineage := DeriveLineage(req)
	if len(lineage) == 0 {
		return ChatIdentity{
			ChatKey:        rootKey + ".b01",
			ChatKeySource:  ChatKeySourceDerived,
			ChatRootKey:    rootKey,
			ChatBranchKey:  "b01",
			Fork:           emptyChatFork(),
			MessageCount:   len(req.Messages),
			ToolCount:      len(req.Tools) + len(req.Functions),
			LineageEntries: nil,
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existingRootKey, branchKey, ok := r.matchCompactedRoot(rootKey, lineage); ok {
		return derivedChatIdentity(existingRootKey, branchKey, req, lineage, emptyChatFork())
	}

	root := r.roots[rootKey]
	if root == nil {
		root = &chatRoot{
			branches:      nil,
			forkAnnounced: false,
		}
		r.roots[rootKey] = root
	}

	branchIndex, compacted := matchingBranch(root.branches, lineage)
	if branchIndex >= 0 {
		branch := root.branches[branchIndex]
		if !compacted && len(lineage) > len(branch.lineage) {
			root.branches[branchIndex].lineage = cloneStrings(lineage)
		}
		return derivedChatIdentity(rootKey, branch.key, req, lineage, emptyChatFork())
	}

	priorBranch, commonPrefixLen := closestBranch(root.branches, lineage)
	branchKey := fmt.Sprintf("b%02d", len(root.branches)+1)
	root.branches = append(root.branches, chatBranch{
		key:     branchKey,
		lineage: cloneStrings(lineage),
	})

	fork := emptyChatFork()
	if len(root.branches) == 2 && !root.forkAnnounced {
		root.forkAnnounced = true
		fork = ChatFork{
			Detected:        true,
			PriorBranchKey:  priorBranch,
			CommonPrefixLen: commonPrefixLen,
		}
	}
	return derivedChatIdentity(rootKey, branchKey, req, lineage, fork)
}

func (r *ChatIdentityResolver) matchCompactedRoot(rootKey string, lineage []string) (string, string, bool) {
	rootKeys := make([]string, 0, len(r.roots))
	for existingRootKey := range r.roots {
		rootKeys = append(rootKeys, existingRootKey)
	}
	sort.Strings(rootKeys)
	for _, existingRootKey := range rootKeys {
		if existingRootKey == rootKey {
			continue
		}
		root := r.roots[existingRootKey]
		branchIndex, compacted := matchingBranch(root.branches, lineage)
		if branchIndex < 0 || !compacted {
			continue
		}
		return existingRootKey, root.branches[branchIndex].key, true
	}
	return "", "", false
}

func nativeChatIdentity(key string) ChatIdentity {
	trimmed := strings.TrimSpace(key)
	return ChatIdentity{
		ChatKey:        trimmed,
		ChatKeySource:  ChatKeySourceNative,
		ChatRootKey:    trimmed,
		ChatBranchKey:  "",
		Fork:           emptyChatFork(),
		MessageCount:   0,
		ToolCount:      0,
		LineageEntries: nil,
	}
}

func emptyChatIdentity() ChatIdentity {
	return ChatIdentity{
		ChatKey:        "",
		ChatKeySource:  "",
		ChatRootKey:    "",
		ChatBranchKey:  "",
		Fork:           emptyChatFork(),
		MessageCount:   0,
		ToolCount:      0,
		LineageEntries: nil,
	}
}

func emptyChatFork() ChatFork {
	return ChatFork{
		Detected:        false,
		PriorBranchKey:  "",
		CommonPrefixLen: 0,
	}
}

func derivedChatIdentity(rootKey string, branchKey string, req adapteropenai.ChatRequest, lineage []string, fork ChatFork) ChatIdentity {
	return ChatIdentity{
		ChatKey:        rootKey + "." + branchKey,
		ChatKeySource:  ChatKeySourceDerived,
		ChatRootKey:    rootKey,
		ChatBranchKey:  branchKey,
		Fork:           fork,
		MessageCount:   len(req.Messages),
		ToolCount:      len(req.Tools) + len(req.Functions),
		LineageEntries: cloneStrings(lineage),
	}
}

func matchingBranch(branches []chatBranch, lineage []string) (int, bool) {
	for i, branch := range branches {
		if stringSliceHasPrefix(lineage, branch.lineage) {
			return i, false
		}
		if stringSliceHasSuffix(branch.lineage, lineage) {
			return i, true
		}
	}
	return -1, false
}

func closestBranch(branches []chatBranch, lineage []string) (string, int) {
	priorBranchKey := ""
	longestPrefix := 0
	for _, branch := range branches {
		commonPrefixLen := commonPrefix(branch.lineage, lineage)
		if commonPrefixLen <= longestPrefix {
			continue
		}
		longestPrefix = commonPrefixLen
		priorBranchKey = branch.key
	}
	return priorBranchKey, longestPrefix
}

func commonPrefix(left []string, right []string) int {
	limit := min(len(left), len(right))
	for i := range limit {
		if left[i] != right[i] {
			return i
		}
	}
	return limit
}

func stringSliceHasPrefix(values []string, prefix []string) bool {
	if len(prefix) > len(values) {
		return false
	}
	for i := range prefix {
		if values[i] != prefix[i] {
			return false
		}
	}
	return true
}

func stringSliceHasSuffix(values []string, suffix []string) bool {
	if len(suffix) > len(values) {
		return false
	}
	offset := len(values) - len(suffix)
	for i := range suffix {
		if values[offset+i] != suffix[i] {
			return false
		}
	}
	return true
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

// DeriveLineage returns ordered, privacy-safe message fingerprints for fork
// detection. Raw message content and raw tool arguments are never returned.
func DeriveLineage(req adapteropenai.ChatRequest) []string {
	lineage := make([]string, 0, len(req.Messages))
	for _, msg := range req.Messages {
		entry := lineageEntry(msg)
		if entry == "" {
			continue
		}
		lineage = append(lineage, entry)
	}
	return lineage
}

func lineageEntry(msg adapteropenai.ChatMessage) string {
	parts := make([]string, 0, 6+len(msg.ToolCalls)*2)
	appendLineagePart(&parts, "role", msg.Role)
	appendLineagePart(&parts, "name", msg.Name)
	appendLineagePart(&parts, "tool_call_id", msg.ToolCallID)
	appendLineagePart(&parts, "content_hash", normalizedContentHash(msg.Content))
	for _, toolCall := range msg.ToolCalls {
		appendLineagePart(&parts, "tool_id", toolCall.ID)
		appendLineagePart(&parts, "tool_name", toolCall.Function.Name)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\x1f")
}

func appendLineagePart(parts *[]string, key string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	*parts = append(*parts, key+"="+value)
}

func normalizedContentHash(raw json.RawMessage) string {
	normalized := normalizedContent(raw)
	if len(normalized) == 0 {
		return ""
	}
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:8])
}

func normalizedContent(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		trimmed := strings.TrimSpace(plain)
		if trimmed == "" {
			return nil
		}
		return []byte(trimmed)
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err == nil {
		return compacted.Bytes()
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return []byte(trimmed)
}
