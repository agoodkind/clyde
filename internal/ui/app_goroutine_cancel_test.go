package ui

import (
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"goodkind.io/clyde/internal/session"
)

// TestRequestSessionsAsync_PostsInterrupt confirms that the async sessions
// loader invokes the callback and posts a sessionsLoaded interrupt.
func TestRequestSessionsAsync_PostsInterrupt(t *testing.T) {
	callbackEnteredAt := make(chan struct{})
	cb := AppCallbacks{
		ListSessions: func() (SessionSnapshot, error) {
			close(callbackEnteredAt)
			return SessionSnapshot{}, nil
		},
	}

	a := NewApp(nil, cb)
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)
	a.screen = scr

	a.requestSessionsAsync("test")

	select {
	case <-callbackEnteredAt:
	case <-time.After(2 * time.Second):
		t.Fatal("ListSessions callback was never invoked")
	}
	time.Sleep(50 * time.Millisecond)
	for scr.HasPendingEvent() {
		ev := scr.PollEvent()
		ei, ok := ev.(*tcell.EventInterrupt)
		if !ok {
			continue
		}
		if _, isLoaded := ei.Data().(sessionsLoaded); isLoaded {
			return
		}
	}
	t.Fatal("sessionsLoaded interrupt was never posted")
}

// TestLoadDetailAsync_PostsInterrupt confirms the detail-load async path posts
// a detailsLoaded interrupt with the current callback contract.
func TestLoadDetailAsync_PostsInterrupt(t *testing.T) {
	sess := &session.Session{
		Name:     "detail-test",
		Metadata: session.Metadata{Name: "detail-test"},
	}

	callbackEnteredAt := make(chan struct{})
	cb := AppCallbacks{
		GetSessionDetail: func(_ *session.Session) (SessionDetail, error) {
			close(callbackEnteredAt)
			return SessionDetail{}, nil
		},
	}

	a := NewApp([]*session.Session{sess}, cb)
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)
	a.screen = scr

	a.loadDetailAsync(sess)

	select {
	case <-callbackEnteredAt:
	case <-time.After(2 * time.Second):
		t.Fatal("GetSessionDetail callback was never invoked")
	}
	time.Sleep(50 * time.Millisecond)
	for scr.HasPendingEvent() {
		ev := scr.PollEvent()
		ei, ok := ev.(*tcell.EventInterrupt)
		if !ok {
			continue
		}
		if _, isLoaded := ei.Data().(detailsLoaded); isLoaded {
			return
		}
	}
	t.Fatal("detailsLoaded interrupt was never posted")
}

// TestRowFor_ConcurrentAccessNoRace exercises rowFor from multiple
// goroutines so -race surfaces any unguarded access to the maps
// rowFor touches. The test is a regression guard: if someone removes
// or weakens tableRowDataMu, race mode should flag this test.
func TestRowFor_ConcurrentAccessNoRace(t *testing.T) {
	sessions := []*session.Session{
		{Name: "row-test-a", Metadata: session.Metadata{Name: "row-test-a", LastAccessed: time.Now()}},
		{Name: "row-test-b", Metadata: session.Metadata{Name: "row-test-b", LastAccessed: time.Now()}},
	}
	a := NewApp(sessions, AppCallbacks{})
	a.modelCache["row-test-a"] = "opus"
	a.modelCache["row-test-b"] = "sonnet"
	a.messageCountCache["row-test-a"] = 3
	a.messageCountCache["row-test-b"] = 7
	a.recentlyUpdatedAt["row-test-a"] = time.Now()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sess := sessions[idx%len(sessions)]
			for j := 0; j < 100; j++ {
				_ = a.rowFor(sess)
			}
		}(i)
	}
	wg.Wait()
}
