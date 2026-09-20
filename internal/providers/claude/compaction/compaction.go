// Package compaction reads Claude Code's compaction prompt: which message
// carries it, and which export arguments the operator typed after /compact.
package compaction

import (
	"strings"

	"goodkind.io/clyde/internal/conversation/exportargs"
)

// promptSignature is a distinctive substring of every Claude Code compaction
// prompt. The full prompt and the partial variants all contain it.
//
// A compaction request is structurally identical to a normal turn on the wire,
// including the full tool schema, so this first-party control string is the
// only discriminator the body offers. A changed prompt stops matching, and the
// split then leaves the request alone.
const promptSignature = "Your task is to create a detailed summary of"

// instructionsHeading precedes the text the operator typed after /compact
// (claude-code source, src/services/compact/prompt.ts:293-303).
const instructionsHeading = "Additional Instructions:"

// reminderHeading opens the trailer Claude Code appends after the operator's
// text.
const reminderHeading = "REMINDER: Do NOT call any tools."

// PromptIndex returns the index of the last user message and whether that
// message contains the compaction prompt. roles and texts are parallel: one
// entry per wire message, in order.
//
// The scan runs backward to the last user message. Interactive Claude Code
// appends a system reminder after the prompt, so the prompt is not always the
// final message. A normal turn ends with the operator's own text, which does
// not contain the signature.
func PromptIndex(roles []string, texts []string) (int, bool) {
	for index := min(len(roles), len(texts)) - 1; index >= 0; index-- {
		if roles[index] != "user" {
			continue
		}
		if strings.Contains(texts[index], promptSignature) {
			return index, true
		}
		return 0, false
	}
	return 0, false
}

// Arguments returns the export arguments the operator typed after /compact.
//
// Three kinds of content appear under the instructions heading, in order:
// declared arguments, free prose such as "focus on the test failures", and
// instructions a PreCompact hook appended. Argument reading stops at the first
// token that names no declared argument, and treats the rest as prose.
func Arguments(prompt string) []string {
	block := instructionBlock(prompt)
	if block == "" {
		return nil
	}
	declared := declaredArguments()
	fields := strings.Fields(block)
	args := make([]string, 0, len(fields))
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if !strings.HasPrefix(field, "-") {
			break
		}
		name, _, hasInlineValue := strings.Cut(field, "=")
		kind, declaredName := declared[strings.TrimLeft(name, "-")]
		if !declaredName {
			break
		}
		if hasInlineValue || kind == exportargs.KindBool {
			args = append(args, field)
			continue
		}
		if index+1 >= len(fields) {
			break
		}
		args = append(args, name, fields[index+1])
		index++
	}
	return args
}

// instructionBlock returns the text between the instructions heading and the
// reminder trailer. It returns an empty string when the prompt has no
// instructions heading, which means the operator typed nothing.
func instructionBlock(prompt string) string {
	_, after, found := strings.Cut(prompt, instructionsHeading)
	if !found {
		return ""
	}
	if before, _, cut := strings.Cut(after, reminderHeading); cut {
		after = before
	}
	return strings.TrimSpace(after)
}

// declaredArguments maps each export argument's dash-spelled name to its value
// kind. A kind decides whether the next field is that argument's value.
func declaredArguments() map[string]exportargs.Kind {
	declared := map[string]exportargs.Kind{}
	for _, declaration := range exportargs.Declarations() {
		declared[strings.ReplaceAll(declaration.Canonical, "_", "-")] = declaration.Kind
		declared[declaration.Canonical] = declaration.Kind
	}
	return declared
}
