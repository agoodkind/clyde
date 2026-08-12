package updateopts

import "strings"

func isLocalBuild(version string, dirty bool) bool {
	if dirty {
		return true
	}
	trimmed := strings.TrimSpace(version)
	if trimmed == "" || trimmed == "dev" || trimmed == "unknown" {
		return true
	}
	return hasGitDescribeSuffix(trimmed)
}

func hasGitDescribeSuffix(version string) bool {
	lastDash := strings.LastIndex(version, "-")
	if lastDash < 0 {
		return false
	}
	objectName := version[lastDash+1:]
	if len(objectName) < 2 || objectName[0] != 'g' {
		return false
	}
	if !isHex(objectName[1:]) {
		return false
	}
	countField := version[:lastDash]
	countDash := strings.LastIndex(countField, "-")
	if countDash < 0 {
		return false
	}
	return isDigits(countField[countDash+1:])
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func isHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		isDigit := character >= '0' && character <= '9'
		isLower := character >= 'a' && character <= 'f'
		isUpper := character >= 'A' && character <= 'F'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}
