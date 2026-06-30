package hookspec

var (
	_ = shellCommand
	_ = shellQuote
	_ = isShellBareWord
	_ = SupportedClients
	_ = writeAdditionalContext
	_ = writeCursorFollowup
	_ = NewFileSnapshotStore
	_ = FileSnapshotStore.Save
	_ = FileSnapshotStore.Consume
	_ = normalizeSnapshotKey
)

func init() {
	_ = shellCommand("", nil)
	_ = SupportedClients()
}
