package cursorstore

import (
	"encoding/json"
	"fmt"
)

// CursorJSONDecodeError reports one failure while decoding Cursor's
// undocumented, version-pinned on-disk JSON format.
type CursorJSONDecodeError struct {
	Description string
	Err         error
}

// Error renders the Cursor JSON decode failure.
func (err CursorJSONDecodeError) Error() string {
	return fmt.Sprintf("decode cursor %s json: %v", err.Description, err.Err)
}

// Unwrap exposes the underlying JSON decode failure.
func (err CursorJSONDecodeError) Unwrap() error {
	return err.Err
}

// DecodeComposerHeaderJSON decodes one Cursor composerData value.
func DecodeComposerHeaderJSON(data []byte) (ComposerHeader, error) {
	var header ComposerHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return ComposerHeader{}, CursorJSONDecodeError{Description: "composer header", Err: err}
	}
	return header, nil
}

func decodeWorkspaceDescriptorJSON(data []byte) (workspaceDescriptor, error) {
	var descriptor workspaceDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return workspaceDescriptor{}, CursorJSONDecodeError{Description: "workspace descriptor", Err: err}
	}
	return descriptor, nil
}
