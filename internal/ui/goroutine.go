package ui

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
// teardown. Returns true when the interrupt was queued. Returns false
// when the context was already canceled.
//
// The data parameter mirrors postInterrupt's existing signature for
// the loaded-event payload union. Tightening that union to a typed
// sum is a follow-up beyond this change.
func (a *App) postInterruptIfActive(name string, data any) bool {
	if a == nil || a.ctx == nil {
		a.postInterrupt(data)
		return true
	}
	select {
	case <-a.ctx.Done():
		tuiLog.Logger().Debug("tui.goroutine.canceled",
			"component", "tui",
			"goroutine", name,
			"err", a.ctx.Err())
		return false
	default:
	}
	a.postInterrupt(data)
	return true
}
