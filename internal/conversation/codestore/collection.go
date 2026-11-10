package codestore

import (
	"bufio"
	"context"
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
	"strings"
	"sync"

	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	headerFileName = "collection.json"
	rowsFilePrefix = "rows"
	dataFilePrefix = "data"
	logFileSuffix  = ".log"
	directoryMode  = 0o700
	fileMode       = 0o600

	compactMinimumDead = 10000
	bufferBytes        = 1 << 20
)

// errGenerationWithoutHeader indicates that a generation file exists without a header.
// Its message instructs the reader to restore the collection header from a backup.
var errGenerationWithoutHeader = errors.New(
	"a generation file exists but the collection header is missing; restore the header from a backup",
)

// errDataFileTooShort is the error for a valid row record that references bytes
// beyond the end of the data file. Recovery requires restoring the data file from
// a backup.
var errDataFileTooShort = errors.New(
	"a row references bytes beyond the end of the data file; restore the data file from a backup",
)

func failed(operation string, err error) error {
	slog.Warn(
		"conversation.codestore.operation_failed",
		"concern",
		"conversation.semantic",
		"component",
		"conversation",
		"operation",
		operation,
		"err",
		err,
	)

	return fmt.Errorf("%s: %w", operation, err)
}

type collectionHeader struct {
	Declaration collection.Declaration `json:"declaration"`
	Dimension   int                    `json:"dimension"`
	Model       string                 `json:"model"`
	Generation  int64                  `json:"generation,omitempty"`
	FrameFormat int                    `json:"frame_format,omitempty"`
}

func generationFileName(prefix string, generation int64) string {
	if generation == 0 {
		return prefix + logFileSuffix
	}

	return fmt.Sprintf("%s.%d%s", prefix, generation, logFileSuffix)
}

func (stored *codeCollection) rowsPath(generation int64) string {
	return filepath.Join(stored.dir, generationFileName(rowsFilePrefix, generation))
}

func (stored *codeCollection) dataPath(generation int64) string {
	return filepath.Join(stored.dir, generationFileName(dataFilePrefix, generation))
}

// Dictionary ID 0 represents an absent column value.
type dictionary struct {
	values []collection.ScalarValue
	ids    map[collection.ScalarValue]uint32
}

func newDictionary() *dictionary {
	return &dictionary{
		values: []collection.ScalarValue{collection.EmptyScalar()},
		ids:    make(map[collection.ScalarValue]uint32),
	}
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

// Lock mutex before reading or changing collection state. The directory path
// is immutable after construction.
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
	// logSize stores the row log size that the state in memory includes.
	logSize int64
	// headerInfo stores the header file identity that the state in memory includes.
	headerInfo os.FileInfo
	dead       int
	// closed records whether Store.Close has closed this collection handle.
	// The collection mutex protects this field. A closed handle cannot reopen its files.
	closed bool
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
	if err := replaceHeader(dir, header); err != nil {
		return err
	}

	return syncDirectory(dir)
}

// initializeGenerationFiles does not change existing bytes.
func initializeGenerationFiles(dir string, generation int64) error {
	for _, prefix := range []string{rowsFilePrefix, dataFilePrefix} {
		path := filepath.Join(dir, generationFileName(prefix, generation))
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode)
		if err != nil {
			return failed("create a generation file in "+dir, err)
		}

		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return failed("sync the generation file in "+dir, err)
		}
	}

	return nil
}

func replaceHeader(dir string, header collectionHeader) error {
	encoded, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return failed("encode collection header", err)
	}

	temporary := filepath.Join(dir, headerFileName+".tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return failed("create collection header in "+dir, err)
	}

	_, writeErr := file.Write(encoded)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		_ = os.Remove(temporary)
		return failed("write collection header in "+dir, err)
	}

	if err := os.Rename(temporary, filepath.Join(dir, headerFileName)); err != nil {
		_ = os.Remove(temporary)
		return failed("replace collection header in "+dir, err)
	}

	slog.Info(
		"conversation.codestore.header_written",
		"concern",
		"conversation.semantic",
		"component",
		"conversation",
		"dir",
		dir,
		"columns",
		len(header.Declaration.Scalars),
		"generation",
		header.Generation,
	)

	return nil
}

func syncDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return failed("open directory "+dir, err)
	}

	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return failed("sync directory "+dir, err)
	}

	return nil
}

