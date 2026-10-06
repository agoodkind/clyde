package codestore

import (
	"bufio"
	"errors"
	"log/slog"
	"os"
)

func (stored *codeCollection) compact() error {
	next := stored.header.Generation + 1
	rowsPath := stored.rowsPath(next)
	dataPath := stored.dataPath(next)
	rowsOut, err := os.OpenFile(rowsPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return failed("create compacted row log in "+stored.dir, err)
	}
	dataOut, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		_ = rowsOut.Close()
		_ = os.Remove(rowsPath)
		return failed("create compacted data file in "+stored.dir, err)
	}
	abandon := func() {
		_ = rowsOut.Close()
		_ = dataOut.Close()
		_ = os.Remove(rowsPath)
		_ = os.Remove(dataPath)
	}
	offsets, dataSize, err := stored.writeCompacted(rowsOut, dataOut)
	if err != nil {
		abandon()
		return err
	}
	// Reopening uses the header generation to select both logs.
	// Sync both new logs before replacing the header.
	header := stored.header
	header.Generation = next
	if err := replaceHeader(stored.dir, header); err != nil {
		abandon()
		return err
	}
	previous := stored.header.Generation
	previousRows := stored.rowsFile
	previousData := stored.dataFile
	stored.header = header
	stored.rowsFile = rowsOut
	stored.dataFile = dataOut
	stored.dataSize = dataSize
	for position, offset := range offsets {
		stored.rows[position].dataOffset = offset
	}
	deadRecords := stored.dead
	stored.dead = 0
	_ = previousRows.Close()
	_ = previousData.Close()
	slog.Info("conversation.codestore.compacted",
		"concern", "conversation.semantic",
		"component", "conversation",
		"dir", stored.dir,
		"rows", len(stored.rows),
		"dead_records", deadRecords,
		"generation", next,
	)
	if err := syncDirectory(stored.dir); err != nil {
		return err
	}
	if err := errors.Join(os.Remove(stored.rowsPath(previous)), os.Remove(stored.dataPath(previous))); err != nil {
		return failed("remove compacted generation in "+stored.dir, err)
	}
	return nil
}

func (stored *codeCollection) writeCompacted(rowsOut *os.File, dataOut *os.File) ([]int64, int64, error) {
	rowsWriter := bufio.NewWriterSize(rowsOut, bufferBytes)
	dataWriter := bufio.NewWriterSize(dataOut, bufferBytes)
	offsets := make([]int64, len(stored.rows))
	var dataOffset int64
	frame := make([]byte, 0, 512)
	for position := range stored.rows {
		record := stored.recordOf(position)
		blob := make([]byte, record.dataLength)
		if _, err := stored.dataFile.ReadAt(blob, record.dataOffset); err != nil {
			return nil, 0, failed("read data for compaction in "+stored.dir, err)
		}
		if _, err := dataWriter.Write(blob); err != nil {
			return nil, 0, failed("write compacted data in "+stored.dir, err)
		}
		record.dataOffset = dataOffset
		offsets[position] = dataOffset
		dataOffset += record.dataLength
		payload, err := encodeRecord(record, stored.header.Declaration.Scalars)
		if err != nil {
			return nil, 0, failed("encode compacted row record", err)
		}
		frame, err = appendFrame(frame[:0], payload)
		if err != nil {
			return nil, 0, failed("frame compacted row record", err)
		}
		if _, err := rowsWriter.Write(frame); err != nil {
			return nil, 0, failed("write compacted row log in "+stored.dir, err)
		}
	}
	if err := errors.Join(dataWriter.Flush(), dataOut.Sync(), rowsWriter.Flush(), rowsOut.Sync()); err != nil {
		return nil, 0, failed("flush compacted files in "+stored.dir, err)
	}
	return offsets, dataOffset, nil
}
