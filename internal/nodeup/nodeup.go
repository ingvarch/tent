// Package nodeup turns a machine into a Nomad agent of its node group: the phases of tent-node up and the runner that
// runs them. The phases reach the machine through a filesystem, a program runner and facts about the machine, which
// tests replace with fakes.
package nodeup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"slices"
	"time"

	"github.com/ingvarch/tent/internal/buildinfo"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/env"
)

// StatusFile is where Up writes the outcome of its last run, for the operator. Only root can read it.
const StatusFile = "/var/lib/tent/status.json"

// Host is the machine as the phases see it. Local returns the machine that tent-node runs on.
type Host struct {
	FS     FS
	Runner Runner
	// Transport downloads the assets; nil uses http.DefaultTransport.
	Transport http.RoundTripper
	// DialContext dials the Nomad API; nil uses the default dialer. It exists for tests, which cannot bind the
	// servers' addresses.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Log         *slog.Logger // nil logs nothing

	Version  string // tent-node's version
	HostName string
	EUID     int    // the effective user id, -1 on systems without one
	OS       string // as GOOS names it, such as linux
	Arch     string // as GOARCH names it, such as amd64
	Now      func() time.Time

	// Instance is the machine as its cloud knows it. The phase that reads the cloud's metadata service fills it in,
	// and Up writes it into the status.
	Instance Instance
}

// Instance is the machine as its cloud knows it, as the cloud's metadata service describes it.
type Instance = env.Instance

// Local returns the machine that tent-node runs on: its own filesystem and programs, its host name, its user, its
// platform and its clock. It logs to log.
func Local(log *slog.Logger) (*Host, error) {
	name, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read the host name: %w", err)
	}
	return &Host{
		FS: OSFS{}, Runner: ExecRunner{}, Log: log,
		Version: buildinfo.Get().Version, HostName: name, EUID: os.Geteuid(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		Now: time.Now,
	}, nil
}

// logger returns the host's logger, or one that logs nothing.
func (h *Host) logger() *slog.Logger {
	if h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

// systemd returns the host's systemd, which the host's runner reaches.
func (h *Host) systemd() Systemd { return Systemd{Runner: h.Runner} }

// Status is the outcome of a phase.
type Status string

// Outcomes of a phase.
const (
	Done      Status = "done"      // it changed the machine
	Unchanged Status = "unchanged" // the machine was as it should be
	Skipped   Status = "skipped"   // it does not apply to the node, or did not run
	Failed    Status = "failed"    // it stopped with an error
)

// Result is what a phase did: Done, Unchanged or Skipped, and why, such as "servers run no workloads" for the cni
// phase on a server. A phase that fails returns an error instead.
type Result struct {
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Phase is a step of tent-node up. Run compares the machine with the NodeConfig and changes only what differs, so
// that a second run changes nothing.
type Phase struct {
	Name string
	Run  func(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error)
}

// PhaseResult is the result of a phase in a Report.
type PhaseResult struct {
	Name string `json:"name"`
	Result
}

// Report is the outcome of a run of Up, which it writes into StatusFile as JSON.
type Report struct {
	Version  string        `json:"version"` // tent-node's
	SpecHash string        `json:"specHash,omitempty"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
	Instance Instance      `json:"instance,omitzero"`
	Phases   []PhaseResult `json:"phases"`
}

// Up runs the phases on the machine, in order, and stops at the first that fails; once ctx has ended, the next phase
// does not start and fails with the reason "not started" and ctx's error. Every phase gets a result: those after a
// failure are skipped. Then, whatever happened, Up writes the report into StatusFile, and returns it with the
// failure, which names the phase, and the error of writing the status, if there is one.
func Up(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig, phases []Phase) (Report, error) {
	log := h.logger()
	log.Info("tent-node up", "version", h.Version, "node", nc.Name, "specHash", nc.SpecHash)
	r := Report{
		Version: h.Version, SpecHash: nc.SpecHash, Started: h.Now().UTC(),
		Phases: make([]PhaseResult, 0, len(phases)),
	}
	var failed string // the name of the phase that failed
	var err error
	for _, p := range phases {
		if failed != "" {
			notRun := Result{Status: Skipped, Reason: "not run: " + failed + " failed"}
			r.Phases = append(r.Phases, PhaseResult{p.Name, notRun})
			continue
		}
		res, perr := runPhase(ctx, h, nc, p)
		if perr != nil {
			failed, err = p.Name, perr
			log.Error("phase failed", "phase", p.Name, "error", perr)
		} else {
			log.Info("phase", "phase", p.Name, "status", res.Status, "reason", res.Reason)
		}
		r.Phases = append(r.Phases, PhaseResult{p.Name, res})
	}
	r.Finished = h.Now().UTC()
	r.Instance = h.Instance
	if werr := writeStatus(h.FS, r); werr != nil {
		log.Error("writing the status failed", "error", werr)
		err = errors.Join(err, werr)
	}
	return r, err
}

// runPhase runs the phase p and returns its result, or the result of a failure and its error. Once ctx has ended, the
// phase does not start and fails.
func runPhase(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig, p Phase) (Result, error) {
	var res Result
	err := context.Cause(ctx)
	if err != nil {
		err = fmt.Errorf("not started: %w", err)
	} else {
		res, err = p.Run(ctx, h, nc)
	}
	if err == nil && !slices.Contains([]Status{Done, Unchanged, Skipped}, res.Status) {
		err = fmt.Errorf("the phase returned the status %q without an error", res.Status)
	}
	if err != nil {
		return Result{Status: Failed, Reason: err.Error()}, fmt.Errorf("phase %s: %w", p.Name, err)
	}
	return res, nil
}

// writeStatus writes the report into StatusFile, in a directory that only root can read.
func writeStatus(fsys FS, r Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("write the status: %w", err)
	}
	if _, err := fsys.EnsureDir(path.Dir(StatusFile), 0o700, nodeconfig.Owner); err != nil {
		return fmt.Errorf("write the status: %w", err)
	}
	if _, err := fsys.WriteFile(StatusFile, append(data, '\n'), 0o600, nodeconfig.Owner); err != nil {
		return fmt.Errorf("write the status: %w", err)
	}
	return nil
}
