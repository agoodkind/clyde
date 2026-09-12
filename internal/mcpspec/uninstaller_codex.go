package mcpspec

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type codexMCPKey string

const (
	codexMCPKeyCommand codexMCPKey = "command"
	codexMCPKeyArgs    codexMCPKey = "args"
)

func removeManagedClydeCodexTable(input string, homeDir string) string {
	lines := strings.Split(input, "\n")
	output := make([]string, 0, len(lines))
	for index := 0; index < len(lines); {
		if !isClydeCodexHeader(lines[index]) {
			output = append(output, lines[index])
			index++
			continue
		}
		parentEnd := index + 1
		for parentEnd < len(lines) && !isTOMLTableHeader(lines[parentEnd]) {
			parentEnd++
		}
		end := parentEnd
		for end < len(lines) && isNestedClydeTable(lines[index], lines[end]) {
			nestedEnd := end + 1
			for nestedEnd < len(lines) && !isTOMLTableHeader(lines[nestedEnd]) {
				nestedEnd++
			}
			end = nestedEnd
		}
		if !isManagedMCPTable(lines[index:parentEnd], homeDir) {
			output = append(output, lines[index:end]...)
		}
		index = end
	}
	return strings.Join(output, "\n")
}

func isNestedClydeTable(parent string, candidate string) bool {
	parentHeader := strings.TrimSpace(parent)
	candidateHeader := strings.TrimSpace(candidate)
	if parentHeader == `[mcp_servers."clyde"]` {
		return strings.HasPrefix(candidateHeader, `[mcp_servers."clyde".`)
	}
	if parentHeader == "[mcp_servers.clyde]" {
		return strings.HasPrefix(candidateHeader, "[mcp_servers.clyde.")
	}
	return false
}

func isManagedMCPTable(lines []string, homeDir string) bool {
	server := mcpServer{Command: "", Args: nil}
	for _, line := range lines {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch codexMCPKey(strings.TrimSpace(key)) {
		case codexMCPKeyCommand:
			decoded, err := strconv.Unquote(strings.TrimSpace(value))
			if err == nil {
				server.Command = decoded
			}
		case codexMCPKeyArgs:
			if strings.Join(strings.Fields(value), "") == `["mcp","serve"]` {
				server.Args = []string{"mcp", "serve"}
			}
		default:
		}
	}
	return isManagedMCPServer(server, homeDir)
}

func isManagedMCPServer(server mcpServer, homeDir string) bool {
	command := filepath.Clean(server.Command)
	knownCommands := []string{
		filepath.Join(homeDir, ".local", "bin", "clyde"),
		"/usr/local/bin/clyde",
	}
	if executable, err := os.Executable(); err == nil {
		knownCommands = append(knownCommands, filepath.Clean(executable))
	}
	for _, knownCommand := range knownCommands {
		if command == filepath.Clean(knownCommand) {
			return len(server.Args) == 2 && server.Args[0] == "mcp" && server.Args[1] == "serve"
		}
	}
	return false
}
