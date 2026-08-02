package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// corpusReplayEnv gates the whole-corpus replay: it streams every local Claude
// transcript twice, which takes minutes, so it runs only when asked for
// explicitly rather than on every suite run.
const corpusReplayEnv = "CLYDE_CORPUS_REPLAY"

// claudeCorpusMarkers are every marker the claude parser strips in any class.
// The replay uses them only to partition messages: a message containing none
// of them must come through the stripping path byte-identical.
var claudeCorpusMarkers = func() []string {
	markers := make([]string, 0, len(noiseTags)+len(injectedTags)+len(injectedContextHeadings)+len(injectedFeedbackLinePrefixes)+1)
	for _, tag := range noiseTags {
		markers = append(markers, "<"+tag)
	}
	for _, tag := range injectedTags {
		markers = append(markers, "<"+tag)
	}
	markers = append(markers, injectedContextHeadings...)
	markers = append(markers, injectedFeedbackLinePrefixes...)
	// stripSystemTags also trims one unmatched leading tag, so any message
	// opening with a tag is marker-carrying for partition purposes.
	markers = append(markers, "<")
	return markers
}()

func containsAnyClaudeMarker(text string) bool {
	for _, marker := range claudeCorpusMarkers {
		if marker == "<" {
			if strings.HasPrefix(text, "<") {
				return true
			}
			continue
		}
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// TestLiveClaudeCorpusReplayNoUserTextLoss replays the whole local Claude
// corpus through the stripping path and pins the no-loss property on real
// data: a message that carries no marker at all is returned byte-identical,
// and a stripped message never retains a hook heading. Run it with
// CLYDE_CORPUS_REPLAY=1; it skips otherwise because it streams every
// transcript twice.
func TestLiveClaudeCorpusReplayNoUserTextLoss(t *testing.T) {
	if os.Getenv(corpusReplayEnv) == "" {
		t.Skipf("set %s=1 to replay the local corpus", corpusReplayEnv)
	}
	root := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	paths, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Skipf("live Claude corpus unavailable at %s: %v", root, err)
	}

	transcripts := 0
	messagesSeen := 0
	markerFree := 0
	strippedChanged := 0
	for _, path := range paths {
		full, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
			IncludeSystemPrompts:  true,
			IncludeSystemMessages: false,
			IncludeToolOutputs:    false,
			IncludeInjected:       true,
		}))
		if err != nil {
			// The corpus holds live files that can change or truncate mid-read;
			// a transcript that cannot be read teaches nothing about stripping.
			continue
		}
		stripped, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
			IncludeSystemPrompts:  false,
			IncludeSystemMessages: false,
			IncludeToolOutputs:    false,
			IncludeInjected:       false,
		}))
		if err != nil {
			continue
		}
		transcripts++
		strippedByUUID := make(map[string]transcript.Message, len(stripped))
		for _, message := range stripped {
			if message.UUID != "" {
				strippedByUUID[message.UUID] = message
			}
		}
		for _, message := range full {
			if message.Role != "user" || message.UUID == "" {
				continue
			}
			messagesSeen++
			after, ok := strippedByUUID[message.UUID]
			if containsAnyClaudeMarker(message.Text) {
				if ok && after.Text != message.Text {
					strippedChanged++
				}
				if ok {
					for _, heading := range injectedContextHeadings {
						if strings.Contains(after.Text, heading) {
							t.Fatalf("%s: stripped message %s still carries %q", path, message.UUID, heading)
						}
					}
				}
				continue
			}
			markerFree++
			if !ok {
				t.Fatalf("%s: marker-free message %s disappeared under stripping", path, message.UUID)
			}
			if after.Text != message.Text {
				t.Fatalf("%s: marker-free message %s changed under stripping:\nfull:     %q\nstripped: %q",
					path, message.UUID, message.Text, after.Text)
			}
		}
	}
	t.Logf("replayed %d transcripts: %d user messages, %d marker-free byte-identical, %d marker-carrying changed",
		transcripts, messagesSeen, markerFree, strippedChanged)
}
