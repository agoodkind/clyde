package codex

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRawResponsesCompactionV2RegistryLifecycle(t *testing.T) {
	now := time.Unix(1, 0)
	registry := NewRawResponsesCompactionV2Registry(func() time.Time { return now })
	if !registry.Arm("s", "encrypted", "transcript") {
		t.Fatal("Arm() = false")
	}
	if transcript, ok := registry.Match("s", "encrypted"); !ok || transcript != "transcript" {
		t.Fatalf("Match() = %q, %v", transcript, ok)
	}
	if registry.Arm("s", "encrypted", "other") {
		t.Fatal("duplicate Arm() succeeded")
	}
	if !registry.Complete("s", "encrypted") {
		t.Fatal("Complete() = false")
	}
	if _, ok := registry.Match("s", "encrypted"); ok {
		t.Fatal("completed entry remained")
	}
}

func TestRawResponsesCompactionV2RegistryReservesAndReleasesAtomically(t *testing.T) {
	registry := NewRawResponsesCompactionV2Registry(nil)
	if !registry.Arm("s", "encrypted", "transcript") {
		t.Fatal("Arm() = false")
	}
	var reservations int
	var generation uint64
	var group sync.WaitGroup
	var matchesMu sync.Mutex
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, reservedGeneration, ok := registry.Reserve("s", "encrypted"); ok {
				matchesMu.Lock()
				reservations++
				generation = reservedGeneration
				matchesMu.Unlock()
			}
		}()
	}
	group.Wait()
	if reservations != 1 {
		t.Fatalf("reservations = %d, want 1", reservations)
	}
	registry.Release("s", "encrypted", generation)
	if transcript, ok := registry.Match("s", "encrypted"); !ok || transcript != "transcript" {
		t.Fatalf("Match() after release = %q, %t", transcript, ok)
	}
	_, generation, ok := registry.Reserve("s", "encrypted")
	if !ok || !registry.Complete("s", "encrypted", generation) {
		t.Fatal("Complete() = false")
	}
}

func TestRawResponsesCompactionV2RegistryDisarmKeepsUnleasedEntry(t *testing.T) {
	registry := NewRawResponsesCompactionV2Registry(nil)
	if !registry.Arm("s", "encrypted", "transcript") {
		t.Fatal("Arm() = false")
	}
	registry.Disarm("s", "encrypted", 1)
	if transcript, ok := registry.Match("s", "encrypted"); !ok || transcript != "transcript" {
		t.Fatalf("Match() after unleased Disarm = %q, %v", transcript, ok)
	}
}

func TestRawResponsesCompactionV2RegistryDiscardRemovesUnleasedEntry(t *testing.T) {
	registry := NewRawResponsesCompactionV2Registry(nil)
	generation, ok := registry.ArmWithGeneration("s", "encrypted", "transcript")
	if !ok {
		t.Fatal("ArmWithGeneration() = false")
	}
	registry.Discard("s", "encrypted", generation)
	if _, ok := registry.Match("s", "encrypted"); ok {
		t.Fatal("Discard() kept unleased entry")
	}
}

func TestRawResponsesCompactionV2RegistryExpiresAndEvicts(t *testing.T) {
	now := time.Unix(1, 0)
	registry := NewRawResponsesCompactionV2Registry(func() time.Time { return now })
	if !registry.Arm("s", "one", "one") {
		t.Fatal("arm one")
	}
	now = now.Add(rawResponsesCompactionV2TTL)
	if _, ok := registry.Match("s", "one"); ok {
		t.Fatal("expired entry matched")
	}
	for index := range maxRawResponsesCompactionV2Pending {
		if !registry.Arm("s", fmt.Sprintf("encrypted-%d", index), "x") {
			t.Fatal("arm capacity")
		}
		now = now.Add(time.Second)
	}
	if !registry.Arm("s", "new", "new") {
		t.Fatal("arm eviction")
	}
	if _, ok := registry.Match("s", "encrypted-0"); ok {
		t.Fatal("oldest entry survived eviction")
	}
}
