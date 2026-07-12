package tokencount

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// fixedCounter returns a constant estimate regardless of input, for testing
// wrappers that transform another counter's output.
type fixedCounter int

func (f fixedCounter) Estimate(string) int { return int(f) }

func TestHeuristicCounter(t *testing.T) {
	h := heuristicCounter{charsPerToken: 3.5}
	if got := h.Estimate(""); got != 0 {
		t.Errorf("Estimate(empty) = %d, want 0", got)
	}
	if got := h.Estimate("abcdefg"); got != 2 { // ceil(7/3.5)
		t.Errorf("Estimate(7 bytes) = %d, want 2", got)
	}
	// A non-positive ratio falls back to the default.
	zero := heuristicCounter{}
	if got := zero.Estimate("abcd"); got <= 0 {
		t.Errorf("Estimate with default ratio = %d, want > 0", got)
	}
}

func TestTiktokenCounterMatchesCodec(t *testing.T) {
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatalf("build o200k codec: %v", err)
	}
	text := "The quick brown fox jumps over the lazy dog."
	want, err := codec.Count(text)
	if err != nil {
		t.Fatalf("codec.Count: %v", err)
	}
	got := tiktokenCounter{}.Estimate(text)
	if got != want {
		t.Errorf("tiktokenCounter.Estimate = %d, want %d", got, want)
	}
	if got <= 0 {
		t.Errorf("tiktokenCounter.Estimate = %d, want > 0", got)
	}
}

func TestScaledCounterRoundsUp(t *testing.T) {
	got := scaledCounter{base: fixedCounter(7), factor: 1.3}.Estimate("x") // 9.1 -> 10
	if got != 10 {
		t.Errorf("scaledCounter.Estimate = %d, want 10", got)
	}
	// A non-positive factor is treated as identity.
	if got := (scaledCounter{base: fixedCounter(5), factor: 0}).Estimate("x"); got != 5 {
		t.Errorf("scaledCounter.Estimate with zero factor = %d, want 5", got)
	}
}

func TestFamilyFromModel(t *testing.T) {
	cases := map[string]Family{
		"claude-opus-4-8": FamilyClaude,
		"gpt-4o":          FamilyGPT,
		"gpt-5-codex":     FamilyGPT,
		"o3-mini":         FamilyGPT,
		"llama-3":         FamilyUnknown,
		"":                FamilyUnknown,
	}
	for model, want := range cases {
		if got := FamilyFromModel(model); got != want {
			t.Errorf("FamilyFromModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestLocalCounterSelection(t *testing.T) {
	text := "func main() { fmt.Println(\"hello, tokens\") }"
	settings := Settings{SafetyFactor: 1.3, CharsPerToken: 3.5}

	gpt := LocalCounter(FamilyGPT, "", settings)
	claude := LocalCounter(FamilyClaude, "", settings)
	raw := gpt.Estimate(text)
	if raw <= 0 {
		t.Fatalf("gpt raw estimate = %d, want > 0", raw)
	}
	wantClaude := int(math.Ceil(float64(raw) * 1.3))
	if got := claude.Estimate(text); got != wantClaude {
		t.Errorf("claude estimate = %d, want ceil(%d*1.3) = %d", got, raw, wantClaude)
	}
	if claude.Estimate(text) < raw {
		t.Errorf("claude estimate %d should be >= raw %d", claude.Estimate(text), raw)
	}

	// Unknown family infers from the model.
	inferred := LocalCounter(FamilyUnknown, "claude-3", settings)
	if inferred.Estimate(text) != claude.Estimate(text) {
		t.Errorf("unknown+claude model should match Claude counter")
	}
	// Unknown family with an unknown model uses the heuristic.
	heuristicWant := heuristicCounter{charsPerToken: 3.5}.Estimate(text)
	if got := LocalCounter(FamilyUnknown, "llama", settings).Estimate(text); got != heuristicWant {
		t.Errorf("unknown+unknown model estimate = %d, want heuristic %d", got, heuristicWant)
	}
}

func TestCapToLastTokens(t *testing.T) {
	// charsPerToken 1 makes each byte one token, so per-line estimates are
	// predictable: each "lineN\n" is 6 tokens.
	c := heuristicCounter{charsPerToken: 1}
	body := "line1\nline2\nline3\n"

	// Budget zero leaves the body unchanged.
	if got, _, trunc := CapToLastTokens(body, 0, c); got != body || trunc {
		t.Errorf("budget 0: got %q trunc %v, want unchanged", got, trunc)
	}
	// A budget larger than the whole body leaves it unchanged.
	if got, _, trunc := CapToLastTokens(body, 1000, c); got != body || trunc {
		t.Errorf("large budget: got %q trunc %v, want unchanged", got, trunc)
	}
	// A tight budget keeps only the tail line.
	got, tokens, trunc := CapToLastTokens(body, 6, c)
	if got != "line3\n" || !trunc {
		t.Errorf("budget 6: got %q trunc %v, want %q true", got, trunc, "line3\n")
	}
	if tokens > 6 {
		t.Errorf("budget 6: kept tokens %d exceeds budget", tokens)
	}
	// A budget for two lines keeps the last two.
	if got, _, _ := CapToLastTokens(body, 12, c); got != "line2\nline3\n" {
		t.Errorf("budget 12: got %q, want %q", got, "line2\nline3\n")
	}
	// When even the last line exceeds budget, the result is empty.
	if got, _, trunc := CapToLastTokens(body, 5, c); got != "" || !trunc {
		t.Errorf("budget 5: got %q trunc %v, want empty true", got, trunc)
	}
	// The kept result never exceeds budget and stays valid UTF-8.
	utf8Body := "héllo\nwörld\ntlast\n"
	capped, _, _ := CapToLastTokens(utf8Body, 8, c)
	if !utf8.ValidString(capped) {
		t.Errorf("capped result is not valid UTF-8: %q", capped)
	}
	if !strings.HasSuffix(utf8Body, capped) && capped != "" {
		t.Errorf("capped %q should be a suffix of the body", capped)
	}
	if c.Estimate(capped) > 8 {
		t.Errorf("capped estimate %d exceeds budget 8", c.Estimate(capped))
	}
}
