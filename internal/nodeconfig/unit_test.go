package nodeconfig_test

import (
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// TestRenderNomadService checks the unit that runs the Nomad agent: its path, mode and owner, and its content
// against the golden.
func TestRenderNomadService(t *testing.T) {
	f := nodeconfig.RenderNomadService()
	if f.Path != "/etc/systemd/system/nomad.service" {
		t.Errorf("path = %q, want /etc/systemd/system/nomad.service", f.Path)
	}
	if f.Mode != 0o644 || f.Owner != nodeconfig.Owner || f.Secret || f.PerNode {
		t.Errorf("mode %#o, owner %s, secret %v, per node %v; want 0644, root:root and neither",
			f.Mode, f.Owner, f.Secret, f.PerNode)
	}
	checkGolden(t, "nomad.service.golden", string(f.Content))
}

// TestNomadServiceStopsGracefully checks the settings that a graceful stop needs: SIGTERM, which leave_on_terminate
// answers, systemd's default stop timeout of 90 seconds, which covers Nomad's 5-second graceful wait, and a KillMode
// that lets executors and logmon survive a restart.
func TestNomadServiceStopsGracefully(t *testing.T) {
	content := string(nodeconfig.RenderNomadService().Content)
	for _, line := range []string{"KillMode=process", "KillSignal=SIGTERM"} {
		if !strings.Contains(content, line+"\n") {
			t.Errorf("nomad.service has no %q", line)
		}
	}
	for line := range strings.Lines(content) {
		if strings.HasPrefix(line, "TimeoutStopSec=") || strings.HasPrefix(line, "TimeoutSec=") {
			t.Errorf("nomad.service has %q, want systemd's default stop timeout", strings.TrimSpace(line))
		}
	}
}
