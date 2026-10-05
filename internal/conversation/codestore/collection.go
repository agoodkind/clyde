package codestore

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"

	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	headerFileName = "collection.json"
	rowsFileName   = "rows.log"
	dataFileName   = "data.log"
	directoryMode  = 0o700
	fileMode       = 0o600
	// compactMinimumDead keeps small collections from rewriting their files on
	// every delete.
	compactMinimumDead = 10000
	bufferBytes        = 1 << 20
)

// failed logs one failed store operation and returns the error with the
// operation that failed.
func failed(operation string, err error) error {
	slog.Warn("conversation.codestore.operation_failed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}

type collectionHeader struct {
	Declaration collection.Declaration `json:"declaration"`
	Dimension   int                    `json:"dimension"`
	Model       string                 `json:"model"`
}

// dictionary interns the values of one column. ID 0 marks a row without the
// column.
type dictionary struct {
	values []collection.ScalarValue
	ids    map[collection.ScalarValue]uint32
}

func newDictionary() *dictionary {
	return &dictionary{values: []collection.ScalarValue{collection.EmptyScalar()}, ids: make(map[collection.ScalarValue]uint32)}
}

func (values *dictionary) intern(value collection.ScalarValue) (uint32, error) {
	if id, found := values.ids[value]; found {
		return id, nil
	}
	next := len(values.values)
	if next > math.MaxUint32 {
		return 0, errors.New("column has more distinct values than a cell ID can name")
	}
	id := uint32(next)
	values.values = append(values.values, value)
	values.ids[value] = id
	return id, nil
}

type row struct {
	id            string
	relativePath  string
	splitPart     int32
	splitRecorded bool
	dataOffset    int64
	dataLength    int64
	cells         []uint32
}

// codeCollection is one collection directory. Lock mutex before reading or
// changing any field except dir.
type codeCollection struct {
	mutex        sync.RWMutex
	dir          string
	header       collectionHeader
	columnIndex  map[string]int
	dictionaries []*dictionary
	rows         []row
	codes        []staticembed.Code
	byID         map[string]int
	rowsFile     *os.File
	dataFile     *os.File
	dataSize     int64
	dead         int
}

func (stored *codeCollection) setDeclaration(declaration collection.Declaration) {
	stored.header.Declaration = declaration
	stored.columnIndex = make(map[string]int, len(declaration.Scalars))
	for position, column := range declaration.Scalars {
		stored.columnIndex[column.Name] = position
		if position >= len(stored.dictionaries) {
			stored.dictionaries = append(stored.dictionaries, newDictionary())
		}
	}
}

func writeHeader(dir string, header collectionHeader) error {
	encoded, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return failed("encode collection header", err)
	}
	temporary := filepath.Join(dir, headerFileName+".tmp")
	if err := os.WriteFile(temporary, encoded, fileMode); err != nil {
		return failed("write collection header in "+dir, err)
	}
	if err := os.Rename(temporary, filepath.Join(dir, headerFileName)); err != nil {
		return failed("replace collection header in "+dir, err)
	}
	slog.Info("conversation.codestore.header_written",
		"concern", "conversation.semantic",
		"component", "conversation",
		"dir", dir,
		"columns", len(header.Declaration.Scalars),
	)
	return nil
}

// openCollection reads a collection directory. It returns
// collection.ErrCollectionMissing when the directory has no header.
func openCollection(dir string) (*codeCollection, error) {
	raw, err := os.ReadFile(filepath.Join(dir, headerFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, collection.ErrCollectionMissing
	}
	if err != nil {
		return nil, failed("read collection header in "+dir, err)
	}
	var header collectionHeader
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, failed("parse collection header in "+dir, err)
	}
	stored := &codeCollection{
		mutex:        sync.RWMutex{},
		dir:          dir,
		header:       header,
		columnIndex:  nil,
		dictionaries: nil,
		rows:         nil,
		codes:        nil,
		byID:         make(map[string]int),
		rowsFile:     nil,
		dataFile:     nil,
		dataSize:     0,
		dead:         0,
	}
	stored.setDeclaration(header.Declaration)
	if err := stored.openFiles(); err != nil {
		return nil, err
	}
	if err := stored.replay(); err != nil {
		stored.closeFiles()
		return nil, err
	}
	return stored, nil
}

func (stored *codeCollection) openFiles() error {
	rowsFile, err := os.OpenFile(filepath.Join(stored.dir, rowsFileName), os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return failed("open row log in "+stored.dir, err)
	}
	dataFile, err := os.OpenFile(filepath.Join(stored.dir, dataFileName), os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		_ = rowsFile.Close()
		return failed("open data file in "+stored.dir, err)
	}
	info, err := dataFile.Stat()
	if err != nil {
		_ = rowsFile.Close()
		_ = dataFile.Close()
		return failed("inspect data file in "+stored.dir, err)
	}
	stored.rowsFile = rowsFile
	stored.dataFile = dataFile
	stored.dataSize = info.Size()
	return nil
}

func (stored *codeCollection) closeFiles() {
	if stored.rowsFile != nil {
		_ = stored.rowsFile.Close()
		stored.rowsFile = nil
	}
	if stored.dataFile != nil {
		_ = stored.dataFile.Close()
		stored.dataFile = nil
	}
}

// replay applies every complete record of the row log. An interrupted write
// leaves a partial last record or a record that names data past the end of the
// data file; replay truncates the log before that record.
func (stored *codeCollection) replay() error {
	if _, err := stored.rowsFile.Seek(0, io.SeekStart); err != nil {
		return failed("seek row log in "+stored.dir, err)
	}
	reader := bufio.NewReaderSize(stored.rowsFile, bufferBytes)
	var offset int64
	header := make([]byte, frameHeaderBytes)
	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return stored.truncateRows(offset, err)
		}
		length := binary.LittleEndian.Uint32(header[:4])
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return stored.truncateRows(offset, err)
		}
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[4:]) {
			return stored.truncateRows(offset, errMalformedRecord)
		}
		record, err := decodeRecord(payload, stored.header.Declaration.Scalars)
		if err != nil {
			return stored.truncateRows(offset, err)
		}
		if record.kind == recordUpsert && record.dataOffset+record.dataLength > stored.dataSize {
			return stored.truncateRows(offset, errors.New("row names data past the end of the data file"))
		}
		if err := stored.apply(record); err != nil {
			return err
		}
		offset += int64(frameHeaderBytes) + int64(length)
	}
}