// readHeader reads and validates the collection header and returns its file identity.
func readHeader(dir string) (collectionHeader, os.FileInfo, error) {
	var header collectionHeader
	file, err := os.Open(filepath.Join(dir, headerFileName))
	if errors.Is(err, os.ErrNotExist) {
		if storedErr := requireNoStoredGenerations(dir); storedErr != nil {
			return header, nil, storedErr
		}

		return header, nil, collection.ErrCollectionMissing
	}

	if err != nil {
		return header, nil, failed("read the collection header in "+dir, err)
	}

	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return header, nil, failed("inspect the collection header in "+dir, err)
	}

	raw, err := io.ReadAll(file)
	if err != nil {
		return header, nil, failed("read the collection header in "+dir, err)
	}

	if err := json.Unmarshal(raw, &header); err != nil {
		return header, nil, failed("parse the collection header in "+dir, err)
	}

	if header.FrameFormat != frameFormatPayloadCRC && header.FrameFormat != frameFormatLengthCRC {
		return header, nil, failed(
			"parse the collection header in "+dir,
			fmt.Errorf("the code store does not support frame format %d", header.FrameFormat),
		)
	}

	return header, info, nil
}

// requireNoStoredGenerations returns an error for nonempty or later generation files
// without a header. It does not return an error for empty generation 0 row and data
// files because collection creation writes them before the header.
func requireNoStoredGenerations(dir string) error {
	emptyInitialFiles := map[string]bool{
		generationFileName(rowsFilePrefix, 0): true,
		generationFileName(dataFilePrefix, 0): true,
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return failed("the store could not list generation files in "+dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		isGenerationFile := strings.HasPrefix(name, rowsFilePrefix) ||
			strings.HasPrefix(name, dataFilePrefix)
		if !isGenerationFile || !strings.HasSuffix(name, logFileSuffix) {
			continue
		}

		path := filepath.Join(dir, name)
		info, err := entry.Info()
		if err != nil {
			return failed("inspect "+path, err)
		}

		if !emptyInitialFiles[name] || info.Size() > 0 {
			return failed(
				"the store found a generation file without a header in "+dir,
				fmt.Errorf("%w: %s", errGenerationWithoutHeader, path),
			)
		}
	}

	return nil
}

// openCollection takes the collection's kernel lock before reading its header
// and rebuilding the collection state from the files.
func openCollection(ctx context.Context, dir string) (*codeCollection, error) {
	lock, err := lockCollectionDirectory(ctx, dir)
	if err != nil {
		return nil, err
	}

	defer func() { _ = lock.Close() }()

	return loadCollection(dir)
}

func loadCollection(dir string) (*codeCollection, error) {
	header, headerInfo, err := readHeader(dir)
	if err != nil {
		return nil, err
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
		logSize:      0,
		headerInfo:   headerInfo,
		dead:         0,
		closed:       false,
	}
	stored.setDeclaration(header.Declaration)
	if err := stored.openFiles(); err != nil {
		return nil, err
	}

	if err := stored.replay(); err != nil {
		stored.closeFiles()
		return nil, err
	}

	stored.removeOtherGenerations()

	return stored, nil
}

func (stored *codeCollection) removeOtherGenerations() {
	current := map[string]bool{
		stored.rowsPath(stored.header.Generation): true,
		stored.dataPath(stored.header.Generation): true,
	}

	for _, prefix := range []string{rowsFilePrefix, dataFilePrefix} {
		matches, err := filepath.Glob(filepath.Join(stored.dir, prefix+"*"+logFileSuffix))
		if err != nil {
			continue
		}

		for _, path := range matches {
			if current[path] {
				continue
			}

			if err := os.Remove(path); err != nil {
				slog.Warn(
					"conversation.codestore.remove_generation_failed",
					"concern",
					"conversation.semantic",
					"component",
					"conversation",
					"path",
					path,
					"err",
					err,
				)
			}
		}
	}
}

// openFiles opens the existing row and data files for the header's generation.
// If either file is missing, it returns an error. loadCollection then returns
// without removing older generations.
func (stored *codeCollection) openFiles() error {
	rowsFile, err := stored.openGenerationFile(stored.rowsPath(stored.header.Generation))
	if err != nil {
		return err
	}

	dataFile, err := stored.openGenerationFile(stored.dataPath(stored.header.Generation))
	if err != nil {
		_ = rowsFile.Close()
		return err
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

// openGenerationFile opens one existing generation file. If the file does not
// exist, it returns an error with the generation, file path, and instruction to
// restore the file from a backup. The store does not create a missing file because
// a header without generation files does not prove that the collection was empty.
func (stored *codeCollection) openGenerationFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, fileMode)
	if errors.Is(err, os.ErrNotExist) {
		return nil, failed(
			fmt.Sprintf(
				"The collection header selects generation %d, but %s does not exist. Restore the file from a backup.",
				stored.header.Generation,
				path,
			),
			err,
		)
	}

	if err != nil {
		return nil, failed("open the generation file "+path, err)
	}

	return file, nil
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

func (stored *codeCollection) replay() error {
	info, err := stored.rowsFile.Stat()
	if err != nil {
		return failed("inspect row log in "+stored.dir, err)
	}

	logSize := info.Size()
	offset := stored.logSize
	if _, err := stored.rowsFile.Seek(offset, io.SeekStart); err != nil {
		return failed("seek row log in "+stored.dir, err)
	}

	reader := bufio.NewReaderSize(stored.rowsFile, bufferBytes)
	headerBytes := frameHeaderSize(stored.header.FrameFormat)
	header := make([]byte, headerBytes)

	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return stored.truncateRows(offset, err)
		}

		length, payloadChecksum, lengthValid := stored.parseFrameHeader(header)
		// A corrupt length can make the end-of-log check truncate later valid records.
		if !lengthValid {
			return stored.rejectRecord(
				offset,
				offset+1,
				logSize,
				"check row log length",
				errMalformedRecord,
			)
		}

		end := offset + int64(headerBytes) + length
		if end > logSize {
			return stored.rejectRecord(
				offset,
				stored.searchStart(offset, end),
				logSize,
				"check row log end",
				io.ErrUnexpectedEOF,
			)
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return failed(fmt.Sprintf("read row log in %s at offset %d", stored.dir, offset), err)
		}

		record, stop, err := stored.checkRecord(offset, end, logSize, payload, payloadChecksum)
		if stop {
			return err
		}

		if err := stored.apply(record); err != nil {
			return err
		}

		offset = end
		stored.logSize = end
	}
}

