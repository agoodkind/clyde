package hookspec

import "io"

// These references keep deliberately split helper branches buildable under the
// repository deadcode gate until their consumer branches land upstack.
var stackRefsCursorDocument = &cursorHooksDocument{fields: nil}

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
	_ = writeAdditionalContext(io.Discard, ClientClaudeCode, "", "")
	_ = writeCursorFollowup(io.Discard, "")

	store := NewFileSnapshotStore(filepath.Join(os.TempDir(), "clyde-hookspec-reachability"))
	_ = store.Save(context.Background(), ReorientSnapshotKey{}, "")
	_, _, _ = store.Consume(context.Background(), ReorientSnapshotKey{})
}
