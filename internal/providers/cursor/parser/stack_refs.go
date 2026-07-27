package parser

import (
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

// These references keep the split Cursor foundation slice reachable under the
// deadcode gate before a later stack branch wires the provider into the daemon
// conversation registry.
var (
	_ = New
	_ = (*Parser).Provider
	_ = (*Parser).Discover
	_ = (*Parser).ScanRecord
	_ = (*Parser).Stream
	_ = RootHash
	_ = BuildVirtualPath
	_ = ParseVirtualPath

	_ = cursorjsonl.ResolveProjectRoots
	_ = cursorjsonl.MatchTranscriptFile
	_ = cursorjsonl.DiscoverTranscriptFiles
	_ = cursorjsonl.WorkspacePathFromProjectKey
	_ = cursorjsonl.ScanHeader
	_ = cursorjsonl.StreamMessages
	_ = cursorstore.ResolveDataRoots
	_ = cursorstore.ResolveDataRootsFromEnv
	_ = cursorstore.DataRoot.ListWorkspaceEntries
	_ = cursorstore.ReadWorkspaceFolderPath
	_ = cursorstore.OpenReadOnlyDatabase
	_ = cursorstore.TableExists
	_ = cursorstore.ReadKVValue
	_ = cursorstore.ReadKVRowsByPrefix
	_ = cursorstore.DecodeComposerHeaderJSON
	_ = cursorstore.ReadComposerHeader
	_ = cursorstore.ListComposerIDs
	_ = cursorstore.DecodeBackgroundComposerWindowMappingJSON
	_ = cursorstore.ListBackgroundComposers
	_ = cursorstore.DecodeBubbleJSON
	_ = cursorstore.DecodeLegacyChatDataJSON
	_ = cursorstore.ReadLegacyChatData
	_ = cursorstore.DecodeAllComposersJSON
	_ = cursorstore.BuildWorkspaceComposerIndex
)

func init() {
	var jsonlDecodeError cursorjsonl.DecodeError
	var unknownKVTableNameError cursorstore.UnknownKVTableNameError
	var cursorJSONDecodeError cursorstore.CursorJSONDecodeError

	_ = jsonlDecodeError.Error
	_ = jsonlDecodeError.Unwrap
	_ = unknownKVTableNameError.Error
	_ = cursorJSONDecodeError.Error
	_ = cursorJSONDecodeError.Unwrap
}
