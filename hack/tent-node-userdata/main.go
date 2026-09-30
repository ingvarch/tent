// Command tent-node-userdata prints the user data that boots a machine into a node of a test cluster, to check a
// development build of tent-node on a real machine. Its NodeConfig holds no secret: a Vultr node of the role that
// downloads tent-node, and the CNI plugins when it runs a client, with the system settings and the host firewall that
// tent gives the role, and no files. The name must be the machine's host name.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// Exit codes of the tool.
const (
	exitError = 1 // the node config cannot be made or is invalid
	exitUsage = 2 // the command line is wrong
)

// The cluster and the node group of the node. Nothing reads them on the node yet.
const (
	cluster   = "tent-node-check"
	nodeGroup = "nodes"
)

// arch is the architecture of the machine: make build builds tent-node for amd64 alone.
const arch = "amd64"

// defaultCIDR is the private network of the node's cluster when -cidr is not given: the VPC of the Vultr spike.
var defaultCIDR = netip.MustParsePrefix("10.64.0.0/16")

// The environment variables that give the tent-node, as tent-node-upload prints them.
const (
	urlEnv = "TENT_NODE_URL"
	sumEnv = "TENT_NODE_SHA256"
)

// options are what the command line and the environment say.
type options struct {
	name, version, url, sum string
	role                    v1alpha1.Role
	cidr                    netip.Prefix
}

// usageError is a wrong command line: the tool exits with exitUsage.
type usageError string

func (e usageError) Error() string { return string(e) }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run runs the tool with args, writes the user data to stdout and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var nc *nodeconfig.NodeConfig
	if err == nil {
		nc, err = nodeConfig(o)
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
	var role string
	fs.StringVar(&role, "role", string(v1alpha1.RoleClient), "the node's `role`: server, client or combined")
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
	o.role = v1alpha1.Role(role)
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

Prints the user data of a node of a test cluster on Vultr, without secrets. The node downloads the tent-node at
$%s with the sha256 $%s, as tent-node-upload prints them, and the CNI plugins
when it runs a client.

Flags:
`, urlEnv, sumEnv)
	fs.PrintDefaults()
}

// nodeConfig returns the NodeConfig of the node: a Vultr node of the role that downloads the tent-node, with the
// system settings and the host firewall that tent gives the role in a cluster whose private network is the CIDR, and
// no files, so no secrets. A node that runs a client downloads the CNI plugins too, and runs Docker, as tent gives a
// node group that keeps the docker driver.
func nodeConfig(o options) (*nodeconfig.NodeConfig, error) {
	tentNode := nodeconfig.Asset{
		Name: nodeconfig.TentNodeAsset, Version: o.version, URLs: []string{o.url}, SHA256: o.sum,
	}
	nc := &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion,
		Kind:       nodeconfig.Kind,
		Cluster:    cluster,
		Provider:   v1alpha1.ProviderVultr,
		NodeGroup:  nodeGroup,
		Name:       o.name,
		Role:       o.role,
		Assets:     []nodeconfig.Asset{tentNode},
		Join:       nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
		System:     app.NodeSystem(o.role, nil),
		Firewall:   app.HostFirewall(o.cidr, o.role),
	}
	if o.role.RunsClient() {
		cni, err := cniPlugins()
		if err != nil {
			return nil, err
		}
		nc.Assets = []nodeconfig.Asset{cni, tentNode}
	}
	nc.SpecHash = nodeconfig.SpecHash(nc)
	return nc, nil
}

// cniPlugins returns the CNI plugins for arch that the embedded stable channel pins.
func cniPlugins() (nodeconfig.Asset, error) {
	ch, err := channels.Load(v1alpha1.DefaultChannel)
	if err != nil {
		return nodeconfig.Asset{}, fmt.Errorf("load the %s channel: %w", v1alpha1.DefaultChannel, err)
	}
	a, err := assets.CNI(ch, arch)
	if err != nil {
		return nodeconfig.Asset{}, fmt.Errorf("find the CNI plugins: %w", err)
	}
	return nodeconfig.Asset(a), nil
}
