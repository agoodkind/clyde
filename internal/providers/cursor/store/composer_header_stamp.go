package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"strconv"
	"strings"
)

type composerHeaderStamp [sha256.Size]byte

type composerHeaderInventory struct {
	headers map[string]ComposerHeader
	stamps  map[string]composerHeaderStamp
}

func readComposerHeaderInventory(
	ctx context.Context,
	db *sql.DB,
	prior GlobalDiscovery,
	signature *globalDiscoverySignature,
) (composerHeaderInventory, error) {
	inventory := composerHeaderInventory{
		headers: make(map[string]ComposerHeader, len(prior.Headers)),
		stamps:  make(map[string]composerHeaderStamp, len(prior.headerStamps)),
	}
	digest := sha256.New()
	failedRows := 0
	var firstDecodeErr error
	err := forEachKVRowInKeyRange(ctx, db, KVTableCursorDiskKV, keyRangeForPrefix(composerDataKeyPrefix), "", func(row KVRow) error {
		composerID := strings.TrimPrefix(row.Key, composerDataKeyPrefix)
		stamp := composerHeaderStamp(sha256.Sum256(row.Value))
		writeFingerprintField(digest, strconv.FormatInt(row.RowID, 10))
		writeFingerprintField(digest, row.Key)
		writeFingerprintField(digest, strconv.Itoa(len(row.Value)))
		_, _ = digest.Write(stamp[:])
		inventory.stamps[composerID] = stamp
		previousHeader, hasPreviousHeader := prior.Headers[composerID]
		previousStamp, hasPreviousStamp := prior.headerStamps[composerID]
		if hasPreviousStamp && previousStamp == stamp {
			if hasPreviousHeader {
				inventory.headers[composerID] = previousHeader
			}
			return nil
		}
		header, decodeErr := DecodeComposerHeaderJSON(row.Value)
		if decodeErr != nil {
			if failedRows == 0 {
				firstDecodeErr = decodeErr
			}
			failedRows++
			if hasPreviousHeader {
				inventory.headers[composerID] = previousHeader
			}
			return nil
		}
		header.ComposerID = composerID
		inventory.headers[composerID] = header
		return nil
	})
	if err != nil {
		return composerHeaderInventory{headers: nil, stamps: nil}, err
	}
	copy(signature.composerDigest[:], digest.Sum(nil))
	if failedRows > 0 {
		discoveryReadLogger(ctx).WarnContext(ctx,
			"providers.cursor.store.composer_header_decode_failed",
			"concern", concern,
			"failed_rows", failedRows,
			"err", firstDecodeErr,
		)
	}
	return inventory, nil
}
