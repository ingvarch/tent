package app_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secret"
)

// withAssets gives svc the release files that nodes download, from assetstest, and the development build's tent-node.
// It returns the sites, which record the URLs asked for.
func withAssets(svc *app.Service) *assetstest.Sites {
	sites := assetstest.New()
	svc.Assets = assets.Options{
		Client:    &http.Client{Transport: sites},
		DevURL:    assetstest.DevURL,
		DevSHA256: assetstest.DevSHA256,
		Now:       assetstest.Now,
	}
	return sites
}

// nomadHook wraps every call that a Nomad client of the world makes, as vultrfake.Hook wraps the calls of Vultr. It
// gets the context, the call as the Nomad fake would log it and next, which carries the call out as the world does
// without a hook.
type nomadHook func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error

// nomadCall is a call that reached the Nomad fake, with how many calls had reached the Vultr fake before it.
type nomadCall struct {
	nomadfake.Call
	Cloud int
}

// nomadWorld is the Nomad cluster of one cluster of the test service, which follows a Vultr fake: before each call it
// sets the fake's leader, health and nodes from the cluster's instances there. The cluster has a leader once as many
// servers are ready as its specs give, its servers are healthy when every listed one is ready, and each ready client
// or combined instance has registered, unless it is withheld. A cluster whose servers are all gone is a new,
// unbootstrapped Nomad when new ones come. The fake's own methods, such as Fail and LoseResponse, are the world's.
type nomadWorld struct {
	*nomadfake.Fake
	cloud *vultrfake.Fake
	svc   *app.Service
	name  string // the cluster

	mu       sync.Mutex
	hook     nomadHook
	log      []nomadCall
	configs  []nomadops.Config
	withheld map[string]bool
	noLeader bool
	// seen holds the ids of the server machines at the last call.
	seen []string
	// unhealthy keeps the servers from being healthy, whatever the instances show.
	unhealthy bool
}

// withNomad gives svc the Nomad of the test cluster prod, which follows f, and returns it.
func withNomad(svc *app.Service, f *vultrfake.Fake) *nomadWorld { return withNomadOf(svc, f, "prod") }

// withNomadOf gives svc the Nomad of the cluster called name, which follows f, and returns it.
func withNomadOf(svc *app.Service, f *vultrfake.Fake, name string) *nomadWorld {
	w := &nomadWorld{Fake: nomadfake.New(), cloud: f, svc: svc, name: name, withheld: map[string]bool{}}
	svc.Nomad = func(cfg nomadops.Config) (nomadops.API, error) {
		w.mu.Lock()
		w.configs = append(w.configs, cfg)
		w.mu.Unlock()
		return &worldClient{w: w, inner: w.Client(cfg), server: cfg.Address}, nil
	}
	return w
}

// SetHook makes every later call go through hook; nil removes it.
func (w *nomadWorld) SetHook(hook nomadHook) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hook = hook
}

// Withhold keeps the nodes called names from registering.
func (w *nomadWorld) Withhold(names ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range names {
		w.withheld[n] = true
	}
}

// NoLeader keeps the cluster from electing a leader.
func (w *nomadWorld) NoLeader() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.noLeader = true
}

// Unhealthy keeps the servers of the cluster from being healthy.
func (w *nomadWorld) Unhealthy() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.unhealthy = true
}

// Configs returns the configurations that the service made Nomad clients with, in order.
func (w *nomadWorld) Configs() []nomadops.Config {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.configs)
}

// Log returns the calls that reached the Nomad fake, in order, with the count of Vultr calls before each.
func (w *nomadWorld) Log() []nomadCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.log)
}

// line returns the call as "nomad <method> <argument> (<the hostname of the server it went to>)", without the
// argument's space when it has none.
func (w *nomadWorld) line(c nomadfake.Call) string {
	return strings.TrimSpace("nomad "+c.Name+" "+c.Arg) + " (" + w.nodeName(c.Server) + ")"
}

// nodeName returns the hostname of the instance whose public address is the host of addr, or addr when none is.
func (w *nomadWorld) nodeName(addr string) string {
	host, _, _ := strings.Cut(addr, ":")
	for _, m := range w.machines() {
		if m.address == host {
			return m.name
		}
	}
	return addr
}

// machine is an instance of the Vultr fake as the world sees it.
type machine struct {
	id, name, address, role string
	ready                   bool // Vultr shows it active, running and ok
}

