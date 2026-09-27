package cli

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/spec"
)

// clusterFlags are what create cluster generates the specs from.
type clusterFlags struct {
	provider, region, machineType, workerMachineType, image, nomadVersion string
	zones, sshKeyFiles, sshAccess, apiAccess                              []string
	servers, workers                                                      int
	combined, allowSingle, dryRun                                         bool
}

func newCreateClusterCommand(opts *globalOptions) *cobra.Command {
	var f clusterFlags
	cmd := &cobra.Command{
		Use:   "cluster [NAME]",
		Short: "Generate the specs of a cluster and store them",
		Long: "Generate the Cluster and its node groups from the flags and write them to the state store. The " +
			"cluster is named by NAME or --name. It has a server group named servers and a client group named " +
			"workers, or with --combined a single group named nodes. The specs hold only what the flags set; " +
			"tent fills in the defaults. Nothing is created in the cloud.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.combined && (cmd.Flags().Changed("workers") || cmd.Flags().Changed("worker-machine-type")) {
				return errors.New("--combined makes one group of --servers nodes; it takes no --workers or " +
					"--worker-machine-type")
			}
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			objs, err := f.objects(name)
			if err != nil {
				return err
			}
			if !f.dryRun {
				return writeObjects(cmd, opts, objs, f.allowSingle, (*app.Service).Create)
			}
			if err := f.check(cmd, opts, objs); err != nil {
				return err
			}
			return printSpecs(cmd.OutOrStdout(), opts.output, objs)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.provider, "provider", "", "cloud `PROVIDER`: vultr or hetzner; the spec gets its empty block, "+
		"vultr: {} or hetzner: {} (required)")
	fs.StringVar(&f.region, "region", "", "`REGION`: a Vultr region, such as ams, or a Hetzner network zone, such as "+
		"eu-central (required)")
	fs.StringSliceVar(&f.zones, "zones", nil, "Hetzner `LOCATIONS` to spread the nodes over, such as fsn1,nbg1,hel1")
	fs.StringVar(&f.machineType, "machine-type", "", "machine `TYPE` of every node, such as vc2-2c-4gb or cx23 "+
		"(required)")
	fs.StringVar(&f.workerMachineType, "worker-machine-type", "", "machine `TYPE` of the workers, if not "+
		"--machine-type")
	fs.IntVar(&f.servers, "servers", 3, "number of servers: 1, 3 or 5")
	fs.IntVar(&f.workers, "workers", 3, "number of workers")
	fs.BoolVar(&f.combined, "combined", false, "run servers and clients on the same nodes, in one group named nodes")
	fs.StringVar(&f.image, "image", "", "operating system `IMAGE` of every node (default "+v1alpha1.DefaultImage+")")
	fs.StringArrayVar(&f.sshKeyFiles, "ssh-key", nil, "public SSH key file at `PATH` to install on every node; "+
		"repeat for more keys")
	fs.StringSliceVar(&f.sshAccess, "ssh-access", nil, "`CIDR` that may reach SSH; repeat or separate with commas "+
		"(default none)")
	fs.StringSliceVar(&f.apiAccess, "api-access", nil, "`CIDR` that may reach the Nomad API; repeat or separate "+
		"with commas (default "+v1alpha1.DefaultAPISource+")")
	fs.StringVar(&f.nomadVersion, "nomad-version", "", "Nomad `VERSION`, such as 2.0.7 (default: the channel's "+
		"recommended one)")
	addAllowSingleServer(cmd, &f.allowSingle)
	fs.BoolVar(&f.dryRun, "dry-run", false, "check the specs, also against the state store when there is one, "+
		"and print them; write nothing")
	for _, name := range []string{"provider", "region", "machine-type"} {
		_ = cmd.MarkFlagRequired(name) // fails only for a flag that does not exist
	}
	return cmd
}

// check checks the specs for --dry-run: against the state store when one is set, else on their own.
func (f *clusterFlags) check(cmd *cobra.Command, opts *globalOptions, objs spec.Objects) error {
	validate := v1alpha1.ValidateOptions{AllowSingleServer: f.allowSingle}
	if opts.state == "" {
		return app.Check(objs, validate)
	}
	svc, err := opts.service(cmd, validate)
	if err != nil {
		return err
	}
	_, err = svc.Create(cmd.Context(), objs, false)
	return err
}

// objects generates the Cluster named name and its node groups.
func (f *clusterFlags) objects(name string) (spec.Objects, error) {
	keys, err := readSSHKeys(f.sshKeyFiles)
	if err != nil {
		return spec.Objects{}, err
	}
	c := &v1alpha1.Cluster{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindCluster},
		Metadata: v1alpha1.ClusterMeta{Name: name},
		Spec: v1alpha1.ClusterSpec{
			Cloud:   v1alpha1.Cloud{Provider: v1alpha1.Provider(f.provider), Region: f.region, Zones: f.zones},
			Access:  v1alpha1.Access{SSH: f.sshAccess, API: f.apiAccess},
			SSHKeys: keys,
			Nomad:   v1alpha1.ClusterNomad{Version: f.nomadVersion},
		},
	}
	switch c.Spec.Cloud.Provider {
	case v1alpha1.ProviderVultr:
		c.Spec.Cloud.Vultr = &v1alpha1.VultrCloud{}
	case v1alpha1.ProviderHetzner:
		c.Spec.Cloud.Hetzner = &v1alpha1.HetznerCloud{}
	}
	group := func(group string, role v1alpha1.Role, machineType string, size int) *v1alpha1.NodeGroup {
		return &v1alpha1.NodeGroup{
			TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindNodeGroup},
			Metadata: v1alpha1.NodeGroupMeta{Name: group, Cluster: name},
			Spec:     v1alpha1.NodeGroupSpec{Role: role, MachineType: machineType, Image: f.image, Size: size},
		}
	}
	groups := []*v1alpha1.NodeGroup{group("nodes", v1alpha1.RoleCombined, f.machineType, f.servers)}
	if !f.combined {
		groups = []*v1alpha1.NodeGroup{
			group("servers", v1alpha1.RoleServer, f.machineType, f.servers),
			group("workers", v1alpha1.RoleClient, cmp.Or(f.workerMachineType, f.machineType), f.workers),
		}
	}
	return spec.Objects{Cluster: c, NodeGroups: groups}, nil
}

// readSSHKeys reads public key files, one key each.
func readSSHKeys(paths []string) ([]string, error) {
	var keys []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading --ssh-key: %w", err)
		}
		if bytes.Contains(data, []byte("PRIVATE KEY")) {
			return nil, fmt.Errorf("--ssh-key %s is a private key: give the public key, the .pub file", p)
		}
		n := 0
		for line := range strings.Lines(string(data)) {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		if n > 1 {
			return nil, fmt.Errorf("--ssh-key %s holds %d keys: give one key per file", p, n)
		}
		keys = append(keys, strings.TrimSpace(string(data)))
	}
	return keys, nil
}
