package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/spec"
)

func newGetCommand(opts *globalOptions) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "get [NAME]",
		Short: "Print the specs of a cluster",
		Long: "Print the Cluster named by NAME or --name and its node groups as YAML documents, the file that " +
			"create -f and replace -f take, or with -o json as a list. get clusters and get nodegroups list them. " +
			"For a cluster named cluster, clusters, nodegroup or nodegroups, use --name.",
		Args:                       nameOrSubcommand,
		SuggestionsMinimumDistance: 2, // cobra's default, which it sets on the root only
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{})
			if err != nil {
				return err
			}
			objs, err := svc.Get(cmd.Context(), name, full)
			if err != nil {
				return err
			}
			return printSpecs(cmd.OutOrStdout(), opts.output, objs)
		},
	}
	addFull(cmd, &full)
	cmd.AddCommand(newGetClustersCommand(opts), newGetNodeGroupsCommand(opts))
	return cmd
}

func addFull(cmd *cobra.Command, full *bool) {
	cmd.Flags().BoolVar(full, "full", false, "fill in the defaults")
}

func newGetClustersCommand(opts *globalOptions) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:     "clusters [NAME...]",
		Aliases: []string{"cluster"},
		Short:   "List the clusters in the state store",
		Long: "List the clusters in the state store, or the named ones. -o yaml and -o json print their Cluster " +
			"specs.",
		RunE: func(cmd *cobra.Command, names []string) error {
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{})
			if err != nil {
				return err
			}
			clusters, err := namedClusters(cmd.Context(), svc, names, full)
			if err != nil {
				return err
			}
			if len(clusters) == 0 {
				// A mistyped --state looks like an empty store. A notice that fails to print changes nothing.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "no clusters in %s\n", svc.Store)
			}
			return printClusters(cmd.OutOrStdout(), opts.output, clusters)
		},
	}
	addFull(cmd, &full)
	return cmd
}

// namedClusters returns the clusters called names, or every cluster when there are no names.
func namedClusters(ctx context.Context, svc *app.Service, names []string, full bool) ([]spec.Objects, error) {
	if len(names) == 0 {
		return svc.Clusters(ctx, full)
	}
	clusters := make([]spec.Objects, len(names))
	for i, name := range names {
		var err error
		if clusters[i], err = svc.Get(ctx, name, full); err != nil {
			return nil, err
		}
	}
	return clusters, nil
}

// printClusters writes a table of clusters, or their Cluster specs.
func printClusters(w io.Writer, format string, clusters []spec.Objects) error {
	switch format {
	case outputTable:
		rows := make([][]string, len(clusters))
		for i, c := range clusters {
			rows[i] = clusterRow(c)
		}
		return writeTable(w, []string{"NAME", "PROVIDER", "REGION", "NOMAD", "SERVERS", "WORKERS"}, rows)
	case outputJSON:
		list := make([]*v1alpha1.Cluster, len(clusters))
		for i, c := range clusters {
			list[i] = c.Cluster
		}
		return printObject(w, format, list, nil)
	}
	for i, c := range clusters {
		if i > 0 {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return fmt.Errorf("writing YAML: %w", err)
			}
		}
		if err := printSpecs(w, format, spec.Objects{Cluster: c.Cluster}); err != nil {
			return err
		}
	}
	return nil
}

// clusterRow is a cluster's row: SERVERS is the size of the server or combined group, WORKERS the sum of the client
// groups.
func clusterRow(c spec.Objects) []string {
	var servers, workers int
	for _, g := range c.NodeGroups {
		switch g.Spec.Role {
		case v1alpha1.RoleServer, v1alpha1.RoleCombined:
			servers += g.Spec.Size
		case v1alpha1.RoleClient:
			workers += g.Spec.Size
		}
	}
	s := c.Cluster.Spec
	return []string{
		c.Cluster.Metadata.Name, string(s.Cloud.Provider), s.Cloud.Region, orDash(s.Nomad.Version),
		strconv.Itoa(servers), strconv.Itoa(workers),
	}
}

func newGetNodeGroupsCommand(opts *globalOptions) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:     "nodegroups [NAME...]",
		Aliases: []string{"nodegroup"},
		Short:   "List the node groups of a cluster",
		Long: "List the node groups of the cluster named by --name, or the named ones. -o yaml and -o json print " +
			"their specs.",
		RunE: func(cmd *cobra.Command, names []string) error {
			cluster, err := opts.requireCluster()
			if err != nil {
				return err
			}
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{})
			if err != nil {
				return err
			}
			groups, err := svc.NodeGroups(cmd.Context(), cluster, names, full)
			if err != nil {
				return err
			}
			if opts.output != outputTable {
				return printSpecs(cmd.OutOrStdout(), opts.output, spec.Objects{NodeGroups: groups})
			}
			rows := make([][]string, len(groups))
			for i, g := range groups {
				rows[i] = []string{
					g.Metadata.Name, string(g.Spec.Role), g.Spec.MachineType, strconv.Itoa(g.Spec.Size),
					orDash(strings.Join(g.Spec.Zones, ",")),
				}
			}
			return writeTable(cmd.OutOrStdout(), []string{"NAME", "ROLE", "MACHINE-TYPE", "SIZE", "ZONES"}, rows)
		},
	}
	addFull(cmd, &full)
	return cmd
}

// writeTable writes a header and rows in aligned columns.
func writeTable(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, row := range append([][]string{header}, rows...) {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return fmt.Errorf("writing the table: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing the table: %w", err)
	}
	return nil
}

// orDash returns s, or "-" for an empty cell.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