// machines returns the instances of the world's cluster, in creation order.
func (w *nomadWorld) machines() []machine {
	var out []machine
	for _, in := range w.cloud.Instances() {
		if tagOf(in.Tags, cloud.LabelCluster) != w.name {
			continue
		}
		out = append(out, machine{
			id: in.ID, name: in.Hostname, address: in.MainIP, role: tagOf(in.Tags, cloud.LabelRole),
			ready: in.Status == "active" && in.PowerStatus == "running" && in.ServerStatus == "ok",
		})
	}
	return out
}

// serverCount returns how many servers the specs of the cluster give.
func (w *nomadWorld) serverCount() int {
	objs, err := w.svc.Get(context.Background(), w.name, true)
	if err != nil {
		return 0
	}
	n := 0
	for _, g := range objs.NodeGroups {
		if g.Spec.Role.RunsServer() {
			n += g.Spec.Size
		}
	}
	return n
}

// follow sets the fake to what the instances of the Vultr fake show now.
func (w *nomadWorld) follow() {
	var servers, readyServers, clients []machine
	for _, m := range w.machines() {
		switch m.role {
		case "server":
			servers = append(servers, m)
		case "combined":
			servers = append(servers, m)
			clients = append(clients, m)
		case "client":
			clients = append(clients, m)
		}
	}
	for _, m := range servers {
		if m.ready {
			readyServers = append(readyServers, m)
		}
	}
	if !w.keepsAServer(servers) {
		w.NewCluster() // a cluster whose servers are all gone is lost with them
	}
	w.mu.Lock()
	noLeader, unhealthy := w.noLeader, w.unhealthy
	w.mu.Unlock()
	leader := ""
	if n := w.serverCount(); !noLeader && n > 0 && len(readyServers) >= n {
		leader = readyServers[0].address + ":4647"
	}
	w.SetLeader(leader)
	w.SetHealth(nomadops.Health{
		Healthy: !unhealthy && len(servers) > 0 && len(readyServers) == len(servers), Voters: len(readyServers),
	})
	if leader == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range clients {
		if m.ready && !w.withheld[m.name] {
			w.Register(nomadops.Node{Name: m.name, Status: "ready", Eligible: true})
		}
	}
}

// keepsAServer records the ids of servers and reports whether the machines of the last call that were servers are not
// all gone: it is true when there were none.
func (w *nomadWorld) keepsAServer(servers []machine) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev := w.seen
	w.seen = nil
	for _, m := range servers {
		w.seen = append(w.seen, m.id)
	}
	return len(prev) == 0 || slices.ContainsFunc(w.seen, func(id string) bool { return slices.Contains(prev, id) })
}

// worldClient is the nomadops.API of one client of the world.
type worldClient struct {
	w      *nomadWorld
	inner  nomadops.API
	server string // the address of the server it calls
}

// do carries out the call through the world's hook, and logs what reached the Nomad fake.
func (c *worldClient) do(ctx context.Context, call nomadfake.Call, run func(context.Context) error) error {
	call.Server = c.server
	c.w.follow()
	c.w.mu.Lock()
	hook := c.w.hook
	c.w.mu.Unlock()
	next := func(ctx context.Context) error {
		before := len(c.w.Calls())
		cloudCalls := len(c.w.cloud.Calls())
		err := run(ctx)
		c.w.mu.Lock()
		defer c.w.mu.Unlock()
		for _, call := range c.w.Calls()[before:] {
			c.w.log = append(c.w.log, nomadCall{Call: call, Cloud: cloudCalls})
		}
		return err
	}
	if hook == nil {
		return next(ctx)
	}
	return hook(ctx, call, next)
}

func (c *worldClient) Leader(ctx context.Context) (v string, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Leader"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Leader(ctx)
		return
	})
	if err != nil {
		return "", err
	}
	return v, nil
}

func (c *worldClient) Bootstrap(ctx context.Context, s secret.Secret) error {
	return c.do(ctx, nomadfake.Call{Name: "Bootstrap", Arg: s.String()}, func(ctx context.Context) error {
		return c.inner.Bootstrap(ctx, s)
	})
}

func (c *worldClient) IntroToken(ctx context.Context, req nomadops.IntroRequest) (v secret.Secret, err error) {
	arg := req.NodeName + " " + req.NodePool + " " + req.TTL.String()
	err = c.do(ctx, nomadfake.Call{Name: "IntroToken", Arg: arg}, func(ctx context.Context) (err error) {
		v, err = c.inner.IntroToken(ctx, req)
		return
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (c *worldClient) Nodes(ctx context.Context) (v []nomadops.Node, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Nodes"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Nodes(ctx)
		return
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (c *worldClient) Health(ctx context.Context) (v nomadops.Health, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Health"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Health(ctx)
		return
	})
	if err != nil {
		return nomadops.Health{}, err
	}
	return v, nil
}
