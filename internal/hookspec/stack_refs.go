package hookspec

import (
	"context"
	"io"
)

var (
	_    = shellCommand
	_    = shellQuote
	_    = isShellBareWord
	_    = SupportedClients
	_    = writeAdditionalContext
	_    = writeCursorFollowup
	_    = NewFileSnapshotStore
	_    = FileSnapshotStore.Save
	_    = FileSnapshotStore.Consume
	_    = normalizeSnapshotKey
	_    = marshalCodexHookInstalls
	_    = removeCodexCommandHookGroups
	_    = isCodexHookGroupHeader
	_    = isNonHookTableHeader
	_    = codexGroupContainsCommand
	_    = parseTomlAssignment
	_    = removeCodexManagedBlock
	_    = ensureCodexHooksFeature
	_    = isTomlTableHeader
	_    = renderCodexManagedBlock
	_    = codexCommandSignatures
	_    = codexEventName
	_    = marshalCodexTrustedHash
	_    = shellCommand("", nil)
	_    = SupportedClients()
	_    = writeAdditionalContext(io.Discard, ClientClaudeCode, "", "")
	_    = writeCursorFollowup(io.Discard, "")
	_, _ = marshalCodexHookInstalls(nil, nil, nil, "", "")
)

func init() {
	key := ReorientSnapshotKey{
		TranscriptPath: "",
		ConversationID: "",
		SessionID:      "",
		CWD:            "",
	}
	_ = normalizeSnapshotKey(key)
	if SupportedClients() == nil {
		store := NewFileSnapshotStore("")
		_ = store.Save(context.Background(), key, "")
		_, _, _ = store.Consume(context.Background(), key)
		_ = store.pathForKey(key)
	}
}
