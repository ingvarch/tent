package nomadops

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/ingvarch/tent/internal/english"
)

// pollEvery is how long a wait waits after a call that did not end it.
const pollEvery = 2 * time.Second

// WaitLeader calls Leader until the cluster has a leader, and returns the leader's RPC address. It waits 2 seconds
// after each call. It fails at once with an error of Leader that does not match ErrNotReady. When ctx ends, it fails
// with an error that matches ctx.Err() and names the error of the last call.
func WaitLeader(ctx context.Context, a API) (string, error) {
	var leader string
	err := poll(ctx, "a leader", func(ctx context.Context) error {
		var err error
		leader, err = a.Leader(ctx)
		return err
	})
	if err != nil {
		return "", err
	}
	return leader, nil
}

// WaitNode calls Nodes until a node called name that advertises addr is ready and eligible, and returns that node. It
// waits 2 seconds after each call. Nodes of the name at other addresses, such as a machine's twin or the node of an
// earlier machine, are ignored, and an invalid addr matches no node. When several nodes have the name at addr, any
// ready one ends the wait. It fails at once with an error of Nodes that does not match ErrNotReady. When ctx ends, it
// fails with an error that matches ctx.Err() and says what the last list showed of the name at addr.
func WaitNode(ctx context.Context, a API, name string, addr netip.Addr) (Node, error) {
	var node Node
	err := poll(ctx, "node "+name, func(ctx context.Context) error {
		nodes, err := a.Nodes(ctx)
		if err != nil {
			return err
		}
		var states []string
		for _, n := range nodes {
			switch {
			case !n.Is(name, addr):
			case n.Ready():
				node = n
				return nil
			case n.Status == "ready":
				states = append(states, "ready but not eligible")
			default:
				states = append(states, n.Status)
			}
		}
		switch len(states) {
		case 0:
			return pending("node " + name + " is not listed at " + addr.String())
		case 1:
			return pending("node " + name + " is " + states[0])
		}
		return pending("the nodes named " + name + " at " + addr.String() + " are " + english.And(states))
	})
	if err != nil {
		return Node{}, err
	}
	return node, nil
}

// WaitHealthy calls Health until autopilot reports healthy servers with at least the given number of voters, and
// returns that Health. It waits 2 seconds after each call. It fails at once with an error of Health that does not
// match ErrNotReady. When ctx ends, it fails with an error that matches ctx.Err() and says what the last Health
// showed.
//
// It checks for at least that many voters, not exactly that many: after a server is removed, a wait for one voter
// fewer succeeds while the removed server still votes.
func WaitHealthy(ctx context.Context, a API, voters int) (Health, error) {
	var h Health
	err := poll(ctx, fmt.Sprintf("healthy servers with at least %d voters", voters), func(ctx context.Context) error {
		var err error
		h, err = a.Health(ctx)
		switch {
		case err != nil:
			return err
		case !h.Healthy:
			return pending(fmt.Sprintf("the servers are unhealthy, with %d voters", h.Voters))
		case h.Voters < voters:
			return pending(fmt.Sprintf("the servers are healthy, with %d voters", h.Voters))
		}
		return nil
	})
	if err != nil {
		return Health{}, err
	}
	return h, nil
}

// WaitKeyring calls KeyringReady until the keyring has an active key. It waits 2 seconds after each call. It fails at
// once with an error of KeyringReady that does not match ErrNotReady. When ctx ends, it fails with an error that
// matches ctx.Err() and names the error of the last call, or says that the keyring has no active key.
func WaitKeyring(ctx context.Context, a API) error {
	return poll(ctx, "the keyring", func(ctx context.Context) error {
		ready, err := a.KeyringReady(ctx)
		switch {
		case err != nil:
			return err
		case !ready:
			return pending("the keyring has no active key")
		}
		return nil
	})
}

// pending is why a wait goes on after a call that succeeded, such as a node that is still initializing. It matches
// ErrNotReady.
type pending string

func (p pending) Error() string { return string(p) }

// Is makes errors.Is hold for ErrNotReady.
func (pending) Is(target error) bool { return target == ErrNotReady }

// poll runs try until it returns nil, waiting pollEvery after each try, and returns nil then. It returns at once an
// error of try that does not match ErrNotReady. When ctx ends, during try or between two, poll fails with an error that
// names what it waited for and the error of the last try that did not end with ctx. That error matches ctx.Err() and
// not ErrNotReady, so a caller does not take it for a reason to try again.
func poll(ctx context.Context, what string, try func(context.Context) error) error {
	var last error
	for {
		err := try(ctx)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ended(ctx, what, last)
		case !errors.Is(err, ErrNotReady):
			return err
		}
		last = err
		select {
		case <-ctx.Done():
			return ended(ctx, what, last)
		case <-time.After(pollEvery):
		}
	}
}

// ended returns the error of a wait for what whose context ended after the last error.
func ended(ctx context.Context, what string, last error) error {
	if last == nil {
		return fmt.Errorf("nomad: wait for %s: %w", what, ctx.Err())
	}
	// Only the text of last: the error must not match ErrNotReady.
	return fmt.Errorf("nomad: wait for %s: %w; last: %s", what, ctx.Err(), last.Error())
}
