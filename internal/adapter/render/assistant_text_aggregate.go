package render

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

const assistantTextPreviewRunes = 160

type assistantTextAggregate struct {
	deltaCount int
	chars      int
	text       strings.Builder
}

type assistantTextSummary struct {
	DeltaCount            int
	Chars                 int
	NormalizedChars       int
	NormalizedSHA256      string
	FirstPreview          string
	LastPreview           string
	FirstPreviewTruncated bool
	LastPreviewTruncated  bool
	RepeatedHalf          bool
	RepeatedSuffix        bool
	RepeatedSuffixChars   int
}

func (a *assistantTextAggregate) record(text string) {
	if text == "" {
		return
	}
	a.deltaCount++
	a.chars += len(text)
	a.text.WriteString(text)
}

func (a *assistantTextAggregate) summary() assistantTextSummary {
	normalized := normalizeAssistantTextForLog(a.text.String())
	hash := sha256.Sum256([]byte(normalized))
	repeatedHalf := hasRepeatedHalf(normalized)
	repeatedSuffix, repeatedSuffixChars := repeatedSuffixInfo(normalized)
	first, firstTruncated := firstPreview(normalized, assistantTextPreviewRunes)
	last, lastTruncated := lastPreview(normalized, assistantTextPreviewRunes)
	return assistantTextSummary{
		DeltaCount:            a.deltaCount,
		Chars:                 a.chars,
		NormalizedChars:       len(normalized),
		NormalizedSHA256:      hex.EncodeToString(hash[:]),
		FirstPreview:          first,
		LastPreview:           last,
		FirstPreviewTruncated: firstTruncated,
		LastPreviewTruncated:  lastTruncated,
		RepeatedHalf:          repeatedHalf,
		RepeatedSuffix:        repeatedSuffix,
		RepeatedSuffixChars:   repeatedSuffixChars,
	}
}

// RecordAssistantTextDeltaEmitted is part of Clyde's typed adapter surface.
func (r *EventRenderer) RecordAssistantTextDeltaEmitted(text string) {
	if r == nil {
		return
	}
	r.assistantText.record(text)
}

func normalizeAssistantTextForLog(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func firstPreview(text string, limit int) (string, bool) {
	if limit <= 0 || text == "" {
		return "", text != ""
	}
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:limit]), true
}

func lastPreview(text string, limit int) (string, bool) {
	if limit <= 0 || text == "" {
		return "", text != ""
	}
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	return string(runes[len(runes)-limit:]), true
}

func hasRepeatedHalf(text string) bool {
	fields := strings.Fields(text)
	if len(fields) < 2 || len(fields)%2 != 0 {
		return false
	}
	half := len(fields) / 2
	for i := range half {
		if fields[i] != fields[i+half] {
			return false
		}
	}
	return true
}

func repeatedSuffixInfo(text string) (bool, int) {
	fields := strings.Fields(text)
	for size := len(fields) / 2; size >= 3; size-- {
		start := len(fields) - size
		prev := start - size
		if prev < 0 {
			continue
		}
		match := true
		for i := range size {
			if fields[prev+i] != fields[start+i] {
				match = false
				break
			}
		}
		if match {
			suffix := strings.Join(fields[start:], " ")
			if len(suffix) >= 32 {
				return true, len(suffix)
			}
		}
	}
	return false, 0
}
