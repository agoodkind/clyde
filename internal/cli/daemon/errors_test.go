package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	daemoncmd "goodkind.io/clyde/internal/cli/daemon"
)

func TestReloadCommandErrorPreservesDeadlineCauseAndLogs(t *testing.T) {
	const recoveryText = "daemon reload did not complete within 1m15s and " +
		"no reload confirmation was received; check `clyde daemon status` and " +
		"the daemon log, then rerun `clyde daemon reload`: "

	for _, testCase := range []struct {
		name  string
		cause error
	}{
		{name: "deadline", cause: context.DeadlineExceeded},
		{name: "wrapped deadline", cause: fmt.Errorf("reload RPC: %w", context.DeadlineExceeded)},
	} {
		t.Run(
			testCase.name,
			func(t *testing.T) {
				var output bytes.Buffer
				previousLogger := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
				t.Cleanup(func() { slog.SetDefault(previousLogger) })

				result := daemoncmd.ReloadCommandError(testCase.cause)
				if result == nil || result.Error() != recoveryText+testCase.cause.Error() {
					t.Fatalf("reload error = %v, want %s%v", result, recoveryText, testCase.cause)
				}

				if !errors.Is(result, testCase.cause) ||
					!errors.Is(result, context.DeadlineExceeded) {
					t.Fatalf("reload error does not retain deadline cause: %v", result)
				}

				var event struct {
					Message string `json:"msg"`
					Level   string `json:"level"`
					Concern string `json:"concern"`
					Error   string `json:"err"`
				}
				if err := json.Unmarshal(output.Bytes(), &event); err != nil {
					t.Fatalf("decode timeout warning: %v", err)
				}

				if event.Message != "cli.daemon.reload_timed_out" || event.Level != "WARN" ||
					event.Concern != "process.daemon.lifecycle" ||
					event.Error != testCase.cause.Error() {
					t.Fatalf("timeout warning = %+v", event)
				}
			},
		)
	}
}

func TestReloadCommandErrorPreservesOtherErrors(t *testing.T) {
	t.Parallel()

	for _, cause := range []error{nil, context.Canceled, errors.New("reload refused")} {
		if result := daemoncmd.ReloadCommandError(cause); result != cause {
			t.Fatalf("reload error = %v, want original %v", result, cause)
		}
	}
}
