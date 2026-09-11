// Command reset-service-manager records native installer commands without
// contacting launchd or systemd. It can start only a binary inside its temp root.
package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root := os.Getenv("CLYDE_RESET_TEST_ROOT")
	if !strings.HasPrefix(root, "/tmp/clyde-reset-") {
		return errors.New("fixture root is not isolated")
	}
	command := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if command == "systemctl" && len(args) > 0 && args[0] == "--user" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("missing fixture command")
	}
	action := args[0]
	file, err := os.OpenFile(filepath.Join(root, "commands"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(file, command, strings.Join(args, " "))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if os.Getenv("CLYDE_RESET_TEST_FAIL") == action {
		return errors.New("recording manager injected failure")
	}
	switch action {
	case "print", "status":
		if _, err := os.Stat(filepath.Join(root, "installed")); err == nil {
			_, _ = fmt.Fprintln(os.Stdout, "fixture daemon is running")
			return nil
		}
		if os.Getenv("CLYDE_RESET_TEST_ABSENT") == "1" {
			_, _ = fmt.Fprintln(os.Stderr, "Could not find service")
			os.Exit(113)
		}
		_, _ = fmt.Fprintln(os.Stdout, "fixture old registration")
		return nil
	case "show":
		if os.Getenv("CLYDE_RESET_TEST_ABSENT") == "1" {
			_, _ = fmt.Fprintln(os.Stdout, "not-found")
		} else {
			_, _ = fmt.Fprintln(os.Stdout, "loaded")
		}
		return nil
	case "bootout":
		if len(args) == 3 {
			return nil
		}
		return removeFixtureService(root)
	case "stop":
		return removeFixtureService(root)
	case "disable", "daemon-reload", "enable":
		return nil
	case "bootstrap", "restart":
		if err := checkData(root, false); err != nil {
			return err
		}
		path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "clyde-daemon.service")
		if action == "bootstrap" {
			path = args[2]
		}
		return startInstalledDaemon(root, path)
	default:
		return fmt.Errorf("unexpected service manager command %q", action)
	}
}

func removeFixtureService(root string) error {
	if err := checkData(root, true); err != nil {
		return err
	}
	if os.Getenv("CLYDE_RESET_TEST_LATE_WORKER") != "1" {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(root, "new-pid"))
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	socket := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "clyde", "daemon.sock")
	deadline := time.Now().Add(10 * time.Second)
	for {
		connection, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err != nil {
			break
		}
		_ = connection.Close()
		if time.Now().After(deadline) {
			return errors.New("old fixture worker did not release its listener")
		}
		time.Sleep(25 * time.Millisecond)
	}
	// Simulate a reload-spawned worker escaping the pre-removal snapshot. The
	// manager exits immediately afterward, so this worker reparents to init.
	command := exec.Command(filepath.Join(root, "clyde"), "daemon", "worker")
	command.Env = append(os.Environ(), "CLYDE_DAEMON_SUPERVISOR_SOCKET="+filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "clyde", "daemon.supervisor.sock"))
	if err := command.Start(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "late-pid"), []byte(strconv.Itoa(command.Process.Pid)), 0o600)
}

func checkData(root string, wantPresent bool) error {
	body, err := os.ReadFile(filepath.Join(root, "inventory"))
	if err != nil {
		return err
	}
	for _, path := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if wantPresent && err != nil {
			return fmt.Errorf("data deleted before teardown: %s: %w", path, err)
		}
		if !wantPresent && err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect data at installation: %s: %w", path, err)
		}
		if !wantPresent && bytes.Contains(data, []byte("old incompatible store bytes")) {
			return fmt.Errorf("old data remains at installation: %s", path)
		}
	}
	return nil
}

func startInstalledDaemon(root, servicePath string) error {
	body, err := os.ReadFile(servicePath)
	if err != nil {
		return err
	}
	arguments, environment, err := decodeService(body)
	if err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if len(arguments) != 3 || arguments[1] != "daemon" || arguments[2] != "run" || !strings.HasPrefix(arguments[0], resolvedRoot+"/") {
		return errors.New("installer selected an unexpected executable or command")
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		if environment[key] != os.Getenv(key) {
			return fmt.Errorf("service did not preserve %s", key)
		}
	}
	command := exec.Command(arguments[0], arguments[1:]...)
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	log, err := os.Create(filepath.Join(root, "new-daemon.log"))
	if err != nil {
		return err
	}
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		_ = log.Close()
		return err
	}
	_ = log.Close()
	if err := os.WriteFile(filepath.Join(root, "new-pid"), []byte(strconv.Itoa(command.Process.Pid)), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		var dialer net.Dialer
		connection, err := dialer.DialContext(ctx, "unix", filepath.Join(environment["XDG_RUNTIME_DIR"], "clyde", "daemon.sock"))
		if err == nil {
			_ = connection.Close()
			return os.WriteFile(filepath.Join(root, "installed"), []byte("ready"), 0o600)
		}
		select {
		case <-ctx.Done():
			return errors.New("installed fixture daemon did not start")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func decodeService(body []byte) ([]string, map[string]string, error) {
	environment := make(map[string]string)
	var arguments []string
	if !bytes.HasPrefix(body, []byte("<?xml")) {
		for _, line := range strings.Split(string(body), "\n") {
			if value, found := strings.CutPrefix(line, "ExecStart="); found {
				arguments = strings.Fields(value)
			}
			value, found := strings.CutPrefix(line, "Environment=")
			if !found {
				continue
			}
			if strings.HasPrefix(value, "\"") {
				var err error
				value, err = strconv.Unquote(value)
				if err != nil {
					return nil, nil, err
				}
			}
			key, value, _ := strings.Cut(value, "=")
			environment[key] = strings.ReplaceAll(value, "%%", "%")
		}
		return arguments, environment, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	key := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return nil, nil, err
			}
		} else if start.Name.Local == "array" && key == "ProgramArguments" {
			var list struct {
				Values []string `xml:"string"`
			}
			if err := decoder.DecodeElement(&list, &start); err != nil {
				return nil, nil, err
			}
			arguments = list.Values
		} else if start.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return nil, nil, err
			}
			environment[key] = value
		}
	}
	return arguments, environment, nil
}
