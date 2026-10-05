package codestore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"

	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	recordUpsert byte = 1
	recordDelete byte = 2

	cellNull  byte = 0
	cellValue byte = 1

	// frameHeaderBytes is the payload length and the CRC-32 of the payload.
	frameHeaderBytes = 8
)

var errMalformedRecord = errors.New("malformed row record")

// storedCell is one declared column value of a row record.
type storedCell struct {
	column int
	value  collection.ScalarValue
}

// rowRecord is one upsert or delete in the row log. An upsert stores the offset
// and length of its content and metadata blob in the data file.
type rowRecord struct {
	kind          byte
	id            string
	code          staticembed.Code
	relativePath  string
	splitPart     int32
	splitRecorded bool
	dataOffset    int64
	dataLength    int64
	cells         []storedCell
}

func emptyRecord(kind byte, id string) rowRecord {
	return rowRecord{
		kind:          kind,
		id:            id,
		code:          staticembed.Code{},
		relativePath:  "",
		splitPart:     0,
		splitRecorded: false,
		dataOffset:    0,
		dataLength:    0,
		cells:         nil,
	}
}

func appendString(buffer []byte, value string) []byte {
	buffer = binary.AppendVarint(buffer, int64(len(value)))
	return append(buffer, value...)
}

func appendFrame(buffer []byte, payload []byte) ([]byte, error) {
	length := len(payload)
	if length > math.MaxUint32 {
		return nil, fmt.Errorf("row record of %d bytes exceeds the frame limit", length)
	}
	buffer = binary.LittleEndian.AppendUint32(buffer, uint32(length))
	buffer = binary.LittleEndian.AppendUint32(buffer, crc32.ChecksumIEEE(payload))
	return append(buffer, payload...), nil
}

func encodeRecord(record rowRecord, columns []collection.ScalarColumn) ([]byte, error) {
	payload := make([]byte, 0, 256)
	payload = append(payload, record.kind)
	payload = appendString(payload, record.id)
	if record.kind == recordDelete {
		return payload, nil
	}
	for _, word := range record.code {
		payload = binary.LittleEndian.AppendUint64(payload, word)
	}
	payload = appendString(payload, record.relativePath)
	payload = binary.AppendVarint(payload, int64(record.splitPart))
	if record.splitRecorded {
		payload = append(payload, 1)
	} else {
		payload = append(payload, 0)
	}
	payload = binary.AppendVarint(payload, record.dataOffset)
	payload = binary.AppendVarint(payload, record.dataLength)
	payload = binary.AppendVarint(payload, int64(len(record.cells)))
	for _, cell := range record.cells {
		payload = binary.AppendVarint(payload, int64(cell.column))
		if cell.value.Null {
			payload = append(payload, cellNull)
			continue
		}
		payload = append(payload, cellValue)
		switch columns[cell.column].Type {
		case collection.ScalarTypeString:
			payload = appendString(payload, cell.value.String)
		case collection.ScalarTypeBool:
			if cell.value.Bool {
				payload = append(payload, 1)
			} else {
				payload = append(payload, 0)
			}
		case collection.ScalarTypeInt64:
			payload = binary.AppendVarint(payload, cell.value.Int64)
		default:
			return nil, fmt.Errorf("column %s has unsupported type %q", columns[cell.column].Name, columns[cell.column].Type)
		}
	}
	return payload, nil
}

type decoder struct {
	data []byte
	err  error
}

func (reader *decoder) byteValue() byte {
	if reader.err != nil || len(reader.data) == 0 {
		reader.err = errMalformedRecord
		return 0
	}
	value := reader.data[0]
	reader.data = reader.data[1:]
	return value
}

func (reader *decoder) varint() int64 {
	if reader.err != nil {
		return 0
	}
	value, read := binary.Varint(reader.data)
	if read <= 0 {
		reader.err = errMalformedRecord
		return 0
	}
	reader.data = reader.data[read:]
	return value
}

// count reads a varint that must lie in [0, limit].
func (reader *decoder) count(limit int64) int {
	value := reader.varint()
	if value < 0 || value > limit {
		reader.err = errMalformedRecord
		return 0
	}
	return int(value)
}

func (reader *decoder) int32Value() int32 {
	value := reader.varint()
	if value < math.MinInt32 || value > math.MaxInt32 {
		reader.err = errMalformedRecord
		return 0
	}
	return int32(value)
}

func (reader *decoder) stringValue() string {
	length := reader.count(int64(len(reader.data)))
	if reader.err != nil {
		return ""
	}
	value := string(reader.data[:length])
	reader.data = reader.data[length:]
	return value
}

func decodeRecord(payload []byte, columns []collection.ScalarColumn) (rowRecord, error) {
	reader := &decoder{data: payload, err: nil}
	kind := reader.byteValue()
	record := emptyRecord(kind, reader.stringValue())
	if record.kind == recordDelete {
		return record, reader.err
	}
	if record.kind != recordUpsert || len(reader.data) < len(record.code)*8 {
		return record, errMalformedRecord
	}
	for word := range record.code {
		record.code[word] = binary.LittleEndian.Uint64(reader.data[word*8:])
	}
	reader.data = reader.data[len(record.code)*8:]
	record.relativePath = reader.stringValue()
	record.splitPart = reader.int32Value()
	record.splitRecorded = reader.byteValue() == 1
	record.dataOffset = int64(reader.count(math.MaxInt64))
	record.dataLength = int64(reader.count(math.MaxInt64))
	cellCount := reader.count(int64(len(columns)))
	if reader.err != nil {
		return record, reader.err
	}
	record.cells = make([]storedCell, 0, cellCount)
	for range cellCount {
		column := reader.count(int64(len(columns) - 1))
		if reader.err != nil {
			return record, reader.err
		}
		columnType := columns[column].Type
		cell := storedCell{column: column, value: collection.ScalarValue{Type: columnType, Null: true, String: "", Bool: false, Int64: 0}}
		if reader.byteValue() == cellValue {
			cell.value.Null = false
			switch columnType {
			case collection.ScalarTypeString:
				cell.value.String = reader.stringValue()
			case collection.ScalarTypeBool:
				cell.value.Bool = reader.byteValue() == 1
			case collection.ScalarTypeInt64:
				cell.value.Int64 = reader.varint()
			default:
				return record, errMalformedRecord
			}
		}
		record.cells = append(record.cells, cell)
	}
	return record, reader.err
}

// encodeBlob writes the content and metadata of one row as stored in the data
// file.
func encodeBlob(content string, metadata string) []byte {
	blob := make([]byte, 0, len(content)+len(metadata)+8)
	blob = appendString(blob, content)
	return appendString(blob, metadata)
}

func decodeBlob(blob []byte) (string, string, error) {
	reader := &decoder{data: blob, err: nil}
	content := reader.stringValue()
	metadata := reader.stringValue()
	return content, metadata, reader.err
}