func (stored *codeCollection) parseFrameHeader(header []byte) (int64, uint32, bool) {
	lengthBytes := header[:frameFieldBytes]
	length := int64(binary.LittleEndian.Uint32(lengthBytes))
	payloadChecksum := binary.LittleEndian.Uint32(header[len(header)-frameFieldBytes:])
	if stored.header.FrameFormat != frameFormatLengthCRC {
		return length, payloadChecksum, true
	}

	lengthChecksum := binary.LittleEndian.Uint32(header[frameFieldBytes : 2*frameFieldBytes])

	return length, payloadChecksum, crc32.ChecksumIEEE(lengthBytes) == lengthChecksum
}

func (stored *codeCollection) checkRecord(
	offset int64,
	end int64,
	logSize int64,
	payload []byte,
	payloadChecksum uint32,
) (rowRecord, bool, error) {
	if len(payload) == 0 || crc32.ChecksumIEEE(payload) != payloadChecksum {
		return emptyRecord(0, ""), true, stored.rejectRecord(
			offset,
			stored.searchStart(offset, end),
			logSize,
			"check row log",
			errMalformedRecord,
		)
	}

	record, err := decodeRecord(payload, stored.header.Declaration.Scalars)
	if err != nil {
		// Replay returns decode failures without truncating the row log. A checksum match does not prove that the payload is decodable.
		return record, true, failed(
			fmt.Sprintf("decode row log in %s at offset %d", stored.dir, offset),
			err,
		)
	}

	// checkRecord relies on write syncing data before appending each row record.
	// A valid record that references bytes beyond the end of the data file indicates
	// missing data bytes. checkRecord returns an error without truncating the row log.
	if record.kind == recordUpsert && record.dataOffset+record.dataLength > stored.dataSize {
		return record, true, failed(
			fmt.Sprintf(
				"The store could not validate the row log in %s at offset %d",
				stored.dir,
				offset,
			),
			errDataFileTooShort,
		)
	}

	return record, false, nil
}

func (stored *codeCollection) searchStart(offset int64, end int64) int64 {
	// Format 1 can trust the declared end because the length checksum passed.
	// Format 0 cannot trust the declared end because its length has no checksum.
	if stored.header.FrameFormat == frameFormatLengthCRC {
		return end
	}

	return offset + 1
}

func (stored *codeCollection) rejectRecord(
	offset int64,
	searchStart int64,
	logSize int64,
	operation string,
	cause error,
) error {
	contentEnd, err := stored.contentEnd(offset, logSize)
	if err != nil {
		return err
	}

	frameFound, err := stored.validFrameBetween(searchStart, contentEnd, logSize)
	if err != nil {
		return err
	}

	// A torn append can damage only bytes after the last complete record.
	// A later complete valid frame proves corruption in the middle of the log.
	if !frameFound {
		return stored.truncateRows(offset, cause)
	}

	return failed(fmt.Sprintf("%s in %s at offset %d", operation, stored.dir, offset), cause)
}

