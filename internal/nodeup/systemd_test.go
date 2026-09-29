package nodeup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

func TestSystemdCommands(t *testing.T) {
	r := &nodeuptest.Runner{}
	sd := nodeup.Systemd{Runner: r}
	ctx := t.Context()
	for _, err := range []error{
		sd.DaemonReload(ctx),
		sd.Enable(ctx, "tent-node.service", "tent-node-join.timer"),
		sd.Start(ctx, "tent-node.service"),
		sd.Restart(ctx, "systemd-journald.service"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"systemctl daemon-reload",
		"systemctl enable tent-node.service tent-node-join.timer",
		"systemctl start tent-node.service",
		"systemctl restart systemd-journald.service",
	}
	if diff := cmp.Diff(want, r.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
}

func TestSystemdFailure(t *testing.T) {
	r := &nodeuptest.Runner{}
	r.On("systemctl start nomad.service", nodeuptest.Exit(1, "Job for nomad.service failed."))
	err := nodeup.Systemd{Runner: r}.Start(t.Context(), "nomad.service")
	if want := "systemctl start nomad.service: exit status 1: Job for nomad.service failed."; errText(err) != want {
		t.Errorf("Start: %q, want %q", errText(err), want)
	}
}

func TestSystemdStates(t *testing.T) {
	broken := func(context.Context) ([]byte, error) { return nil, errors.New("systemctl is missing") }
	noBus := nodeuptest.Exit(1, "System has not been booted with systemd as init system (PID 1). Can't operate.")
	type stateCase struct {
		name   string
		answer nodeuptest.Answer
		state  string // "" when systemctl gives none, and the call fails
		yes    bool
	}
	checks := []struct {
		command string
		state   func(context.Context, nodeup.Systemd) (string, bool, error)
		cases   []stateCase
	}{
		{
			"systemctl is-enabled tent-node.service",
			func(ctx context.Context, sd nodeup.Systemd) (string, bool, error) {
				return sd.IsEnabled(ctx, "tent-node.service")
			},
			[]stateCase{
				{"enabled", nodeuptest.Output("enabled\n"), "enabled", true},
				{"static", nodeuptest.Output("static\n"), "static", false},
				{"enabled until the next boot", nodeuptest.Output("enabled-runtime\n"), "enabled-runtime", false},
				{"disabled", nodeuptest.ExitOutput(1, "disabled\n", ""), "disabled", false},
				{"masked", nodeuptest.ExitOutput(1, "masked\n", ""), "masked", false},
				{"not found", nodeuptest.ExitOutput(4, "not-found\n", ""), "not-found", false},
				{"not found, without a state", nodeuptest.Exit(1, "Failed to get unit file state for "+
					"tent-node.service: No such file or directory"), "", false},
				{"no systemd", noBus, "", false},
				{"no state at all", nodeuptest.Output(""), "", false},
				{"systemctl broken", broken, "", false},
			},
		},
		{
			"systemctl is-active nomad.service",
			func(ctx context.Context, sd nodeup.Systemd) (string, bool, error) {
				return sd.IsActive(ctx, "nomad.service")
			},
			[]stateCase{
				{"active", nodeuptest.Output("active\n"), "active", true},
				{"reloading", nodeuptest.Output("reloading\n"), "reloading", true},
				{"inactive", nodeuptest.ExitOutput(3, "inactive\n", ""), "inactive", false},
				{"activating", nodeuptest.ExitOutput(3, "activating\n", ""), "activating", false},
				{"failed", nodeuptest.ExitOutput(3, "failed\n", ""), "failed", false},
				{"no systemd", noBus, "", false},
				{"no state at all", nodeuptest.Output(""), "", false},
				{"systemctl broken", broken, "", false},
			},
		},
	}
	for _, check := range checks {
		for _, c := range check.cases {
			t.Run(check.command+" "+c.name, func(t *testing.T) {
				r := &nodeuptest.Runner{}
				r.On(check.command, c.answer)
				state, yes, err := check.state(t.Context(), nodeup.Systemd{Runner: r})
				if state != c.state || yes != c.yes || (err != nil) != (c.state == "") {
					t.Errorf("%s: %q, %v, %v; want %q, %v and an error %v", check.command, state, yes, err, c.state,
						c.yes, c.state == "")
				}
				if err != nil && !strings.HasPrefix(err.Error(), check.command+": ") {
					t.Errorf("%s: the error %q does not name the command", check.command, err)
				}
			})
		}
	}
}

func TestSystemdNeedsReload(t *testing.T) {
	const command = "systemctl show -p NeedDaemonReload --value tent-node.service"
	cases := []struct {
		name   string
		answer nodeuptest.Answer
		want   bool
		err    string
	}{
		{"yes", nodeuptest.Output("yes\n"), true, ""},
		{"no", nodeuptest.Output("no\n"), false, ""},
		{"something else", nodeuptest.Output("maybe\n"), false, command + `: printed "maybe", not yes or no`},
		{"no systemd", nodeuptest.Exit(1, "Failed to connect to bus: No such file or directory"), false,
			command + ": exit status 1: Failed to connect to bus: No such file or directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &nodeuptest.Runner{}
			r.On(command, c.answer)
			got, err := nodeup.Systemd{Runner: r}.NeedsReload(t.Context(), "tent-node.service")
			if got != c.want || errText(err) != c.err {
				t.Errorf("NeedsReload: %v, %q; want %v, %q", got, errText(err), c.want, c.err)
			}
		})
	}
}

// errText returns the error's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
