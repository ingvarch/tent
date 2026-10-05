// Command tent-node-userdata prints the user data that boots a machine into the only node of a test cluster, to check
// a development build of tent-node on a real machine. The node is combined, a Nomad server and client in one agent,
// and gets the NodeConfig that tent gives such a node on Vultr: Nomad, the CNI plugins and the tent-node under test,
// the Nomad agent configuration, the system settings and the host firewall. Each run makes a new CA, the node's
// certificate and a gossip key; the CA's key never leaves the tool, but the user data carries the node's key and the
// gossip key, so the machine must be deleted after the check. The name must be the machine's host name.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/buildinfo"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/spec"
)

// Exit codes of the tool.
const (
	exitError = 1 // the node config cannot be made or is invalid
	exitUsage = 2 // the command line is wrong
)

// The cluster and the node group of the node.
const (
	cluster   = "tent-node-check"
	nodeGroup = "nodes"
	// machineType is the spike's plan. The spec needs one; NodeConfig does not carry it.
	machineType = "vc2-1c-1gb"
)

// arch is the architecture of the machine: make build builds tent-node for amd64 alone.
const arch = "amd64"

// defaultCIDR is the private network of the node's cluster when -cidr is not given: the VPC of the Vultr spike.
var defaultCIDR = netip.MustParsePrefix("10.64.0.0/16")

// defaultZone is the machine's zone when -zone is not given: the Vultr spike's region.
const defaultZone = "ams"

// The environment variables that give the tent-node, as tent-node-upload prints them.
const (
	urlEnv = "TENT_NODE_URL"
	sumEnv = "TENT_NODE_SHA256"
)

// options are what the command line and the environment say.
type options struct {
	name, version, url, sum, zone string
	cidr                          netip.Prefix
}

// usageError is a wrong command line: the tool exits with exitUsage.
type usageError string

func (e usageError) Error() string { return string(e) }

// main runs the tool. Ctrl-C and SIGTERM stop the reads of Nomad's release files.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, http.DefaultClient, time.Now())
	stop()
	os.Exit(code)
}

// run runs the tool with args at now, reads Nomad's release files with client, writes the user data to stdout and
// returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, client *http.Client, now time.Time) int {
	o, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var nc *nodeconfig.NodeConfig
	if err == nil {
		nc, err = nodeConfig(ctx, o, client, now)
	}
	var data []byte
	if err == nil {
		data, err = nodeconfig.UserData(nc)
	}
	if err == nil {
		_, err = stdout.Write(data)
	}
	if err == nil {
		return 0
	}
	// Nowhere to report a failed write to stderr; the exit code still says the tool failed.
	_, _ = fmt.Fprintf(stderr, "Error: %s\n", err)
	if _, ok := errors.AsType[usageError](err); ok {
		return exitUsage
	}
	return exitError
}

// parseArgs reads the flags, and the URL and the sha256 from the environment unless flags give them. It returns
// flag.ErrHelp once it has written the usage for -h.
func parseArgs(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("tent-node-userdata", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.name, "name", "", "the node's `name`, which must be the machine's host name")
	fs.StringVar(&o.version, "version", "", "the `version` of the tent-node binary, as tent built with it prints "+
		"it: bin/tent version -o json")
	// The defaults stay empty: the usage would print the URL, which carries a signature.
	fs.StringVar(&o.url, "url", "", "the `URL` of the tent-node binary (default $"+urlEnv+")")
	fs.StringVar(&o.sum, "sha256", "", "the `sha256` of the tent-node binary (default $"+sumEnv+")")
	fs.StringVar(&o.zone, "zone", defaultZone, "the machine's `zone`, its Vultr region, which is Nomad's datacenter")
	fs.TextVar(&o.cidr, "cidr", defaultCIDR, "the private network of the node's cluster, the `CIDR` of the VPC")
	// run writes the errors of the flag package once, as it writes every error.
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(stderr)
		writeUsage(fs)
		return options{}, err
	case err != nil:
		return options{}, usageError(err.Error() + "; run tent-node-userdata -h for the flags")
	case fs.NArg() > 0:
		return options{}, usageError("tent-node-userdata takes no arguments, only flags")
	}
	if o.url == "" {
		o.url = os.Getenv(urlEnv)
	}
	if o.sum == "" {
		o.sum = os.Getenv(sumEnv)
	}
	switch {
	case o.name == "":
		return options{}, usageError("-name is required: the machine's host name")
	case o.version == "":
		return options{}, usageError("-version is required: the version of the tent-node binary")
	// tent gives the nodes of a release the release's tent-node, not the one under test.
	case buildinfo.IsRelease(o.version):
		return options{}, usageError("-version " + o.version + " is a release, whose nodes download the " +
			"release's tent-node: check a development build")
	case o.url == "":
		return options{}, usageError(urlEnv + " is not set and -url is not given")
	case o.sum == "":
		return options{}, usageError(sumEnv + " is not set and -sha256 is not given")
	}
	return o, nil
}

