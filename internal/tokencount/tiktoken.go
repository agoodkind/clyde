package tokencount

import (
	"fmt"
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

// tiktokenCounter counts tokens with the o200k_base tokenizer. It is exact for
// current GPT models and a proxy for Claude. The codec is built once and reused;
// any codec or encode failure degrades to the heuristic instead of failing.
type tiktokenCounter struct {
	fallback heuristicCounter
}

var (
	o200kOnce  sync.Once
	o200kCodec tokenizer.Codec
	errO200k   error
)

func o200k() (tokenizer.Codec, error) {
	o200kOnce.Do(func() {
		codec, err := tokenizer.Get(tokenizer.O200kBase)
		if err != nil {
			errO200k = fmt.Errorf("build o200k tokenizer: %w", err)
			return
		}
		o200kCodec = codec
	})
	return o200kCodec, errO200k
}

// Estimate returns the exact o200k token count, or the heuristic estimate when
// the codec is unavailable or the text cannot be encoded.
func (t tiktokenCounter) Estimate(text string) int {
	codec, err := o200k()
	if err != nil {
		return t.fallback.Estimate(text)
	}
	count, err := codec.Count(text)
	if err != nil {
		return t.fallback.Estimate(text)
	}
	return count
}
