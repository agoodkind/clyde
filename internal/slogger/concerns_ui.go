package slogger

// UI (TUI, sidecar), MCP server, and compact concern constants.
const (
	ConcernUITUILifecycle   = "ui.tui.lifecycle"
	ConcernUITUIActions     = "ui.tui.actions"
	ConcernUITUIRenderErr   = "ui.tui.render-errors"
	ConcernUISidecarTail    = "ui.sidecar.tail"
	ConcernUISidecarSend    = "ui.sidecar.send"
	ConcernMCPServerRequest = "mcp.server.requests"
	ConcernMCPServerSearch  = "mcp.server.search"
	ConcernMCPServerContext = "mcp.server.context"
	ConcernMCPServerErrors  = "mcp.server.errors"
	ConcernCompactPreview   = "compact.preview"
	ConcernCompactApply     = "compact.apply"
	ConcernCompactUndo      = "compact.undo"
	ConcernCompactLedger    = "compact.ledger"
	ConcernCompactCalib     = "compact.calibration"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernUITUILifecycle:   "ui/tui/lifecycle.jsonl",
		ConcernUITUIActions:     "ui/tui/actions.jsonl",
		ConcernUITUIRenderErr:   "ui/tui/render-errors.jsonl",
		ConcernUISidecarTail:    "ui/sidecar/tail.jsonl",
		ConcernUISidecarSend:    "ui/sidecar/send.jsonl",
		ConcernMCPServerRequest: "mcp/server/requests.jsonl",
		ConcernMCPServerSearch:  "mcp/server/search.jsonl",
		ConcernMCPServerContext: "mcp/server/context.jsonl",
		ConcernMCPServerErrors:  "mcp/server/errors.jsonl",
		ConcernCompactPreview:   "compact/preview.jsonl",
		ConcernCompactApply:     "compact/apply.jsonl",
		ConcernCompactUndo:      "compact/undo.jsonl",
		ConcernCompactLedger:    "compact/ledger.jsonl",
		ConcernCompactCalib:     "compact/calibration.jsonl",
	})

	registerEventConcernRules([]eventConcernRule{
		{"dashboard.", ConcernUITUILifecycle},
		{"tui.sidecar.tail", ConcernUISidecarTail},
		{"tui.sidecar.send", ConcernUISidecarSend},
		{"sidecar.", ConcernUISidecarSend},
		{"tui.input.", ConcernUITUIActions},
		{"tui.event.", ConcernUITUIActions},
		{"tui.overlay.", ConcernUITUIActions},
		{"resume.row_selected", ConcernUITUIActions},
		{"returnprompt.", ConcernUITUIActions},
		{"tui.draw.", ConcernUITUIRenderErr},
		{"tui.loop.event_timing", ConcernUITUIRenderErr},
		{"tui.table.populate_timing", ConcernUITUIRenderErr},
		{"tui.signal.", ConcernUITUIRenderErr},
		{"tui.", ConcernUITUILifecycle},
		{"mcp.server.", ConcernMCPServerRequest},
		{"mcp.search.", ConcernMCPServerSearch},
		{"mcp.context.", ConcernMCPServerContext},
		{"mcp.error", ConcernMCPServerErrors},
		{"analyze_results", ConcernMCPServerSearch},
		{"compact.preview.", ConcernCompactPreview},
		{"compact.apply.", ConcernCompactApply},
		{"compact.undo.", ConcernCompactUndo},
		{"compact.ledger.", ConcernCompactLedger},
		{"compact.calibration.", ConcernCompactCalib},
		{"compact.", ConcernCmdCompact},
	})
}
