package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/clyde/internal/daemonsupervisor"
)

type supervisorReplacementDaemonStarter struct {
	socketPath string
}

func (s supervisorReplacementDaemonStarter) startReplacementDaemon(ctx context.Context, log *slog.Logger, req replacementDaemonRequest) (*replacementDaemonProcess, error) {
	socketPath := strings.TrimSpace(s.socketPath)
	if socketPath == "" {
		return nil, fmt.Errorf("supervisor reload socket is empty")
	}
	files := append([]*os.File{}, req.files...)
	files = append(files, req.readyWrite)
	pid, err := daemonsupervisor.RequestReplacement(
		ctx,
		socketPath,
		req.executablePath,
		supervisorListenerSpecs(req.specs),
		req.readyFD,
		os.Environ(),
		files,
	)
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.supervisor_request_failed",
			"component", "daemon",
			"socket_path", socketPath,
			"path", req.executablePath,
			"err", err,
		)
		return nil, fmt.Errorf("request supervisor replacement daemon: %w", err)
	}
	log.InfoContext(ctx, "daemon.reload.supervisor_replacement_started",
		"component", "daemon",
		"socket_path", socketPath,
		"new_pid", pid,
	)
	return &replacementDaemonProcess{
		pid:  pid,
		wait: func() error { return nil },
		kill: func() error { return nil },
	}, nil
}

func supervisorListenerSpecs(specs []inheritedListenerSpec) []daemonsupervisor.ListenerSpec {
	out := make([]daemonsupervisor.ListenerSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, daemonsupervisor.ListenerSpec{
			Name:    spec.Name,
			Network: spec.Network,
			Addr:    spec.Addr,
			FD:      spec.FD,
		})
	}
	return out
}
