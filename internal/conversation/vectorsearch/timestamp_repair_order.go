package vectorsearch

import (
	"strconv"
	"strings"

	"goodkind.io/lm-semantic-search/collection"
)

const familyMessagePart = 2

// storedDate records one known stored date and its message position.
type storedDate struct {
	message int
	stamp   int64
}

// knownStoredDates returns the known dates of the stored rows in families.
func knownStoredDates(families map[string][]collection.StoredRow) []storedDate {
	dates := make([]storedDate, 0)
	for key, members := range families {
		message, parsed := familyMessage(key)
		if !parsed {
			continue
		}
		for _, row := range members {
			if stamp, dated := rowStoredDate(row); dated {
				dates = append(dates, storedDate{message: message, stamp: stamp})
			}
		}
	}
	return dates
}

// matchedTextOutcome returns the outcome of a text family with one source date.
// For a date that used composer header times as a bound, it returns a conflict if
// the date falls outside the stored dates at the nearest earlier and later message
// positions. For other dates and corrections, it returns a match without comparing
// stored dates.
func (lookup sourceTimeLookup) matchedTextOutcome(key string, stamp int64) string {
	if lookup.corrections != nil || !lookup.boundedDates[stamp] {
		return SourceOutcomeMatched
	}
	message, parsed := familyMessage(key)
	if !parsed {
		return SourceOutcomeDateOrderConflict
	}
	earlier, later := -1, -1
	for _, date := range lookup.storedDates {
		if date.message < message && date.message > earlier {
			earlier = date.message
		}
		if date.message > message && (later < 0 || date.message < later) {
			later = date.message
		}
	}
	for _, date := range lookup.storedDates {
		if (date.message == earlier && date.stamp > stamp) || (date.message == later && date.stamp < stamp) {
			return SourceOutcomeDateOrderConflict
		}
	}
	return SourceOutcomeMatched
}

// familyMessage returns the message position in a family key.
func familyMessage(key string) (int, bool) {
	parts := strings.Split(key, familyPartSeparator)
	if len(parts) <= familyMessagePart {
		return 0, false
	}
	message, err := strconv.Atoi(parts[familyMessagePart])
	return message, err == nil
}

// rowStoredDate returns a stored timestamp other than NULL and the unknown time marker.
func rowStoredDate(row collection.StoredRow) (int64, bool) {
	cell, found := row.Scalars[timestampUnixColumn]
	if !found || cell.State != collection.ScalarCellValue || cell.Value.Int64 == missingTimestampUnix {
		return 0, false
	}
	return cell.Value.Int64, true
}
