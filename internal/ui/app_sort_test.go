package ui

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

func TestSortSessionsStableTieBreakForLastUsed(t *testing.T) {
	now := time.Date(2026, 4, 24, 19, 0, 0, 0, time.UTC)
	a := NewApp([]*session.Session{
		{Name: "zeta", Metadata: session.Metadata{LastAccessed: now}},
		{Name: "alpha", Metadata: session.Metadata{LastAccessed: now}},
	}, AppCallbacks{})
	a.sortCol = SortColUsed
	a.sortAsc = false

	a.sortSessions()

	if len(a.sessions) != 2 {
		t.Fatalf("sessions=%d want 2", len(a.sessions))
	}
	if a.sessions[0].Name != "alpha" || a.sessions[1].Name != "zeta" {
		t.Fatalf("unexpected order: %s, %s", a.sessions[0].Name, a.sessions[1].Name)
	}
}

func TestSortSessionsUsesTitleForNameColumn(t *testing.T) {
	a := NewApp([]*session.Session{
		{
			Name: "z-slug",
			Metadata: session.Metadata{
				Name:  "z-slug",
				Title: "Alpha Human",
			},
		},
		{
			Name: "a-slug",
			Metadata: session.Metadata{
				Name:  "a-slug",
				Title: "Zulu Human",
			},
		},
	}, AppCallbacks{})
	a.sortCol = SortColName
	a.sortAsc = true

	a.sortSessions()

	if len(a.sessions) != 2 {
		t.Fatalf("sessions=%d want 2", len(a.sessions))
	}
	if a.sessions[0].Name != "z-slug" || a.sessions[1].Name != "a-slug" {
		t.Fatalf("order = %q, %q; want display-title sort", a.sessions[0].Name, a.sessions[1].Name)
	}
}

func TestDedupeSessionListPrefersNonAutoAdoptedNameForSharedSessionID(t *testing.T) {
	now := time.Date(2026, 4, 24, 19, 0, 0, 0, time.UTC)
	in := []*session.Session{
		{Name: "clyde-dev-1a4837fd", Metadata: session.Metadata{
			Name:         "clyde-dev-1a4837fd",
			SessionID:    "shared",
			LastAccessed: now.Add(2 * time.Hour),
		}},
		{Name: "unified-session-resolution", Metadata: session.Metadata{
			Name:         "unified-session-resolution",
			SessionID:    "shared",
			LastAccessed: now,
		}},
	}

	out := dedupeSessionList(in)

	if len(out) != 1 {
		t.Fatalf("sessions=%d want 1", len(out))
	}
	if out[0].Name != "unified-session-resolution" {
		t.Fatalf("winner=%q want unified-session-resolution", out[0].Name)
	}
}
