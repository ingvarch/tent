package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

const (
	// uiGrace is how long tent ui waits for open requests at its end before it closes them.
	uiGrace = 5 * time.Second
	// uiReadHeaderTimeout is how long a client has to send the headers of a request.
	uiReadHeaderTimeout = 10 * time.Second
	// uiDefaultListen is the address that tent ui listens on unless --listen says otherwise.
	uiDefaultListen = "127.0.0.1:4646"
)

// uiSettings are the parts of tent ui that tests replace; the zero value is the real thing.
type uiSettings struct {
	// listen opens the listener at an address; a TCP listener when nil.
	listen func(ctx context.Context, address string) (net.Listener, error)
	// uiClock is the clock that the session's end follows; the real one when its parts are nil.
	uiClock
	// grace is how long to wait for open requests at the end; uiGrace when zero.
	grace time.Duration
}

// shutdownGrace returns how long tent ui waits for open requests at its end.
func (s uiSettings) shutdownGrace() time.Duration {
	return cmp.Or(s.grace, uiGrace)
}

func newUICommand(opts *globalOptions) *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "ui [NAME]",
		Short: "Serve the Nomad UI and API of a cluster on a loopback port",
		Long: "Serve the web UI and the API of the Nomad cluster named by NAME or --name on a port of this machine, " +
			"and keep serving until Ctrl-C or the end of the session. The port needs no certificate and no token: " +
			"tent passes each request to one of the cluster's servers over mutual TLS with an operator " +
			"certificate and a management token that it made for this run, in place of any token the request " +
			"carries. While the command runs, everyone who can reach the port on this machine has that " +
			"management token, so the command listens on a loopback address only: --listen takes an address in " +
			"127.0.0.0/8, ::1 or localhost, and port 0 lets the system pick one. A request whose Host or Origin " +
			"is another is refused. The command prints the URL of the UI on stdout, and on stderr a notice that " +
			"gives the end of the session. The Nomad CLI works through the port too: set NOMAD_ADDR to " +
			"the address in the notice; the CLI needs no certificate and no token then. tent opens no browser. " +
			"The certificate and the token work for 24 hours; the session ends then, and tent stops as it does " +
			"on Ctrl-C, after it waits up to 5 seconds for open requests. The token is not deleted at the end: " +
			"it expires. With -o json or -o yaml the command prints the cluster, the URL and the end of the " +
			"session instead of the first line. The token's secret and the key are never printed. With -v " +
			"the log tells each request's method, path and status. The command needs the cloud's credentials in " +
			"the environment, VULTR_API_KEY for Vultr, and a way to port 4646 of the servers, which " +
			"spec.access.api allows. It changes nothing in the state store and takes no lock.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkLoopbackListen(listen); err != nil {
				return err
			}
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			return opts.runUI(cmd, name, listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", uiDefaultListen, "the loopback address to serve on, host:port")
	return cmd
}

// checkLoopbackListen fails unless listen is host:port, where host is localhost or an address in 127.0.0.0/8 or ::1,
// and port is a number from 0 to 65535.
func checkLoopbackListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %s: want host:port, such as %s", listen, uiDefaultListen)
	}
	if !loopbackHost(host) {
		return loopbackOnly(listen)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("--listen %s: the port must be a number from 0 to 65535", listen)
	}
	return nil
}

// loopbackHost reports whether host is localhost or a loopback IP address, one that is no IPv4-mapped address and has
// no zone.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Zone() == "" && !ip.Is4In6() && ip.IsLoopback()
}

// loopbackOnly is the error for an address that is not loopback.
func loopbackOnly(listen string) error {
	return fmt.Errorf("--listen %s: tent ui listens on a loopback address only", listen)
}