// writeUsage writes how to run the tool.
func writeUsage(fs *flag.FlagSet) {
	_, _ = fmt.Fprintf(fs.Output(), `Usage: tent-node-userdata -name NAME -version VERSION [flags]

Prints the user data of the only node of a test cluster on Vultr: a combined node with a new CA, certificate and
gossip key, which the user data carries, so delete the machine after the check. The node downloads Nomad and the
CNI plugins of the stable channel, and the tent-node at $%s with the sha256 $%s,
as tent-node-upload prints them.

Flags:
`, urlEnv, sumEnv)
	fs.PrintDefaults()
}

// nodeConfig returns the NodeConfig that tent gives the node that o describes, with a new CA, certificate and gossip
// key made at now. It reads Nomad's signed release files with client.
func nodeConfig(ctx context.Context, o options, client *http.Client, now time.Time) (*nodeconfig.NodeConfig, error) {
	ch, err := channels.Load(v1alpha1.DefaultChannel)
	if err != nil {
		return nil, fmt.Errorf("load the %s channel: %w", v1alpha1.DefaultChannel, err)
	}
	objs, err := specs(o, ch)
	if err != nil {
		return nil, err
	}
	ca, err := pki.NewCA(cluster, now)
	if err != nil {
		return nil, err
	}
	cert, err := ca.IssueNode(v1alpha1.RoleCombined, objs.Cluster.Spec.Nomad.Region, now)
	if err != nil {
		return nil, err
	}
	return app.NodeConfigOf(ctx, app.NewNode{
		Specs:   objs,
		Channel: ch,
		Assets: assets.Options{
			Client: client, DevURL: o.url, DevSHA256: o.sum, Now: func() time.Time { return now },
		},
		TentVersion: o.version,
		Arch:        arch,
		Gossip:      pki.NewGossipKey(),
		CABundle:    ca.Bundle(),
		// The cluster's only server: it bootstraps alone and joins no seed.
		Group: nodeGroup, Name: o.name, Zone: o.zone, BootstrapExpect: 1, Cert: cert,
	})
}

// specs returns the specs of the node's cluster on Vultr in the zone, whose private network is the CIDR, with their
// defaults and the Nomad version that ch recommends, as tent pins it for a new cluster: one combined group of one
// node. A combined group makes the servers take clients without intro tokens, so the node needs none.
func specs(o options, ch *channels.Channel) (spec.Objects, error) {
	c := &v1alpha1.Cluster{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindCluster},
		Metadata: v1alpha1.ClusterMeta{Name: cluster},
		Spec: v1alpha1.ClusterSpec{
			Cloud:      v1alpha1.Cloud{Provider: v1alpha1.ProviderVultr, Region: o.zone},
			Networking: v1alpha1.Networking{CIDR: o.cidr.String()},
			Nomad:      v1alpha1.ClusterNomad{Version: ch.Nomad.Recommended},
		},
	}
	groups := []*v1alpha1.NodeGroup{{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindNodeGroup},
		Metadata: v1alpha1.NodeGroupMeta{Name: nodeGroup, Cluster: cluster},
		Spec:     v1alpha1.NodeGroupSpec{Role: v1alpha1.RoleCombined, MachineType: machineType, Size: 1},
	}}
	v1alpha1.SetDefaults(c, groups)
	if err := v1alpha1.Validate(c, groups, v1alpha1.ValidateOptions{AllowSingleServer: true}); err != nil {
		return spec.Objects{}, err
	}
	return spec.Objects{Cluster: c, NodeGroups: groups}, nil
}
