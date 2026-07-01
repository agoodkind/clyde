package hookspec

import "strings"

func shellCommand(clydeBin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(clydeBin))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if isShellBareWord(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func isShellBareWord(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' {
			continue
		}
		if char >= 'A' && char <= 'Z' {
			continue
		}
		if char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '/', '.', '_', '-', ':', '+', '=':
			continue
		default:
			return false
		}
	}
	return true
}

func shellWords(value string) ([]string, bool) {
	words := make([]string, 0)
	var current strings.Builder
	inSingleQuote := false

	for index := 0; index < len(value); index++ {
		char := value[index]
		if inSingleQuote {
			if char == '\'' {
				inSingleQuote = false
				continue
			}
			current.WriteByte(char)
			continue
		}
		switch char {
		case ' ', '\t', '\n':
			if current.Len() == 0 {
				continue
			}
			words = append(words, current.String())
			current.Reset()
		case '\\':
			if index+2 < len(value) && value[index+1] == '\'' && value[index+2] == '\'' {
				current.WriteByte('\'')
				inSingleQuote = true
				index += 2
				continue
			}
			current.WriteByte(char)
		case '\'':
			inSingleQuote = true
		default:
			current.WriteByte(char)
		}
	}
	if inSingleQuote {
		return nil, false
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words, true
}

func commandHasManagedArgs(command string, signatures [][]string) bool {
	words, ok := shellWords(command)
	if !ok || len(words) <= 1 {
		return false
	}
	commandArgs := words[1:]
	for _, signature := range signatures {
		if len(signature) != len(commandArgs) {
			continue
		}
		matches := true
		for index := range signature {
			if signature[index] != commandArgs[index] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}
