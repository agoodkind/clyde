package staticembed

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// wordPiece applies the pinned model's BERT normalization and WordPiece
// tokenization settings.
type wordPiece struct {
	vocab        map[string]int32
	prefix       string
	maxWordRunes int
}

type tokenizerFile struct {
	Model struct {
		Type                    string           `json:"type"`
		ContinuingSubwordPrefix string           `json:"continuing_subword_prefix"`
		MaxInputCharsPerWord    int              `json:"max_input_chars_per_word"`
		Vocab                   map[string]int32 `json:"vocab"`
	} `json:"model"`
}

func parseWordPiece(raw []byte) (*wordPiece, error) {
	var file tokenizerFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, parseFailed("parse tokenizer.json", err)
	}
	if file.Model.Type != "WordPiece" {
		return nil, fmt.Errorf("tokenizer model type %q, want WordPiece", file.Model.Type)
	}
	return &wordPiece{
		vocab:        file.Model.Vocab,
		prefix:       file.Model.ContinuingSubwordPrefix,
		maxWordRunes: file.Model.MaxInputCharsPerWord,
	}, nil
}

// The model averages known-token vectors and omits unknown words.
func (tokenizer *wordPiece) appendIDs(ids []int32, text string) []int32 {
	normalized := normalize(text)
	word := make([]rune, 0, 32)
	flush := func() {
		if len(word) > 0 {
			ids = tokenizer.appendWord(ids, word)
			word = word[:0]
		}
	}
	for _, character := range normalized {
		switch {
		case unicode.IsSpace(character):
			flush()
		case isBertPunctuation(character):
			flush()
			ids = tokenizer.appendWord(ids, []rune{character})
		default:
			word = append(word, character)
		}
	}
	flush()
	return ids
}

func (tokenizer *wordPiece) appendWord(ids []int32, word []rune) []int32 {
	if len(word) > tokenizer.maxWordRunes {
		return ids
	}
	start := len(ids)
	var builder strings.Builder
	for begin := 0; begin < len(word); {
		matched := int32(-1)
		matchEnd := begin
		for end := len(word); end > begin; end-- {
			builder.Reset()
			if begin > 0 {
				builder.WriteString(tokenizer.prefix)
			}
			builder.WriteString(string(word[begin:end]))
			if id, found := tokenizer.vocab[builder.String()]; found {
				matched = id
				matchEnd = end
				break
			}
		}
		if matched < 0 {
			return ids[:start]
		}
		ids = append(ids, matched)
		begin = matchEnd
	}
	return ids
}

// The pinned tokenizer enables clean_text, handle_chinese_chars and lowercase.
// Its strip_accents=null setting also enables accent stripping.
func normalize(text string) string {
	var cleaned strings.Builder
	cleaned.Grow(len(text))
	for _, character := range text {
		switch {
		case character == 0 || character == utf8.RuneError || isControl(character):
			continue
		case unicode.IsSpace(character):
			cleaned.WriteByte(' ')
		case isChinese(character):
			cleaned.WriteByte(' ')
			cleaned.WriteRune(character)
			cleaned.WriteByte(' ')
		default:
			cleaned.WriteRune(character)
		}
	}
	// BertNormalizer strips accents before it lowercases.
	var stripped strings.Builder
	stripped.Grow(cleaned.Len())
	for _, character := range norm.NFD.String(cleaned.String()) {
		if unicode.Is(unicode.Mn, character) {
			continue
		}
		stripped.WriteRune(character)
	}
	return strings.ToLower(stripped.String())
}

func isControl(character rune) bool {
	if character == '\t' || character == '\n' || character == '\r' {
		return false
	}
	return unicode.In(character, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) || !unicode.In(character, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.C)
}

func isBertPunctuation(character rune) bool {
	if character < utf8.RuneSelf {
		return (character >= 33 && character <= 47) || (character >= 58 && character <= 64) ||
			(character >= 91 && character <= 96) || (character >= 123 && character <= 126)
	}
	return unicode.IsPunct(character)
}

func isChinese(character rune) bool {
	return (character >= 0x4E00 && character <= 0x9FFF) ||
		(character >= 0x3400 && character <= 0x4DBF) ||
		(character >= 0x20000 && character <= 0x2A6DF) ||
		(character >= 0x2A700 && character <= 0x2B73F) ||
		(character >= 0x2B740 && character <= 0x2B81F) ||
		(character >= 0x2B920 && character <= 0x2CEAF) ||
		(character >= 0xF900 && character <= 0xFAFF) ||
		(character >= 0x2F800 && character <= 0x2FA1F)
}
