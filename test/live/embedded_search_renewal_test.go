//go:build live

package live

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLiveEmbeddedActivePagingRenewsInactivity(t *testing.T) {
	home, _, expected := writeEmbeddedPublicCorpus(t)
	harness, configuration := newEmbeddedPublicHarness(t, home)
	harness.boot(t)
	waitEmbeddedPublicPublication(t, harness)
	harness.teardown(t)
	configuration.Conversation.Semantic.IngestionEnabled = false
	configuration.Conversation.Semantic.SearchEnabled = true
	writeEmbeddedLifecycleConfig(t, harness, configuration)
	harness.boot(t)
	waitEmbeddedPublicSearch(t, harness, home)
	baseline := traverseEmbeddedPublicCLI(t, harness, home, expected)
	idle, err := embeddedPublicCLI(t, harness, home, "", 1)
	if err != nil || !idle.HasMore || idle.NextCursor == "" {
		t.Fatalf("idle first page: %+v, %v", idle, err)
	}
	active, err := embeddedPublicCLI(t, harness, home, "", 1)
	if err != nil || !active.HasMore || active.NextCursor == "" {
		t.Fatalf("active first page: %+v, %v", active, err)
	}
	identities := validateEmbeddedPublicPage(t, active, expected)
	started := time.Now()
	for continuation := range 11 {
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(time.Minute):
		}
		previous := active.NextCursor
		active, err = embeddedPublicCLI(t, harness, home, previous, 1)
		if err != nil || !active.HasMore || active.NextCursor == "" || active.NextCursor == previous {
			t.Fatalf("active continuation %d: %+v, %v", continuation+1, active, err)
		}
		identities = append(identities, validateEmbeddedPublicPage(t, active, expected)...)
		t.Logf("continuation %d succeeded after %s", continuation+1, time.Since(started))
	}
	if time.Since(started) <= 10*time.Minute {
		t.Fatal("active traversal did not exceed ten minutes")
	}
	if _, err := embeddedPublicCLI(t, harness, home, idle.NextCursor, 1); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("inactive continuation: %v, want expiration", err)
	}
	for active.HasMore {
		active, err = embeddedPublicCLI(t, harness, home, active.NextCursor, 5)
		if err != nil {
			t.Fatal(err)
		}
		identities = append(identities, validateEmbeddedPublicPage(t, active, expected)...)
		if len(identities) > len(expected) {
			t.Fatal("active traversal repeated results")
		}
	}
	if !slices.Equal(identities, baseline) {
		t.Fatalf("active traversal changed ordered identities: %v / %v", identities, baseline)
	}
}
