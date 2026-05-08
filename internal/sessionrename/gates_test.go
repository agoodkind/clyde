package sessionrename

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

func TestEvaluateGates_LiveLeaseDefersRename(t *testing.T) {
	sess := &session.Session{
		Name:     "demo",
		Metadata: session.Metadata{Name: "demo"},
	}
	res := EvaluateGates(sess, 5, true, time.Now(), GatesConfig{MinUserMessages: 3, Cooldown: 30 * time.Minute})
	if res.Allow {
		t.Fatalf("Allow=true with live lease, want false")
	}
	if res.Reason != "live_lease_held" {
		t.Fatalf("Reason=%q want live_lease_held", res.Reason)
	}
}

func TestEvaluateGates_UserOwnedNameLocked(t *testing.T) {
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:           "demo",
			AutoNameSource: session.AutoNameSourceUser,
		},
	}
	res := EvaluateGates(sess, 5, false, time.Now(), GatesConfig{MinUserMessages: 3})
	if res.Allow {
		t.Fatalf("Allow=true on user-owned, want false")
	}
	if res.Reason != "user_owned" {
		t.Fatalf("Reason=%q want user_owned", res.Reason)
	}
}

func TestEvaluateGates_UserLockedState(t *testing.T) {
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:          "demo",
			AutoNameState: session.AutoNameStateUserLocked,
		},
	}
	res := EvaluateGates(sess, 5, false, time.Now(), GatesConfig{MinUserMessages: 3})
	if res.Allow || res.Reason != "user_locked" {
		t.Fatalf("got Allow=%v Reason=%q want false/user_locked", res.Allow, res.Reason)
	}
}

func TestEvaluateGates_BelowMinUserMessages(t *testing.T) {
	sess := &session.Session{Name: "demo", Metadata: session.Metadata{Name: "demo"}}
	res := EvaluateGates(sess, 2, false, time.Now(), GatesConfig{MinUserMessages: 3})
	if res.Allow || res.Reason != "min_user_messages" {
		t.Fatalf("got Allow=%v Reason=%q want false/min_user_messages", res.Allow, res.Reason)
	}
}

func TestEvaluateGates_CooldownActive(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:           "demo",
			LastAutoNameAt: now.Add(-5 * time.Minute),
		},
	}
	res := EvaluateGates(sess, 5, false, now, GatesConfig{MinUserMessages: 3, Cooldown: 30 * time.Minute})
	if res.Allow || res.Reason != "cooldown_active" {
		t.Fatalf("got Allow=%v Reason=%q want false/cooldown_active", res.Allow, res.Reason)
	}
}

func TestEvaluateGates_CooldownElapsed(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	sess := &session.Session{
		Name: "demo",
		Metadata: session.Metadata{
			Name:           "demo",
			LastAutoNameAt: now.Add(-31 * time.Minute),
		},
	}
	res := EvaluateGates(sess, 5, false, now, GatesConfig{MinUserMessages: 3, Cooldown: 30 * time.Minute})
	if !res.Allow {
		t.Fatalf("got Allow=false Reason=%q want true after cooldown", res.Reason)
	}
}

func TestEvaluateGates_AllPass(t *testing.T) {
	sess := &session.Session{Name: "demo", Metadata: session.Metadata{Name: "demo"}}
	res := EvaluateGates(sess, 3, false, time.Now(), GatesConfig{MinUserMessages: 3})
	if !res.Allow {
		t.Fatalf("got Allow=false Reason=%q want true", res.Reason)
	}
}

func TestEvaluateGates_NilSession(t *testing.T) {
	res := EvaluateGates(nil, 0, false, time.Now(), GatesConfig{})
	if res.Allow || res.Reason != "nil_session" {
		t.Fatalf("got Allow=%v Reason=%q want false/nil_session", res.Allow, res.Reason)
	}
}
