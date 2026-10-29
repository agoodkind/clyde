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
	command := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-F0faGntsDik", "+fg")
	command.Args = append(command.Args, "-p", strconv.Itoa(pid))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("query lsof file flags: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return readSupervisorLogFiles(output, pid)
}

func readSupervisorLogFiles(output []byte, pid int) ([]supervisorLogFile, error) {
	var files []supervisorLogFile
	var current supervisorLogFile
	var metadata [4]string
	var regular, writable bool
	flush := func() error {
		if !regular || !writable || current.path == "" {
			return nil
		}
		file, err := readSupervisorLogMetadata(current, metadata)
		if err != nil {
			return err
		}
		files = append(files, file)
		return nil
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
			if err := flush(); err != nil {
				return nil, err
			}
			descriptor, parseErr := strconv.Atoi(field[1:])
			current = supervisorLogFile{path: "", descriptor: descriptor, appendMode: false, size: 0, links: 0, identity: supervisorLogIdentity{device: 0, inode: 0}}
			metadata = [4]string{}
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
		case 's':
			metadata[0] = field[1:]
		case 'D':
			metadata[1] = field[1:]
		case 'i':
			metadata[2] = field[1:]
		case 'k':
			metadata[3] = field[1:]
		}
	}
	if !confirmed {
		return nil, fmt.Errorf("lsof did not confirm supervisor process %d", pid)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return files, nil
}

func readSupervisorLogMetadata(file supervisorLogFile, metadata [4]string) (supervisorLogFile, error) {
	size, err := strconv.ParseInt(metadata[0], 10, 64)
	if err != nil {
		slog.Warn("daemon.retention.supervisor_size_failed", "concern", "daemon.workers", "descriptor", file.descriptor, "err", err)
		return file, fmt.Errorf("parse lsof descriptor %d size: %w", file.descriptor, err)
	}
	if size < 0 {
		return file, fmt.Errorf("lsof descriptor %d reports negative size %d", file.descriptor, size)
	}
	var numbers [3]uint64
	for index, value := range metadata[1:] {
		base := 10
		if index == 0 {
			base = 0
		}
		number, err := strconv.ParseUint(value, base, 64)
		if err != nil {
			slog.Warn("daemon.retention.supervisor_metadata_failed", "concern", "daemon.workers", "descriptor", file.descriptor, "field", index, "err", err)
			return file, fmt.Errorf("parse lsof descriptor %d metadata field %d: %w", file.descriptor, index, err)
		}
		numbers[index] = number
	}
	file.size = size
	file.identity = supervisorLogIdentity{device: numbers[0], inode: numbers[1]}
	file.links = numbers[2]
	return file, nil
}
