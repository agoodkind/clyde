package config

import "testing"

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

	semantic := ConversationSemanticConfig{Enabled: false}

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

	yes := true
	no := false

	for _, testCase := range []struct {
		name        string
		semantic    ConversationSemanticConfig
		wantFeeds   bool
		wantAnswers bool
		wantUses    bool
	}{
		{
			name:        "both on",
			semantic:    ConversationSemanticConfig{Enabled: true, SearchEnabled: &yes},
			wantFeeds:   true,
			wantAnswers: true,
			wantUses:    true,
		},
		{
			name:        "writes on, search off",
			semantic:    ConversationSemanticConfig{Enabled: true, SearchEnabled: &no},
			wantFeeds:   true,
			wantAnswers: false,
			wantUses:    true,
		},
		{
			name:        "writes off, search on",
			semantic:    ConversationSemanticConfig{Enabled: false, SearchEnabled: &yes},
			wantFeeds:   false,
			wantAnswers: true,
			wantUses:    true,
		},
		{
			name:        "both off",
			semantic:    ConversationSemanticConfig{Enabled: false, SearchEnabled: &no},
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

// TestAnUnwrittenSearchSettingMeansOn pins the default, which is what makes this
// change safe for a config that predates it: an operator who wrote only
// `enabled = false` gets their search back without editing anything.
func TestAnUnwrittenSearchSettingMeansOn(t *testing.T) {
	t.Parallel()

	semantic := ConversationSemanticConfig{}
	if semantic.SearchEnabled != nil {
		t.Fatal("the fixture should leave the setting unwritten")
	}
	if !semantic.AnswersSearch() {
		t.Fatal("an unwritten search setting must mean on")
	}

	explicit := false
	semantic.SearchEnabled = &explicit
	if semantic.AnswersSearch() {
		t.Fatal("an explicit false must still turn search off, or the setting does nothing")
	}
}
