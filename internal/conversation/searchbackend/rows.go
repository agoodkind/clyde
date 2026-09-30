package searchbackend

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// noToolIndex is the ToolIndex of a message field.
const noToolIndex = -1

// ProjectionProfile returns the projection profile identity for fields loaded
// under loadRules. Row keys include the profile. A new loading-rules tag or a
// new projection rule version produces new row keys.
func ProjectionProfile(loadRules string) string {
	return projectionProfileVersion + "|" + loadRules
}

// ProjectedFields is the field projection of one conversation.
type ProjectedFields struct {
	Fields []Field
	// WithheldOpenFields counts fields of a trailing message that the provider
	// can still extend.
	WithheldOpenFields int
}

// ProjectFields builds the selected nonempty fields of each message. A
// trailing message that the provider can still extend produces no field until
// a later message exists or Conversation.ArtifactSettled reports that the
// artifact settled.
func ProjectFields(conversation Conversation, messages []Message) ProjectedFields {
	profile := ProjectionProfile(conversation.LoadRules)
	projected := ProjectedFields{Fields: make([]Field, 0, len(messages)), WithheldOpenFields: 0}
	trailingIndex := conversation.MessageCount - 1
	withholdTrailing := conversation.TrailingMessageMayGrow && !conversation.ArtifactSettled
	for _, message := range messages {
		fields := messageFields(profile, conversation.ToolDetail, message)
		if withholdTrailing && message.Index == trailingIndex {
			projected.WithheldOpenFields += len(fields)
			continue
		}
		projected.Fields = append(projected.Fields, fields...)
	}
	return projected
}

func messageFields(profile string, toolDetail ToolDetail, message Message) []Field {
	fields := make([]Field, 0, 2+len(message.Tools))
	if text := selectedText(message.Text); text != "" {
		fields = append(fields, newField(profile, message, FieldKindChat, noToolIndex, "", text))
	}
	if thinking := selectedText(message.Thinking); thinking != "" {
		fields = append(fields, newField(profile, message, FieldKindThinking, noToolIndex, "", thinking))
	}
	for toolIndex, tool := range message.Tools {
		fields = append(fields, toolFields(profile, toolDetail, message, toolIndex, tool)...)
	}
	return fields
}

func toolFields(profile string, toolDetail ToolDetail, message Message, toolIndex int, tool Tool) []Field {
	name := strings.TrimSpace(cleanText(tool.Name))
	switch toolDetail {
	case ToolDetailNone:
		return nil
	case ToolDetailName:
		if name == "" {
			return nil
		}
		return []Field{newField(profile, message, FieldKindToolName, toolIndex, "", name)}
	case ToolDetailCall, ToolDetailOutput:
		fields := make([]Field, 0, 2)
		prefix, text := toolCallPrefixAndText(tool, name)
		if text != "" {
			fields = append(fields, newField(profile, message, FieldKindToolCall, toolIndex, prefix, text))
		}
		if toolDetail == ToolDetailOutput {
			if output := selectedText(tool.Output); output != "" {
				fields = append(fields, newField(profile, message, FieldKindToolOutput, toolIndex, toolNamePrefix(name), output))
			}
		}
		return fields
	default:
		return nil
	}
}

// toolCallPrefixAndText preserves the displayed text as the source. The
// prefix supplies tool attribution and derived tokens on every prepared part.
func toolCallPrefixAndText(tool Tool, name string) (string, string) {
	sourceText := selectedText(tool.Display)
	if sourceText == "" {
		return "", name
	}
	normalizedDisplay := strings.ReplaceAll(sourceText, "\x00", " ")
	normalizedName := strings.TrimSpace(strings.ReplaceAll(name, "\x00", " "))
	sanitized := Tool{
		Name:        normalizedName,
		Display:     normalizedDisplay,
		DisplayLang: tool.DisplayLang,
		Output:      "",
	}
	tokens := toolCallTokens(sanitized)
	var prefix strings.Builder
	prefix.WriteString(toolNamePrefix(name))
	for _, token := range tokens {
		if token == normalizedName || token == strings.TrimSpace(normalizedDisplay) {
			continue
		}
		prefix.WriteString(token)
		prefix.WriteByte('\n')
	}
	return prefix.String(), sourceText
}

func toolNamePrefix(name string) string {
	if name == "" {
		return ""
	}
	return name + "\n"
}

// selectedText cleans value with cleanText and treats text with only
// whitespace as no content.
func selectedText(value string) string {
	text := cleanText(value)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}

// cleanText removes invalid UTF-8 from selected source text. Search and
// embedding preparation normalize NUL separately from the source identity.
func cleanText(value string) string {
	return strings.ToValidUTF8(value, "")
}

func newField(profile string, message Message, kind FieldKind, toolIndex int, prefix string, text string) Field {
	return Field{
		Key:               fieldKey(profile, message.Index, kind, toolIndex),
		MessageIndex:      message.Index,
		ProviderMessageID: message.ProviderMessageID,
		Role:              message.Role,
		Timestamp:         message.Timestamp,
		Kind:              kind,
		ToolIndex:         toolIndex,
		DocumentPrefix:    prefix,
		Text:              text,
		Digest:            fieldDigest(prefix, text),
	}
}

func fieldKey(profile string, messageIndex int, kind FieldKind, toolIndex int) string {
	key := fmt.Sprintf("%s/m%d/%s", profile, messageIndex, kind)
	if toolIndex == noToolIndex {
		return key
	}
	return fmt.Sprintf("%s/t%d", key, toolIndex)
}

func fieldDigest(prefix string, text string) string {
	hasher := sha256.New()
	for _, value := range []string{prefix, text} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// FieldSelection is the comparison of projected fields with the fields a
// previous ingestion committed.
type FieldSelection struct {
	// New lists fields with a key that no committed field has.
	New []Field
	// Unchanged counts fields with a committed key and the committed digest.
	Unchanged int
	// ChangedCommitted counts fields with a committed key and a different
	// digest. The committed occurrence stays, and no row is sent for the key.
	ChangedCommitted int
}

// SelectNewFields keeps the fields with keys absent from committed, which maps
// each committed field key to its digest.
func SelectNewFields(fields []Field, committed map[string]string) FieldSelection {
	selection := FieldSelection{New: make([]Field, 0, len(fields)), Unchanged: 0, ChangedCommitted: 0}
	for _, field := range fields {
		digest, found := committed[field.Key]
		switch {
		case !found:
			selection.New = append(selection.New, field)
		case digest == field.Digest:
			selection.Unchanged++
		default:
			selection.ChangedCommitted++
		}
	}
	return selection
}