func (stored *codeCollection) truncateRows(offset int64, cause error) error {
	slog.Warn("conversation.codestore.row_log_truncated",
		"concern", "conversation.semantic",
		"component", "conversation",
		"dir", stored.dir,
		"offset", offset,
		"err", cause,
	)
	if err := stored.rowsFile.Truncate(offset); err != nil {
		return failed("truncate row log in "+stored.dir, err)
	}
	return nil
}

func (stored *codeCollection) apply(record rowRecord) error {
	if record.kind == recordDelete {
		stored.remove(record.id)
		stored.dead++
		return nil
	}
	cells := make([]uint32, len(stored.header.Declaration.Scalars))
	for _, cell := range record.cells {
		id, err := stored.dictionaries[cell.column].intern(cell.value)
		if err != nil {
			return failed("intern column "+stored.header.Declaration.Scalars[cell.column].Name, err)
		}
		cells[cell.column] = id
	}
	next := row{
		id:            record.id,
		relativePath:  record.relativePath,
		splitPart:     record.splitPart,
		splitRecorded: record.splitRecorded,
		dataOffset:    record.dataOffset,
		dataLength:    record.dataLength,
		cells:         cells,
	}
	if position, found := stored.byID[record.id]; found {
		stored.rows[position] = next
		stored.codes[position] = record.code
		stored.dead++
		return nil
	}
	stored.byID[record.id] = len(stored.rows)
	stored.rows = append(stored.rows, next)
	stored.codes = append(stored.codes, record.code)
	return nil
}

