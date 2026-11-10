package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConversationSemanticDirectionsFromConfig(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name        string
		contents    string
		wantFeeds   bool
		wantAnswers bool
		wantUses    bool
	}{
		{name: "omitted section", contents: "", wantFeeds: false, wantAnswers: false, wantUses: false},
		{name: "explicit false", contents: "[conversation.semantic]\ningestion_enabled = false\nsearch_enabled = false\n", wantFeeds: false, wantAnswers: false, wantUses: false},
		{name: "ingestion only", contents: "[conversation.semantic]\ningestion_enabled = true\n", wantFeeds: true, wantAnswers: false, wantUses: true},
		{name: "search only", contents: "[conversation.semantic]\nsearch_enabled = true\n", wantFeeds: false, wantAnswers: true, wantUses: true},
		{name: "both true", contents: "[conversation.semantic]\ningestion_enabled = true\nsearch_enabled = true\n", wantFeeds: true, wantAnswers: true, wantUses: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(testCase.contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := loadConfig(dir)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			semantic := cfg.Conversation.Semantic
			if got := semantic.FeedsEngine(); got != testCase.wantFeeds {
				t.Fatalf("FeedsEngine() = %v, want %v", got, testCase.wantFeeds)
			}
			if got := semantic.AnswersSearch(); got != testCase.wantAnswers {
				t.Fatalf("AnswersSearch() = %v, want %v", got, testCase.wantAnswers)
			}
			if got := semantic.UsesEngine(); got != testCase.wantUses {
				t.Fatalf("UsesEngine() = %v, want %v", got, testCase.wantUses)
			}
		})
	}
}

// TestStoppingTheWritesKeepsSearchReachable is the case this split exists for.
//
// Offering conversations to the engine embeds text, which occupies the GPU and
// grows the store, so an operator stops it under thermal or disk pressure.
// Reading queries a corpus that already exists and costs nothing. When one
// setting decided both, stopping the writes also took millions of already
// embedded rows out of reach with no way to get them back short of resuming the
// writes.
func TestStoppingTheWritesKeepsSearchReachable(t *testing.T) {
	t.Parallel()

	semantic := ConversationSemanticConfig{IngestionEnabled: false, SearchEnabled: true}

	if semantic.FeedsEngine() {
		t.Fatal("writes are off, so nothing should be offered to the engine")
	}
	if !semantic.AnswersSearch() {
		t.Fatal("stopping the writes must not stop search over what is already stored")
	}
	if !semantic.UsesEngine() {
		t.Fatal("search still needs the engine connection, so it must still be built")
	}
}

// TestEachDirectionCanBeStoppedOnItsOwn covers the four combinations, so a later
// change that folds one setting back into the other fails here.
func TestEachDirectionCanBeStoppedOnItsOwn(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name        string
		semantic    ConversationSemanticConfig
		wantFeeds   bool
		wantAnswers bool
		wantUses    bool
	}{
		{
			name:        "both on",
			semantic:    ConversationSemanticConfig{IngestionEnabled: true, SearchEnabled: true},
			wantFeeds:   true,
			wantAnswers: true,
			wantUses:    true,
		},
		{
			name:        "writes on, search off",
			semantic:    ConversationSemanticConfig{IngestionEnabled: true, SearchEnabled: false},
			wantFeeds:   true,
			wantAnswers: false,
			wantUses:    true,
		},
		{
			name:        "writes off, search on",
			semantic:    ConversationSemanticConfig{IngestionEnabled: false, SearchEnabled: true},
			wantFeeds:   false,
			wantAnswers: true,
			wantUses:    true,
		},
		{
			name:        "both off",
			semantic:    ConversationSemanticConfig{IngestionEnabled: false, SearchEnabled: false},
			wantFeeds:   false,
			wantAnswers: false,
			wantUses:    false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := testCase.semantic.FeedsEngine(); got != testCase.wantFeeds {
				t.Fatalf("FeedsEngine() = %v, want %v", got, testCase.wantFeeds)
			}
			if got := testCase.semantic.AnswersSearch(); got != testCase.wantAnswers {
				t.Fatalf("AnswersSearch() = %v, want %v", got, testCase.wantAnswers)
			}
			if got := testCase.semantic.UsesEngine(); got != testCase.wantUses {
				t.Fatalf("UsesEngine() = %v, want %v", got, testCase.wantUses)
			}
		})
	}
}

func TestAnUnwrittenSearchSettingMeansOff(t *testing.T) {
	t.Parallel()

	semantic := ConversationSemanticConfig{}
	if semantic.AnswersSearch() {
		t.Fatal("an unwritten search setting must mean off")
	}
}
