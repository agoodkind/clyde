package codexstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusReplayEnv gates the whole-corpus replay: it streams every local Codex
// rollout twice, which takes minutes, so it runs only when asked for
// explicitly rather than on every suite run.
const corpusReplayEnv = "CLYDE_CORPUS_REPLAY"

// TestLiveCodexCorpusReplayNoUserTextLoss replays the local Codex corpus and
// pins the no-loss property on real data: every user-role text the classifier
// calls conversation is returned byte-identical under the default options, and
// every text it withholds reappears under the matching include option. Run it
// with CLYDE_CORPUS_REPLAY=1.
func TestLiveCodexCorpusReplayNoUserTextLoss(t *testing.T) {
	if os.Getenv(corpusReplayEnv) == "" {
		t.Skipf("set %s=1 to replay the local corpus", corpusReplayEnv)
	}
	root := filepath.Join(os.Getenv("HOME"), ".codex", "sessions")
	paths, err := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Skipf("live Codex corpus unavailable at %s: %v", root, err)
	}

	allOn := HistoryOptions{IncludeSystemMessages: true, IncludeSystemPrompts: true, IncludeInjected: true}
	defaults := HistoryOptions{IncludeSystemMessages: false, IncludeSystemPrompts: false, IncludeInjected: false}

	rollouts := 0
	conversationTexts := 0
	withheldTexts := 0
	for _, path := range paths {
		fullTexts := collectUserTexts(t, path, allOn)
		if fullTexts == nil {
			continue
		}
		strippedTexts := collectUserTexts(t, path, defaults)
		if strippedTexts == nil {
			continue
		}
		rollouts++
		strippedSet := make(map[string]int, len(strippedTexts))
		for _, text := range strippedTexts {
			strippedSet[text]++
		}
		for _, text := range fullTexts {
			if classifyCodexUserText(text) == codexUserTextConversation {
				conversationTexts++
				if strippedSet[text] == 0 {
					t.Fatalf("%s: conversation-class user text lost under defaults: %.120q", path, text)
				}
				strippedSet[text]--
				continue
			}
			withheldTexts++
		}
		// Nothing the defaults returned may be a harness class: the withheld
		// set and the returned set partition the user texts exactly.
		for text, count := range strippedSet {
			if count > 0 && classifyCodexUserText(text) != codexUserTextConversation {
				t.Fatalf("%s: harness-class text survived the defaults: %.120q", path, text)
			}
		}
	}
	t.Logf("replayed %d rollouts: %d conversation user texts preserved, %d harness texts withheld",
		rollouts, conversationTexts, withheldTexts)
}

// collectUserTexts streams one rollout and returns its user-role texts, or nil
// when the file cannot be read (live files change underfoot and teach nothing
// about classification).
func collectUserTexts(t *testing.T, path string, opts HistoryOptions) []string {
	t.Helper()
	texts := make([]string, 0)
	for message, err := range StreamMessages(path, opts) {
		if err != nil {
			return nil
		}
		if message.Role != "user" || strings.TrimSpace(message.Text) == "" {
			continue
		}
		texts = append(texts, message.Text)
	}
	return texts
}
