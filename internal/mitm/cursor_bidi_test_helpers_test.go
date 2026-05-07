package mitm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

func appendProtoString(dst []byte, fieldNumber uint64, value string) []byte {
	dst = appendProtoKey(dst, fieldNumber, 2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendProtoBytes(dst []byte, fieldNumber uint64, value []byte) []byte {
	dst = appendProtoKey(dst, fieldNumber, 2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendProtoVarint(dst []byte, fieldNumber uint64, value uint64) []byte {
	dst = appendProtoKey(dst, fieldNumber, 0)
	return binary.AppendUvarint(dst, value)
}

func appendProtoKey(dst []byte, fieldNumber uint64, wireType uint64) []byte {
	return binary.AppendUvarint(dst, fieldNumber<<3|wireType)
}

func mustCursorDiagnosticJSON(diag cursorBidiAppendDiagnostic) []byte {
	raw, err := json.Marshal(diag)
	if err != nil {
		panic(fmt.Sprintf("marshal cursor diagnostic: %v", err))
	}
	return raw
}
