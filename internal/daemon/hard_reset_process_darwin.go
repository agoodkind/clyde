//go:build darwin

package daemon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"goodkind.io/clyde/internal/daemonsupervisor"
)

func resetProcessOwnedStart(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", resetDarwinProcessError(pid, err)
	}
	if int64(info.Eproc.Ucred.Uid) != int64(os.Getuid()) {
		return "", fmt.Errorf("process %d belongs to another user", pid)
	}
	if info.Proc.P_stat == 5 {
		return "", os.ErrNotExist
	}
	return fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), nil
}

func resetProcessDetails(pid int) (resetProcess, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return resetProcess{}, resetDarwinProcessError(pid, err)
	}
	if int64(info.Eproc.Ucred.Uid) != int64(os.Getuid()) {
		return resetProcess{}, fmt.Errorf("process %d belongs to another user", pid)
	}
	if info.Proc.P_stat == 5 {
		return resetProcess{}, os.ErrNotExist
	}
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return resetProcess{}, resetDarwinProcessError(pid, err)
	}
	process, err := decodeResetProcessArguments(pid, data)
	if err != nil {
		return resetProcess{}, err
	}
	process.Started = fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec)
	return process, nil
}

func resetDarwinProcessError(pid int, original error) error {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return os.ErrNotExist
	}
	// procargs disappears when a process becomes a zombie between the two
	// sysctl reads. A zombie holds no database files and cannot write again.
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return original
	}
	for _, process := range processes {
		if int(process.Proc.P_pid) == pid {
			if process.Proc.P_stat == 5 {
				return os.ErrNotExist
			}
			return original
		}
	}
	return os.ErrNotExist
}

func decodeResetProcessArguments(pid int, data []byte) (resetProcess, error) {
	if len(data) < 4 {
		return resetProcess{}, fmt.Errorf("process %d arguments are unavailable", pid)
	}
	count := int(binary.NativeEndian.Uint32(data[:4]))
	executable, tail, found := bytes.Cut(data[4:], []byte{0})
	if !found || count < 1 {
		return resetProcess{}, fmt.Errorf("process %d arguments are malformed", pid)
	}
	tail = bytes.TrimLeft(tail, "\x00")
	parts := bytes.Split(tail, []byte{0})
	if count > len(parts) {
		return resetProcess{}, fmt.Errorf("process %d arguments are truncated", pid)
	}
	result := resetProcess{PID: pid, Executable: string(executable), Started: "", Arguments: nil, SupervisorSocket: ""}
	for _, argument := range parts[:count] {
		result.Arguments = append(result.Arguments, string(argument))
	}
	for _, entry := range parts[count:] {
		if value, ok := strings.CutPrefix(string(entry), daemonsupervisor.EnvSupervisorSocket+"="); ok {
			result.SupervisorSocket = value
		}
	}
	return result, nil
}
