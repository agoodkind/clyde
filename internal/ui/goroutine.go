package ui

func logUIGoroutinePanic(name string, recoveredValue string) {
	tuiLog.Logger().Error("tui.goroutine.panic",
		"component", "tui",
		"goroutine", name,
		"recover", recoveredValue,
		"err", "panic")
}

// asyncLoadEvent is the sealed marker interface implemented by the
// loaded-event payloads that postInterruptIfActive forwards. The
// interface enumerates the payload union so the screen post path stays
// off the open `any` channel.
type asyncLoadEvent interface {
	isAsyncLoadEvent()
}

func (sessionsLoaded) isAsyncLoadEvent()       {}
func (configControlsLoaded) isAsyncLoadEvent() {}
func (exportStatsLoaded) isAsyncLoadEvent()    {}
func (detailsLoaded) isAsyncLoadEvent()        {}
func (accountStatusLoaded) isAsyncLoadEvent()  {}

// postInterruptIfActive forwards data to the screen via postInterrupt
// only when the App context has not been canceled. Async TUI workers
// that block on daemon RPCs must check this before posting. A slow
// callback that returns after Run exits would otherwise race against
// teardown.
//
// The data parameter is restricted to asyncLoadEvent, the sealed
// marker interface implemented by the four loaded-event payload
// structs. New async load callbacks must add their payload to the
// asyncLoadEvent union before calling this helper.
func (a *App) postInterruptIfActive(name string, data asyncLoadEvent) {
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