// runUI serves the UI of the cluster on listen until Ctrl-C or the end of the session. It listens before it asks
// Nomad for a token, so that a port in use leaves no token, and it closes the listener on every way out.
func (o *globalOptions) runUI(cmd *cobra.Command, name, listen string) error {
	if o.nomadProxy == nil {
		return errors.New("no Nomad proxy is set up")
	}
	svc, err := o.service(cmd, v1alpha1.ValidateOptions{})
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	ln, err := o.listenLoopback(ctx, listen)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	address := ln.Addr().String()
	access, err := svc.OperatorAccess(ctx, name, "ui", accessTTL)
	if err != nil {
		return err
	}
	handler, err := o.nomadProxy(nomadops.ProxyConfig{
		Servers: access.Servers, Region: access.Region, CA: access.CA,
		Cert: pki.Certificate{Cert: access.Cert, Key: access.Key}, Token: access.Token, Listen: address, Log: o.logger,
	})
	if err != nil {
		return fmt.Errorf("set up the proxy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return interrupted(ctx, err)
	}
	session := uiSession{Cluster: name, URL: "http://" + address + "/ui/", Expires: access.Until.Truncate(time.Second)}
	if err := printObject(cmd.OutOrStdout(), o.output, session, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Nomad UI of cluster %s: %s\n", name, session.URL)
		return err
	}); err != nil {
		return err
	}
	// A notice that fails to print changes nothing.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the Nomad CLI works through it with NOMAD_ADDR=http://%s; "+
		"press Ctrl-C to stop; the session ends at %s\n", address, session.Expires.Format(noticeTime))
	ended, err := serveUntil(ctx, newUIServer(handler, o.logger), ln, access.Until, o.ui.uiClock, o.ui.shutdownGrace())
	if ended {
		_, _ = io.WriteString(cmd.ErrOrStderr(), "the session of tent ui ended: its certificate and token expired; "+
			"run it again\n")
	}
	return err
}

// uiSession is what tent ui prints with -o json and -o yaml.
type uiSession struct {
	Cluster string    `json:"cluster"`
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"` // the end of the session, in whole seconds
}

// listenLoopback opens a listener at listen, and closes it again, with an error, when its real address is not
// loopback: localhost is resolved by the system.
func (o *globalOptions) listenLoopback(ctx context.Context, listen string) (net.Listener, error) {
	open := o.ui.listen
	if open == nil {
		open = func(ctx context.Context, address string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(ctx, "tcp", address)
		}
	}
	ln, err := open(ctx, listen)
	if err != nil {
		if opErr, ok := errors.AsType[*net.OpError](err); ok {
			err = opErr.Err
		}
		return nil, fmt.Errorf("listen on %s: %w; pick another address with --listen", listen, err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return nil, loopbackOnly(listen)
	}
	return ln, nil
}

// uiClockCheck is the longest that tent ui waits before it reads the clock again: a timer does not count the time
// that the machine sleeps.
const uiClockCheck = time.Minute

// uiClock is the wall clock and the timer of tent ui; the real ones when nil.
type uiClock struct {
	now   func() time.Time
	after func(d time.Duration) <-chan time.Time
}

// sessionEnds returns a channel that closes when the wall clock reaches until; after ctx ends it stays open. It reads
// the clock at least once in uiClockCheck.
func sessionEnds(ctx context.Context, until time.Time, c uiClock) <-chan struct{} {
	now, after := c.now, c.after
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	ended := make(chan struct{})
	go func() {
		for now().Before(until) {
			select {
			case <-ctx.Done():
				return
			case <-after(min(until.Sub(now()), uiClockCheck)):
			}
		}
		close(ended)
	}()
	return ended
}

// newUIServer returns the server of handler: a limit for the headers of a request, and none for a body or a response,
// which may be a stream. The server's own error lines go to log as warnings.
func newUIServer(handler http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: uiReadHeaderTimeout,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// serveUntil serves srv on ln until ctx ends or the clock reaches until, and then stops it: it closes the listener,
// waits up to grace for the open requests and closes what stays open. It reports whether the clock made it stop. A
// server that fails by itself is an error.
func serveUntil(ctx context.Context, srv *http.Server, ln net.Listener, until time.Time, clock uiClock,
	grace time.Duration,
) (ended bool, err error) {
	address := ln.Addr()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	endCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	select {
	case err := <-served:
		return false, fmt.Errorf("serve on %s: %w", address, err)
	case <-ctx.Done():
	case <-sessionEnds(endCtx, until, clock):
		ended = true
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if err := srv.Shutdown(stopCtx); err != nil {
		_ = srv.Close()
	}
	return ended, nil
}