func (stored *codeCollection) contentEnd(offset int64, logSize int64) (int64, error) {
	chunk := make([]byte, min(int64(bufferBytes), logSize-offset))
	for chunkEnd := logSize; chunkEnd > offset; {
		chunkStart := max(offset, chunkEnd-int64(len(chunk)))
		count := chunkEnd - chunkStart
		if _, err := stored.rowsFile.ReadAt(chunk[:count], chunkStart); err != nil {
			return 0, failed(
				fmt.Sprintf("read row log tail in %s at offset %d", stored.dir, chunkStart),
				err,
			)
		}

		for position := count - 1; position >= 0; position-- {
			if chunk[position] != 0 {
				return chunkStart + position + 1, nil
			}
		}

		chunkEnd = chunkStart
	}

	return offset, nil
}

func (stored *codeCollection) validFrameBetween(
	searchStart int64,
	contentEnd int64,
	logSize int64,
) (bool, error) {
	headerBytes := int64(frameHeaderSize(stored.header.FrameFormat))

	window := make([]byte, int64(bufferBytes)+headerBytes)
	for windowStart := searchStart; windowStart < contentEnd; windowStart += int64(bufferBytes) {
		windowEnd := min(windowStart+int64(len(window)), logSize)
		if _, err := stored.rowsFile.ReadAt(
			window[:windowEnd-windowStart],
			windowStart,
		); err != nil {
			return false, failed(
				fmt.Sprintf("read row log tail in %s at offset %d", stored.dir, windowStart),
				err,
			)
		}

		lastStart := min(windowStart+int64(bufferBytes), contentEnd)
		for frameStart := windowStart; frameStart < lastStart; frameStart++ {
			payloadStart := frameStart + headerBytes
			if payloadStart > logSize {
				return false, nil
			}

			length, payloadChecksum, lengthValid := stored.parseFrameHeader(
				window[frameStart-windowStart : payloadStart-windowStart],
			)
			if !lengthValid || length == 0 || payloadStart+length > logSize {
				continue
			}

			checksumMatches, err := stored.payloadMatches(payloadStart, length, payloadChecksum)
			if err != nil {
				return false, err
			}

			if checksumMatches {
				return true, nil
			}
		}
	}

	return false, nil
}

func (stored *codeCollection) payloadMatches(
	payloadStart int64,
	length int64,
	payloadChecksum uint32,
) (bool, error) {
	checksum := crc32.NewIEEE()
	if _, err := io.Copy(
		checksum,
		io.NewSectionReader(stored.rowsFile, payloadStart, length),
	); err != nil {
		return false, failed(
			fmt.Sprintf("read row log payload in %s at offset %d", stored.dir, payloadStart),
			err,
		)
	}

	return checksum.Sum32() == payloadChecksum, nil
}

func (stored *codeCollection) truncateRows(offset int64, cause error) error {
	slog.Warn(
		"conversation.codestore.row_log_truncated",
		"concern",
		"conversation.semantic",
		"component",
		"conversation",
		"dir",
		stored.dir,
		"offset",
		offset,
		"err",
		cause,
	)
	if err := stored.rowsFile.Truncate(offset); err != nil {
		return failed("truncate row log in "+stored.dir, err)
	}

	stored.logSize = offset

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

// write syncs new data before appending its offsets to the row log.
// Nil blobs reuse the existing data offsets and lengths.
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

		rowBuffer, err = appendFrame(rowBuffer, payload, stored.header.FrameFormat)
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

	logEnd, err := stored.rowsFile.Seek(0, io.SeekEnd)
	if err != nil {
		return failed("seek row log in "+stored.dir, err)
	}

	_, writeErr := stored.rowsFile.Write(rowBuffer)
	if writeErr == nil {
		writeErr = stored.rowsFile.Sync()
	}

	if writeErr != nil {
		truncateErr := stored.rowsFile.Truncate(logEnd)
		return failed("append row log in "+stored.dir, errors.Join(writeErr, truncateErr))
	}

	stored.logSize = logEnd + int64(len(rowBuffer))

	for _, record := range records {
		if err := stored.apply(record); err != nil {
			return err
		}
	}

	if stored.dead > compactMinimumDead && stored.dead > len(stored.rows) {
		// write has already synced and applied the records.
		_ = stored.compact()
	}

	return nil
}

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

		record.cells = append(
			record.cells,
			storedCell{column: column, value: stored.dictionaries[column].values[id]},
		)
	}

	return record
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

func (stored *codeCollection) cell(
	position int,
	column collection.ScalarColumn,
) collection.ScalarCell {
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

func (stored *codeCollection) cells(
	position int,
	columns []collection.ScalarColumn,
) map[string]collection.ScalarCell {
	cells := make(map[string]collection.ScalarCell, len(columns))
	for _, column := range columns {
		cells[column.Name] = stored.cell(position, column)
	}

	return cells
}
