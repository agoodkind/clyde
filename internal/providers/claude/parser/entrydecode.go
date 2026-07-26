package parser

// FieldDecode says whether one field of a transcript record decoded
// completely.
//
// A field this parser decodes itself records its outcome here instead of
// returning an error. encoding/json aborts the whole surrounding object when a
// nested unmarshaler returns an error, so an error costs every key written
// after that field, and Claude writes the session and workspace keys after the
// union-typed ones.
type FieldDecode string

const (
	// FieldDecodeComplete marks a field whose whole JSON value matched the model.
	FieldDecodeComplete FieldDecode = ""
	// FieldDecodePartial marks a field that held a JSON type the model does not
	// accept. Whatever decoded before the mismatch is kept, and the field's Raw
	// member still holds the original value.
	FieldDecodePartial FieldDecode = "partial"
)

// EntryDecodeOutcome says how completely one transcript line decoded.
type EntryDecodeOutcome string

const (
	// EntryDecodeComplete marks a record where every modeled field decoded.
	EntryDecodeComplete EntryDecodeOutcome = ""
	// EntryDecodePartial marks a record where at least one field held a JSON
	// type the model does not accept. Every other key of the line still
	// decoded, so the record stays usable.
	EntryDecodePartial EntryDecodeOutcome = "partial"
)

// EntryDecode reports how completely [DecodeTranscriptEntry] read one line. A
// caller that needs a whole record checks this rather than only the returned
// error, because a field whose JSON type does not match the model is tolerated
// rather than failed.
type EntryDecode struct {
	Outcome EntryDecodeOutcome
	// Fields names the JSON keys whose value did not match the model, as dotted
	// paths for members of a field this parser decodes itself.
	Fields []string
}

// emptyEntryDecode returns the zero decode report, written out so exhaustruct
// sees every field set.
func emptyEntryDecode() EntryDecode {
	return EntryDecode{Outcome: EntryDecodeComplete, Fields: nil}
}

// Complete reports whether every modeled field of the record decoded.
func (decode EntryDecode) Complete() bool {
	return decode.Outcome == EntryDecodeComplete
}

// partialFields names the fields this parser decodes itself whose value did
// not fully match the model. Each of those types records its own outcome, and
// the record is the only place that knows which JSON key each one came from,
// so the walk is spelled out here rather than derived.
func (entry TranscriptEntry) partialFields() []string {
	var fields []string
	if entry.Timestamp.Decode == FieldDecodePartial {
		fields = append(fields, "timestamp")
	}
	if entry.ToolUseResult.Decode == FieldDecodePartial {
		fields = append(fields, "toolUseResult")
	}
	if entry.ToolUseResult.Detail != nil && entry.ToolUseResult.Detail.Content.Decode == FieldDecodePartial {
		fields = append(fields, "toolUseResult.content")
	}
	if entry.Error.Decode == FieldDecodePartial {
		fields = append(fields, "error")
	}
	if entry.Attachment != nil {
		if entry.Attachment.Decode == FieldDecodePartial {
			fields = append(fields, "attachment")
		}
		if entry.Attachment.Timestamp.Decode == FieldDecodePartial {
			fields = append(fields, "attachment.timestamp")
		}
	}
	if entry.Snapshot != nil {
		if entry.Snapshot.Timestamp.Decode == FieldDecodePartial {
			fields = append(fields, "snapshot.timestamp")
		}
		if snapshotBackupTimePartial(entry.Snapshot.TrackedFileBackups) {
			fields = append(fields, "snapshot.trackedFileBackups")
		}
	}
	if entry.Backup != nil && entry.Backup.BackupTime.Decode == FieldDecodePartial {
		fields = append(fields, "backup.backupTime")
	}
	return fields
}

// snapshotBackupTimePartial reports whether any tracked-file backup carries an
// unparsable backup time. The map is reported as one field because its key set
// is a file path per record and the iteration order is not stable.
func snapshotBackupTimePartial(backups map[string]FileBackup) bool {
	for _, backup := range backups {
		if backup.BackupTime.Decode == FieldDecodePartial {
			return true
		}
	}
	return false
}
