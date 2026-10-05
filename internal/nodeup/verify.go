package nodeup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/retry"
)

// agentURL is the node's own Nomad agent, which verify calls with the TLS name localhost.
var agentURL = "https://" + netip.AddrPortFrom(localAgent, apiPort).String()

// verify gives the agent healthTries failed tries, healthWait apart: about two minutes to turn healthy.
const (
	healthTries = 60
	healthWait  = 2 * time.Second
)

// maxExcerpt is how many characters of data an error or a log quotes.
const maxExcerpt = 200

// verify is the phase that checks that the units that run tent-node start at every boot, and that the node's Nomad
// agent is healthy. It changes nothing.
func verify(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
	sd := h.systemd()
	for _, unit := range []string{serviceUnit, joinTimerUnit} {
		state, enabled, err := sd.IsEnabled(ctx, unit)
		if err != nil {
			return Result{}, err
		}
		if !enabled {
			return Result{}, fmt.Errorf("%s is %s, not enabled", unit, state)
		}
	}
	if err := checkHealth(ctx, h, nc.Role, healthWait); err != nil {
		return Result{}, err
	}
	return Result{Status: Unchanged}, nil
}

// healthChecks returns the calls that tell whether the agent of the role is healthy, in the order that verify asks
// them. A server's needs no leader, which a new cluster lacks for a while; a client's turns healthy once the client
// knows a server.
func healthChecks(role v1alpha1.Role) []string {
	var paths []string
	if role.RunsServer() {
		paths = append(paths, "/v1/status/leader?stale")
	}
	if role.RunsClient() {
		paths = append(paths, "/v1/agent/health?type=client")
	}
	return paths
}

// checkHealth asks the node's agent the health checks of the role in order, each until it answers 200. It tries again
// wait after a failure that askHealth says may pass, logs the first one, and gives up after healthTries failed tries.
// When ctx ends, it stops waiting at once, and the error keeps the last failure.
func checkHealth(ctx context.Context, h *Host, role v1alpha1.Role, wait time.Duration) error {
	client, err := newAPIClient(h, "localhost")
	if err != nil {
		return fmt.Errorf("check the Nomad agent: %w", err)
	}
	defer client.close()
	checks := healthChecks(role)
	for failed := 0; len(checks) > 0; {
		again, err := askHealth(ctx, client, checks[0])
		if err == nil {
			checks = checks[1:]
			continue
		}
		failed++
		switch {
		case !again:
			return fmt.Errorf("check the Nomad agent: %w", err)
		case failed == healthTries:
			return fmt.Errorf("check the Nomad agent: not healthy after %d tries: %w", healthTries, err)
		case failed == 1:
			h.logger().Warn("the Nomad agent is not healthy yet, trying again", "error", err, "every", wait,
				"tries", healthTries)
		}
		if !retry.Sleep(ctx, wait) {
			return fmt.Errorf("check the Nomad agent: %w; stopped waiting: %w", err, context.Cause(ctx))
		}
	}
	return nil
}

// askHealth asks the agent the check at path once. again reports whether the failure may pass by waiting: a 500,
// which the agent answers until it has joined, or a lost connection, as while systemd restarts the agent. Waiting
// does not change the other failures: a TLS failure, such as another cluster's certificate, another status, or a call
// that times out on a stuck agent.
func askHealth(ctx context.Context, c *apiClient, path string) (again bool, err error) {
	code, body, err := c.get(ctx, agentURL+path)
	switch {
	case err != nil:
		return lostConnection(err), fmt.Errorf("GET %s: %w", path, withoutURL(err))
	case code == http.StatusOK:
		return false, nil
	}
	msg := fmt.Sprintf("GET %s answered %d %s", path, code, http.StatusText(code))
	if quote := excerpt(body); quote != "" {
		msg += ": " + quote
	}
	return code == http.StatusInternalServerError, errors.New(msg)
}

// lostConnection reports whether err is a connection that was refused, reset, or closed before the whole answer.
func lostConnection(err error) bool {
	for _, lost := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, io.EOF, io.ErrUnexpectedEOF} {
		if errors.Is(err, lost) {
			return true
		}
	}
	return false
}

// excerpt returns the start of data, such as an answer's body, for an error or a log, on one line: at most maxExcerpt
// characters of its first bytes, white space and characters that do not print turned into single spaces, and "..."
// where data goes on.
func excerpt(body []byte) string {
	n := min(len(body), maxExcerpt*utf8.UTFMax)
	words := strings.FieldsFunc(strings.ToValidUTF8(string(body[:n]), ""), func(r rune) bool {
		return unicode.IsSpace(r) || !unicode.IsPrint(r)
	})
	s := []rune(strings.Join(words, " "))
	cut := n < len(body)
	if len(s) > maxExcerpt {
		s, cut = s[:maxExcerpt], true
	}
	if cut {
		return string(s) + "..."
	}
	return string(s)
}