// remove swaps the last row into the removed position.
func (stored *codeCollection) remove(id string) {
	position, found := stored.byID[id]
	if !found {
		return
	}
	last := len(stored.rows) - 1
	if position != last {
		stored.rows[position] = stored.rows[last]
		stored.codes[position] = stored.codes[last]
		stored.byID[stored.rows[position].id] = position
	}
	stored.rows = stored.rows[:last]
	stored.codes = stored.codes[:last]
	delete(stored.byID, id)
}

// write appends data blobs and row records, syncs the data file before the
// row log, and applies the records in memory. A nil blob keeps the record's
// data offset and length.
func (stored *codeCollection) write(records []rowRecord, blobs [][]byte) error {
	dataBuffer := make([]byte, 0)
	for position := range records {
		if records[position].kind != recordUpsert || blobs[position] == nil {
			continue
		}
		records[position].dataOffset = stored.dataSize + int64(len(dataBuffer))
		records[position].dataLength = int64(len(blobs[position]))
		dataBuffer = append(dataBuffer, blobs[position]...)
	}
	rowBuffer := make([]byte, 0, len(records)*256)
	for _, record := range records {
		payload, err := encodeRecord(record, stored.header.Declaration.Scalars)
		if err != nil {
			return failed("encode row record", err)
		}
		rowBuffer, err = appendFrame(rowBuffer, payload)
		if err != nil {
			return failed("frame row record", err)
		}
	}
	if len(dataBuffer) > 0 {
		if _, err := stored.dataFile.WriteAt(dataBuffer, stored.dataSize); err != nil {
			return failed("write data file in "+stored.dir, err)
		}
		if err := stored.dataFile.Sync(); err != nil {
			return failed("sync data file in "+stored.dir, err)
		}
		stored.dataSize += int64(len(dataBuffer))
	}
	if _, err := stored.rowsFile.Seek(0, io.SeekEnd); err != nil {
		return failed("seek row log in "+stored.dir, err)
	}
	if _, err := stored.rowsFile.Write(rowBuffer); err != nil {
		return failed("write row log in "+stored.dir, err)
	}
	if err := stored.rowsFile.Sync(); err != nil {
		return failed("sync row log in "+stored.dir, err)
	}
	for _, record := range records {
		if err := stored.apply(record); err != nil {
			return err
		}
	}
	if stored.dead > compactMinimumDead && stored.dead > len(stored.rows) {
		return stored.compact()
	}
	return nil
}

// recordOf returns the upsert record of the row at position. The record keeps
// the row's data blob in place.
func (stored *codeCollection) recordOf(position int) rowRecord {
	current := stored.rows[position]
	record := emptyRecord(recordUpsert, current.id)
	record.code = stored.codes[position]
	record.relativePath = current.relativePath
	record.splitPart = current.splitPart
	record.splitRecorded = current.splitRecorded
	record.dataOffset = current.dataOffset
	record.dataLength = current.dataLength
	record.cells = make([]storedCell, 0, len(current.cells))
	for column, id := range current.cells {
		if id == 0 {
			continue
		}
		record.cells = append(record.cells, storedCell{column: column, value: stored.dictionaries[column].values[id]})
	}
	return record
}

