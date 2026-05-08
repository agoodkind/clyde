package ui

// tuiEventPayload is the sealed marker interface for event values that
// async worker goroutines post back to the TUI event loop via
// postInterruptIfActive. Each event struct implements the unexported
// marker so the helper accepts only known payload shapes; callers are
// type-checked at compile time and the helper signature avoids any.
type tuiEventPayload interface {
	isTUIEventPayload()
}

func (sessionsLoaded) isTUIEventPayload()       {}
func (configControlsLoaded) isTUIEventPayload() {}
func (exportStatsLoaded) isTUIEventPayload()    {}
func (detailsLoaded) isTUIEventPayload()        {}

func logUIGoroutinePanic(name string, recoveredValue string) {
	tuiLog.Logger().Error("tui.goroutine.panic",
		"component", "tui",
		"goroutine", name,
		"recover", recoveredValue,
		"err", "panic")
}

// postInterruptIfActive forwards data to the screen via postInterrupt
// only when the App context has not been canceled. Async TUI workers
// that block on daemon RPCs must check this before posting. A slow
// callback that returns after Run exits would otherwise race against
// teardown. The data parameter is restricted to types in the
// tuiEventPayload sealed union so the helper avoids a generic any
// channel value.
func (a *App) postInterruptIfActive(name string, data tuiEventPayload) {
	if a == nil || a.ctx == nil {
		a.postInterrupt(data)
		return
	}
	select {
	case <-a.ctx.Done():
		tuiLog.Logger().Debug("tui.goroutine.canceled",
			"component", "tui",
			"goroutine", name,
			"err", a.ctx.Err())
		return
	default:
	}
	a.postInterrupt(data)
}
