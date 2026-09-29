// Command tent-node-userdata prints the user data that boots a machine into a node of a test cluster, to check a
// development build of tent-node on a real machine. Its NodeConfig holds no secret: a Vultr client whose only asset is
// tent-node, with a client's system settings and no files. The name must be the machine's host name.
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
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// Exit codes of the tool.
const (
	exitError = 1 // the node config is invalid
	exitUsage = 2 // the command line is wrong
)

// The cluster and the node group of the node. Nothing reads them on the node yet.
const (
	cluster   = "tent-node-check"
	nodeGroup = "clients"
)

// The environment variables that give the tent-node, as tent-node-upload prints them.
const (
	urlEnv = "TENT_NODE_URL"
	sumEnv = "TENT_NODE_SHA256"
)

// metadataAddr is the address of Vultr's metadata service.
var metadataAddr = netip.AddrFrom4([4]byte{169, 254, 169, 254})

// options are what the command line and the environment say.
type options struct {
	name, version, url, sum string
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
	var data []byte
	if err == nil {
		data, err = nodeconfig.UserData(nodeConfig(o))
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

Prints the user data of a node of a test cluster on Vultr: a client without secrets whose only asset is the tent-node
at $%s with the sha256 $%s, as tent-node-upload prints them.

Flags:
`, urlEnv, sumEnv)
	fs.PrintDefaults()
}

// nodeConfig returns the NodeConfig of the node: a Vultr client whose only asset is the tent-node, with the system
// settings that tent gives a client that runs Docker, and no files, so no secrets.
func nodeConfig(o options) *nodeconfig.NodeConfig {
	nc := &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion,
		Kind:       nodeconfig.Kind,
		Cluster:    cluster,
		Provider:   v1alpha1.ProviderVultr,
		NodeGroup:  nodeGroup,
		Name:       o.name,
		Role:       v1alpha1.RoleClient,
		Assets: []nodeconfig.Asset{
			{Name: nodeconfig.TentNodeAsset, Version: o.version, URLs: []string{o.url}, SHA256: o.sum},
		},
		Join: nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
		System: nodeconfig.System{
			Sysctls: map[string]string{
				"net.bridge.bridge-nf-call-arptables": "1",
				"net.bridge.bridge-nf-call-ip6tables": "1",
				"net.bridge.bridge-nf-call-iptables":  "1",
			},
			KernelModules: []string{"br_netfilter", "overlay"},
			Docker:        true,
		},
		Firewall: nodeconfig.HostFirewall{BlockMetadata: metadataAddr},
	}
	nc.SpecHash = nodeconfig.SpecHash(nc)
	return nc
}