// compact rewrites both files with only the live rows, then replaces them.
func (stored *codeCollection) compact() error {
	rowsTemporary := filepath.Join(stored.dir, rowsFileName+".compact")
	dataTemporary := filepath.Join(stored.dir, dataFileName+".compact")
	rowsOut, err := os.OpenFile(rowsTemporary, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return failed("create compacted row log in "+stored.dir, err)
	}
	dataOut, err := os.OpenFile(dataTemporary, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		_ = rowsOut.Close()
		return failed("create compacted data file in "+stored.dir, err)
	}
	offsets, err := stored.writeCompacted(rowsOut, dataOut)
	closeErr := errors.Join(rowsOut.Close(), dataOut.Close())
	if err != nil {
		return err
	}
	if closeErr != nil {
		return failed("close compacted files in "+stored.dir, closeErr)
	}
	stored.closeFiles()
	if err := os.Rename(dataTemporary, filepath.Join(stored.dir, dataFileName)); err != nil {
		return failed("replace data file in "+stored.dir, err)
	}
	if err := os.Rename(rowsTemporary, filepath.Join(stored.dir, rowsFileName)); err != nil {
		return failed("replace row log in "+stored.dir, err)
	}
	for position, offset := range offsets {
		stored.rows[position].dataOffset = offset
	}
	slog.Info("conversation.codestore.compacted",
		"concern", "conversation.semantic",
		"component", "conversation",
		"dir", stored.dir,
		"rows", len(stored.rows),
		"dead_records", stored.dead,
	)
	stored.dead = 0
	return stored.openFiles()
}

// writeCompacted writes every live row to the new files and returns each row's
// new data offset.
func (stored *codeCollection) writeCompacted(rowsOut *os.File, dataOut *os.File) ([]int64, error) {
	rowsWriter := bufio.NewWriterSize(rowsOut, bufferBytes)
	dataWriter := bufio.NewWriterSize(dataOut, bufferBytes)
	offsets := make([]int64, len(stored.rows))
	var dataOffset int64
	frame := make([]byte, 0, 512)
	for position := range stored.rows {
		record := stored.recordOf(position)
		blob := make([]byte, record.dataLength)
		if _, err := stored.dataFile.ReadAt(blob, record.dataOffset); err != nil {
			return nil, failed("read data for compaction in "+stored.dir, err)
		}
		if _, err := dataWriter.Write(blob); err != nil {
			return nil, failed("write compacted data in "+stored.dir, err)
		}
		record.dataOffset = dataOffset
		offsets[position] = dataOffset
		dataOffset += record.dataLength
		payload, err := encodeRecord(record, stored.header.Declaration.Scalars)
		if err != nil {
			return nil, failed("encode compacted row record", err)
		}
		frame, err = appendFrame(frame[:0], payload)
		if err != nil {
			return nil, failed("frame compacted row record", err)
		}
		if _, err := rowsWriter.Write(frame); err != nil {
			return nil, failed("write compacted row log in "+stored.dir, err)
		}
	}
	if err := errors.Join(dataWriter.Flush(), dataOut.Sync(), rowsWriter.Flush(), rowsOut.Sync()); err != nil {
		return nil, failed("flush compacted files in "+stored.dir, err)
	}
	return offsets, nil
}

func (stored *codeCollection) readBlob(position int) (string, string, error) {
	current := stored.rows[position]
	blob := make([]byte, current.dataLength)
	if _, err := stored.dataFile.ReadAt(blob, current.dataOffset); err != nil {
		return "", "", failed("read data of row "+current.id, err)
	}
	content, metadata, err := decodeBlob(blob)
	if err != nil {
		return "", "", failed("decode data of row "+current.id, err)
	}
	return content, metadata, nil
}

// cell returns the declared column cell of the row at position.
func (stored *codeCollection) cell(position int, column collection.ScalarColumn) collection.ScalarCell {
	index, declared := stored.columnIndex[column.Name]
	if !declared {
		return collection.AbsentCell(column.Name)
	}
	id := stored.rows[position].cells[index]
	if id == 0 {
		return collection.AbsentCell(column.Name)
	}
	value := stored.dictionaries[index].values[id]
	if value.Null {
		return collection.NullCell(column.Name)
	}
	return collection.ValueCell(column.Name, value)
}

func (stored *codeCollection) cells(position int, columns []collection.ScalarColumn) map[string]collection.ScalarCell {
	cells := make(map[string]collection.ScalarCell, len(columns))
	for _, column := range columns {
		cells[column.Name] = stored.cell(position, column)
	}
	return cells
}
