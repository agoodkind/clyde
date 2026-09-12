package clispec

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestInstallSetupRequiresAComponent(t *testing.T) {
	t.Parallel()
	if _, err := prepareInstallSetup(installSetupInput{}); err == nil || !strings.Contains(err.Error(), "component") {
		t.Fatalf("prepare without component = %v", err)
	}
}

func TestInstallSetupRunsOnlySelectedComponents(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "daemon", args: []string{"--daemon"}, want: "daemon"},
		{name: "hooks", args: []string{"--hooks"}, want: "hooks"},
		{name: "mcp", args: []string{"--mcp"}, want: "mcp"},
		{name: "all", args: []string{"--daemon", "--hooks", "--mcp"}, want: "daemon,hooks,mcp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			dependencies := installSetupDependencies{
				installDaemon: func(context.Context, ResultSink) error {
					calls = append(calls, "daemon")
					return nil
				},
				installHooks: func(context.Context, ResultSink) error {
					calls = append(calls, "hooks")
					return nil
				},
				installMCP: func(context.Context, ResultSink) error {
					calls = append(calls, "mcp")
					return nil
				},
			}
			op := installSetupOpWithDependencies(dependencies)
			command := op.cobraCommand(testFactory(&bytes.Buffer{}))
			command.SetArgs(test.args)
			if err := command.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := strings.Join(calls, ","); got != test.want {
				t.Fatalf("calls = %q, want %q", got, test.want)
			}
		})
	}
}
