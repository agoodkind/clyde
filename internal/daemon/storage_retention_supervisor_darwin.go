//go:build darwin

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

func supervisorOpenLogFiles(ctx context.Context, pid int) (_ []supervisorLogFile, resultError error) {
	defer func() {
		if resultError != nil {
			slog.WarnContext(ctx, "daemon.retention.supervisor_lsof_failed", "concern", "daemon.workers", "pid", pid, "err", resultError)
		}
	}()
	if pid <= 0 {
		return nil, fmt.Errorf("cannot inspect invalid supervisor process ID %d", pid)
	}
	slog.DebugContext(ctx, "daemon.retention.supervisor_lsof_started", "concern", "daemon.workers", "pid", pid)
	command := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-F0faGnt", "+fg")
	command.Args = append(command.Args, "-p", strconv.Itoa(pid))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("query lsof file flags: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var files []supervisorLogFile
	var current supervisorLogFile
	var regular, writable bool
	flush := func() {
		if !regular || !writable || current.path == "" {
			return
		}
		files = append(files, current)
	}
	confirmed := false
	for raw := range bytes.SplitSeq(output, []byte{0}) {
		field := strings.TrimLeft(string(raw), "\n")
		if field == "" {
			continue
		}
		switch field[0] {
		case 'p':
			confirmed = field[1:] == strconv.Itoa(pid)
		case 'f':
			flush()
			descriptor, parseErr := strconv.Atoi(field[1:])
			current = supervisorLogFile{path: "", descriptor: descriptor, appendMode: false}
			regular = false
			writable = parseErr == nil
		case 't':
			regular = field[1:] == "REG"
		case 'a':
			writable = writable && field[1:] != "r"
		case 'G':
			flags := strings.FieldsFunc(field[1:], func(character rune) bool { return character == ',' || character == ';' })
			current.appendMode = slices.Contains(flags, "AP")
		case 'n':
			current.path = field[1:]
		}
	}
	if !confirmed {
		return nil, fmt.Errorf("lsof did not confirm supervisor process %d", pid)
	}
	flush()
	return files, nil
}
