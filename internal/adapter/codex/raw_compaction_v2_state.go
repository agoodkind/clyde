package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	maxRawResponsesCompactionV2Pending = 32
	rawResponsesCompactionV2TTL        = 2 * time.Hour
)

// RawResponsesCompactionV2Registry retains bounded process-local recovery state.
type RawResponsesCompactionV2Registry struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]rawResponsesCompactionV2Entry
}

type rawResponsesCompactionV2Entry struct {
	transcript string
	created    time.Time
	claimed    bool
}

// NewRawResponsesCompactionV2Registry creates an empty recovery registry.
func NewRawResponsesCompactionV2Registry(now func() time.Time) *RawResponsesCompactionV2Registry {
	if now == nil {
		now = time.Now
	}
	return &RawResponsesCompactionV2Registry{mu: sync.Mutex{}, now: now, entries: make(map[string]rawResponsesCompactionV2Entry)}
}

// Arm stores transcript recovery under the digest of encrypted content.
func (r *RawResponsesCompactionV2Registry) Arm(sessionID, encryptedContent, transcript string) bool {
	_, ok := r.ArmWithGeneration(sessionID, encryptedContent, transcript)
	return ok
}

// ArmWithGeneration stores recovery and returns its ownership generation.
func (r *RawResponsesCompactionV2Registry) ArmWithGeneration(sessionID, encryptedContent, transcript string) (uint64, bool) {
	if r == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(encryptedContent) == "" || strings.TrimSpace(transcript) == "" {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expire(now)
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	if _, exists := r.entries[key]; exists {
		return 0, false
	}
	if len(r.entries) >= maxRawResponsesCompactionV2Pending {
		if !r.evictOldest() {
			return 0, false
		}
	}
	r.entries[key] = rawResponsesCompactionV2Entry{transcript: transcript, created: now, claimed: false}
	return r.nextGeneration, true
}

// Match atomically reserves an unexpired transcript recovery entry.
func (r *RawResponsesCompactionV2Registry) Match(sessionID, encryptedContent string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire(r.now())
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	entry, ok := r.entries[key]
	if !ok || entry.claimed {
		return "", false
	}
	entry.claimed = true
	r.entries[key] = entry
	if !ok || entry.leased {
		return "", false
	}
	return entry.transcript, true
}

// Release makes a reserved recovery entry available after persistence fails.
func (r *RawResponsesCompactionV2Registry) Release(sessionID, encryptedContent string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire(r.now())
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	entry, ok := r.entries[key]
	if !ok || !entry.claimed {
		return false
	}
	entry.claimed = false
	r.entries[key] = entry
	return true
}

// Complete removes a reserved recovery entry after successful persistence.
func (r *RawResponsesCompactionV2Registry) Complete(sessionID, encryptedContent string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	entry, ok := r.entries[key]
	if !ok || !entry.claimed {
		return false
	}
	delete(r.entries, key)
	return true
}

func (r *RawResponsesCompactionV2Registry) expire(now time.Time) {
	for key, entry := range r.entries {
		if now.Sub(entry.created) >= rawResponsesCompactionV2TTL {
			delete(r.entries, key)
		}
	}
}

func (r *RawResponsesCompactionV2Registry) evictOldest() bool {
	var oldestKey string
	var oldest time.Time
	for key, entry := range r.entries {
		if entry.leased {
			continue
		}
		if oldestKey == "" || entry.created.Before(oldest) {
			oldestKey, oldest = key, entry.created
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(r.entries, oldestKey)
	return true
}

func rawResponsesCompactionV2Key(sessionID, encryptedContent string) string {
	sum := sha256.Sum256([]byte(encryptedContent))
	return sessionID + ":" + hex.EncodeToString(sum[:])
}
