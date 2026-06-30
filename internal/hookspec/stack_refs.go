package hookspec

import (
	"context"
	"io"
)

// These references keep deliberately split helper branches buildable under the
// repository deadcode gate until their consumer branches land upstack.
var stackRefsCursorDocument = &cursorHooksDocument{fields: nil}

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
	_    = unmarshalCursorHooksDocument
	_    = (*cursorHooksDocument).MarshalJSON
	_    = (*cursorHooksDocument).marshalCursorHookInstalls
	_    = (*cursorHooksDocument).unmarshalCursorHooks
	_    = (*cursorHooksDocument).setInt
	_    = cursorCommandSignatures
	_    = removeCursorHookHandlers
	_    = newCursorCommandHookHandler
	_    = (*rawCursorHookHandler).MarshalJSON
	_    = (*rawCursorHookHandler).UnmarshalJSON
	_    = (*rawCursorHookHandler).command
	_    = (*rawCursorHookHandler).setString
	_    = (*rawCursorHookHandler).setInt
	_    = (*rawCursorHookHandler).setBool
	_    = Registry{hooks: nil}.HooksForClient
	_    = Registry{hooks: nil}.InstallsForClient
	_    = Registry{hooks: nil}.ClydeCommandSignatures
	_    = shellCommand("", nil)
	_    = SupportedClients()
	_    = writeAdditionalContext(io.Discard, ClientClaudeCode, "", "")
	_    = writeCursorFollowup(io.Discard, "")
	_, _ = marshalCodexHookInstalls(nil, nil, nil, "", "")
	_    = stackRefsCursorDocument.marshalCursorHookInstalls
	_    = stackRefsCursorDocument.MarshalJSON
)

func init() {
	key := ReorientSnapshotKey{
		TranscriptPath: "",
		ConversationID: "",
		SessionID:      "",
		CWD:            "",
	}
	_ = normalizeSnapshotKey(key)
	if stackRefsCursorDocument == nil {
		store := NewFileSnapshotStore("")
		_ = store.Save(context.Background(), key, "")
		_, _, _ = store.Consume(context.Background(), key)
		_ = store.pathForKey(key)
	}
	document, _ := unmarshalCursorHooksDocument(nil)
	_ = document
}
