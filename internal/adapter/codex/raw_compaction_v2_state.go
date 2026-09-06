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
	mu             sync.Mutex
	now            func() time.Time
	entries        map[string]rawResponsesCompactionV2Entry
	nextGeneration uint64
}

type rawResponsesCompactionV2Entry struct {
	transcript string
	created    time.Time
	leased     bool
	generation uint64
}

// NewRawResponsesCompactionV2Registry creates an empty recovery registry.
func NewRawResponsesCompactionV2Registry(now func() time.Time) *RawResponsesCompactionV2Registry {
	if now == nil {
		now = time.Now
	}
	return &RawResponsesCompactionV2Registry{mu: sync.Mutex{}, now: now, entries: make(map[string]rawResponsesCompactionV2Entry), nextGeneration: 0}
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
	r.nextGeneration++
	r.entries[key] = rawResponsesCompactionV2Entry{transcript: transcript, created: now, leased: false, generation: r.nextGeneration}
	return r.nextGeneration, true
}

// Disarm removes a pending recovery after its terminal client write fails.
func (r *RawResponsesCompactionV2Registry) Disarm(sessionID, encryptedContent string, generation uint64) {
	if r == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(encryptedContent) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire(r.now())
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	entry, ok := r.entries[key]
	if !ok || entry.leased || entry.generation != generation {
		return
	}
	delete(r.entries, key)
}

// Match returns an unexpired transcript recovery entry.
func (r *RawResponsesCompactionV2Registry) Match(sessionID, encryptedContent string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire(r.now())
	entry, ok := r.entries[rawResponsesCompactionV2Key(sessionID, encryptedContent)]
	if !ok || entry.leased {
		return "", false
	}
	return entry.transcript, true
}

// Reserve leases an unexpired recovery entry to one regular final-answer request.
func (r *RawResponsesCompactionV2Registry) Reserve(sessionID, encryptedContent string) (string, uint64, bool) {
	if r == nil {
		return "", 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire(r.now())
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	entry, ok := r.entries[key]
	if !ok || entry.leased {
		return "", 0, false
	}
	entry.leased = true
	r.entries[key] = entry
	return entry.transcript, entry.generation, true
}

// Release makes a reserved recovery available after fail-open delivery.
func (r *RawResponsesCompactionV2Registry) Release(sessionID, encryptedContent string, generation uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	entry, ok := r.entries[key]
	if !ok || entry.generation != generation {
		return
	}
	entry.leased = false
	r.entries[key] = entry
}

// Complete removes a matched recovery entry after successful persistence.
func (r *RawResponsesCompactionV2Registry) Complete(sessionID, encryptedContent string, generation uint64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := rawResponsesCompactionV2Key(sessionID, encryptedContent)
	entry, ok := r.entries[key]
	if !ok || !entry.leased || entry.generation != generation {
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
