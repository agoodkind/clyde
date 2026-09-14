package tokencount

import "fmt"

// Estimate is a local tokenizer count of a body. Tokenizer names the estimator
// that produced Tokens, so a Claude export can report "o200k x 1.3" while a
// GPT export reports "o200k".
type Estimate struct {
	Tokenizer string
	Tokens    int
}

// Spec selects the local tokenizer used to count an export body.
type Spec struct {
	Family   Family
	Model    string
	Settings Settings
}

// Count estimates text with this spec's local tokenizer.
func (s Spec) Count(text string) Estimate {
	name, counter := describe(s.Family, s.Model, s.Settings)
	return Estimate{Tokenizer: name, Tokens: counter.Estimate(text)}
}

func describe(family Family, model string, settings Settings) (string, Counter) {
	switch family {
	case FamilyClaude:
		return fmt.Sprintf("o200k x %.1f", settings.safetyFactor()), LocalCounter(family, model, settings)
	case FamilyGPT:
		return "o200k", LocalCounter(family, model, settings)
	case FamilyUnknown:
		inferred := FamilyFromModel(model)
		if inferred != FamilyUnknown {
			return describe(inferred, model, settings)
		}
		return heuristicName(settings), LocalCounter(family, model, settings)
	default:
		return heuristicName(settings), LocalCounter(family, model, settings)
	}
}

func heuristicName(settings Settings) string {
	return fmt.Sprintf("heuristic (%.1f chars/token)", settings.charsPerToken())
}
